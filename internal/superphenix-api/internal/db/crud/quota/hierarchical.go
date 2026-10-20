package quota

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/super-phenix/superphenix/internal/superphenix-api/internal/az"
	"github.com/super-phenix/superphenix/internal/superphenix-api/internal/db"
	"github.com/super-phenix/superphenix/internal/superphenix-api/internal/db/model"
	"github.com/super-phenix/superphenix/internal/superphenix-api/pkg/config"
	spxQuota "github.com/super-phenix/superphenix/internal/superphenix-api/pkg/quota"
	logger "github.com/super-phenix/superphenix/pkg/utils/log"
)

// Business errors of the hierarchical quotas. Every one of them is caused by the request
// (and is safe to return to the caller); any other error is an internal error.
var (
	ErrInvalidQuantity      = spxQuota.ErrInvalidResources
	ErrOrgQuotaExceeded     = errors.New("organization quota exceeded")
	ErrProjectQuotaExceeded = errors.New("project quota exceeded")
	ErrOrganizationNotFound = errors.New("organization not found")
	ErrProjectNotFound      = errors.New("project not found")
	ErrAZNotFound           = errors.New("availability zone not found")
	ErrProjectQuotaEnforced = errors.New("the project quota cannot be removed while the organization has a quota")
	ErrBelowAllocated       = errors.New("quota below what is already allocated")
	ErrBelowUsage           = errors.New("quota below what is currently used")
	ErrUsageUnavailable     = errors.New("quota usage unavailable, retry later")
	ErrAZUnavailable        = errors.New("availability zone unavailable, retry later")
)

// AZUsage is the quota of a project as enforced by the cluster of one AZ (Hard, the ResourceQuota
// `spec.hard`) and what the project uses of it (Used, its `status.used`), as abstract quantities.
type AZUsage struct {
	Hard spxQuota.Quantities
	Used spxQuota.Quantities
}

// ReadUsage reads the AZUsage of a project in one AZ from the controller of the AZ. It is set
// by the quotasync package; when nil, lowering an AZ quota fails.
var ReadUsage func(ctx context.Context, orgId, projectId uuid.UUID, codeAZ string) (AZUsage, error)

// PushAZ sends the Kubernetes quota (`spec.hard`) of a project to the controller of one AZ. The
// controller refuses to lower a limit below its usage, which PushAZ reports as ErrBelowUsage. It
// is set by the quotasync package; when nil, lowering an AZ quota fails.
var PushAZ func(ctx context.Context, orgId, projectId uuid.UUID, codeAZ string, hard map[string]string) error

// OrganizationQuotaOverview is the quota of an organization and its distribution to projects.
type OrganizationQuotaOverview struct {
	OrganizationId string               `json:"organizationId"`
	Unlimited      bool                 `json:"unlimited"`
	Resources      model.QuotaResources `json:"resources,omitempty"`
	Allocated      model.QuotaResources `json:"allocated"`
	Remaining      model.QuotaResources `json:"remaining,omitempty"`
	ProjectQuotas  []model.ProjectQuota `json:"projectQuotas"`
}

// ProjectQuotaOverview is the quota of a project and its distribution to AZs.
type ProjectQuotaOverview struct {
	ProjectId      string                 `json:"projectId"`
	OrganizationId string                 `json:"organizationId"`
	Resources      model.QuotaResources   `json:"resources,omitempty"`
	AZQuotas       []model.ProjectAZQuota `json:"azQuotas"`
}

// GetOrganizationQuotaOverview returns the organization quota (unlimited when not defined),
// the sum allocated to its projects (see projectAllocations) and what remains.
func GetOrganizationQuotaOverview(ctx context.Context, orgId uuid.UUID) (OrganizationQuotaOverview, error) {
	tx := db.Client.WithContext(ctx)
	overview := OrganizationQuotaOverview{OrganizationId: orgId.String(), Unlimited: true}

	oq, err := findOrganizationQuota(tx, orgId)
	if err != nil {
		return overview, err
	}
	pqs, err := listProjectQuotas(tx, orgId)
	if err != nil {
		return overview, err
	}
	allocations, err := projectAllocations(tx, orgId, pqs)
	if err != nil {
		return overview, err
	}
	allocated := sum(allocations)

	overview.ProjectQuotas = pqs
	overview.Allocated = allocated.Resources()
	if oq != nil {
		parsedOrg, err := oq.Resources.Parse()
		if err != nil {
			return overview, err
		}
		overview.Unlimited = false
		overview.Resources = oq.Resources
		overview.Remaining = parsedOrg.Sub(allocated).Resources()
	}
	return overview, nil
}

// GetProjectQuotaOverview returns the quota of a project of the organization and its AZ quotas.
func GetProjectQuotaOverview(ctx context.Context, orgId, projectId uuid.UUID) (ProjectQuotaOverview, error) {
	tx := db.Client.WithContext(ctx)
	overview := ProjectQuotaOverview{ProjectId: projectId.String(), OrganizationId: orgId.String()}

	if err := checkProject(tx, orgId, projectId); err != nil {
		return overview, err
	}
	pq, err := findProjectQuota(tx, projectId)
	if err != nil {
		return overview, err
	}
	if pq != nil {
		overview.Resources = pq.Resources
	}
	if err := tx.Where("project_id = ?", projectId).Order("code_az").Find(&overview.AZQuotas).Error; err != nil {
		return overview, err
	}
	return overview, nil
}

// Hierarchy holds the quotas applying to a project. A nil level has no quota.
// Kubernetes only enforces what the AZ controllers are sent, see the quotasync package.
type Hierarchy struct {
	Organization model.QuotaResources
	Project      model.QuotaResources
	AZs          map[string]model.QuotaResources
}

// AZ returns the abstract quota to enforce in the given AZ of the project.
func (h Hierarchy) AZ(codeAZ string) model.QuotaResources {
	return spxQuota.EffectiveAZQuota(h.Organization, h.Project, h.AZs[codeAZ])
}

// GetHierarchy returns the organization, project and AZ quotas of a project.
func GetHierarchy(ctx context.Context, orgId, projectId uuid.UUID) (Hierarchy, error) {
	tx := db.Client.WithContext(ctx)
	h := Hierarchy{AZs: map[string]model.QuotaResources{}}

	oq, err := findOrganizationQuota(tx, orgId)
	if err != nil {
		return h, err
	}
	if oq != nil {
		h.Organization = oq.Resources
	}
	pq, err := findProjectQuota(tx, projectId)
	if err != nil {
		return h, err
	}
	if pq != nil {
		h.Project = pq.Resources
	}
	var azqs []model.ProjectAZQuota
	if err := tx.Where("project_id = ?", projectId).Find(&azqs).Error; err != nil {
		return h, err
	}
	for _, azq := range azqs {
		h.AZs[azq.CodeAZ] = azq.Resources
	}
	return h, nil
}

// ListProjectIds returns the IDs of the live projects of the organization.
func ListProjectIds(ctx context.Context, orgId uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := db.Client.WithContext(ctx).Model(&model.Project{}).Where("orga_id = ?", orgId).Order("id").Pluck("id", &ids).Error
	return ids, err
}

// ListAllProjects lists the live projects of every organization (only their ID and
// organization ID), ordered by organization then project.
func ListAllProjects(ctx context.Context) ([]model.Project, error) {
	var projects []model.Project
	err := db.Client.WithContext(ctx).Select("id", "orga_id").Order("orga_id").Order("id").Find(&projects).Error
	return projects, err
}

// WithOrganizationLock runs fn while holding the row lock the quota writes of the organization
// take (see lockOrganization), so that fn sees no quota write in progress and none starts until
// it returns. fn reads the committed quotas through db.Client.
func WithOrganizationLock(ctx context.Context, orgId uuid.UUID, fn func() error) error {
	return db.Client.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockOrganization(tx, orgId); err != nil {
			return err
		}
		return fn()
	})
}

// CheckProject ensures the project exists and belongs to the organization (ErrProjectNotFound otherwise).
func CheckProject(ctx context.Context, orgId, projectId uuid.UUID) error {
	return checkProject(db.Client.WithContext(ctx), orgId, projectId)
}

// GetProjectQuota retrieves a project's quota, or nil if not defined.
func GetProjectQuota(ctx context.Context, projectId uuid.UUID) (*model.ProjectQuota, error) {
	return findProjectQuota(db.Client.WithContext(ctx), projectId)
}

// SetOrganizationQuota creates or updates an organization quota (platform administrators only).
//
// The organization quota cannot be lower than what its projects are allocated (see
// projectAllocations), on any resource it limits: ErrBelowAllocated otherwise. The projects
// must be lowered first.
//
// When no project quota exists yet, the organization quota is distributed among the projects
// of the organization: each one gets what its AZ quotas already hold, plus an equal part of the
// rest (and, when it has no AZ quota, its project quota is split among the AZs).
func SetOrganizationQuota(ctx context.Context, orgId uuid.UUID, resources model.QuotaResources) (*model.OrganizationQuota, error) {
	parsedOrg, err := resources.Parse()
	if err != nil {
		return nil, err
	}

	saved := model.OrganizationQuota{OrganizationId: orgId, Resources: resources}
	err = db.Client.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockOrganization(tx, orgId); err != nil {
			return err
		}

		pqs, err := listProjectQuotas(tx, orgId)
		if err != nil {
			return err
		}
		allocations, err := projectAllocations(tx, orgId, pqs)
		if err != nil {
			return err
		}
		allocated := sum(allocations)
		if err := checkNotBelow(parsedOrg, allocated, "its projects"); err != nil {
			return err
		}

		if len(pqs) == 0 {
			var projects []model.Project
			if err := tx.Where("orga_id = ?", orgId).Order("id").Find(&projects).Error; err != nil {
				return err
			}
			share := parsedOrg.Sub(allocated).SplitEqually(len(projects))
			for _, p := range projects {
				pq := share.Add(allocations[p.ID].Only(parsedOrg))
				if _, err := saveProjectQuota(tx, orgId, p.ID, pq.Resources(), pq); err != nil {
					return err
				}
			}
		}

		return upsert(tx, &saved, []string{"organization_id"}, "resources")
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

// SetProjectQuota sets the quota of a project of the organization. When the project has no
// AZ quota yet, it is split equally among the AZs of the organization; otherwise the AZ quotas
// are kept as they are.
//
// The project quota cannot be lower than the sum of its AZ quotas, on any resource it limits
// (ErrBelowAllocated). It can be set whether or not the organization has a quota. When it has
// one, the sum of the project allocations (see projectAllocations) must fit in it (see
// checkCapacity).
func SetProjectQuota(ctx context.Context, orgId, projectId uuid.UUID, resources model.QuotaResources) (*model.ProjectQuota, error) {
	parsedProj, err := resources.Parse()
	if err != nil {
		return nil, err
	}

	var saved model.ProjectQuota
	err = db.Client.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockOrganization(tx, orgId); err != nil {
			return err
		}
		if err := checkProject(tx, orgId, projectId); err != nil {
			return err
		}

		azqs, err := listAZQuotas(tx, projectId)
		if err != nil {
			return err
		}
		azSum := spxQuota.Quantities{}
		for _, azq := range azqs {
			parsed, err := azq.Resources.Parse()
			if err != nil {
				return err
			}
			azSum = azSum.Add(parsed)
		}
		if err := checkNotBelow(parsedProj, azSum, "its availability zones"); err != nil {
			return err
		}

		oq, err := findOrganizationQuota(tx, orgId)
		if err != nil {
			return err
		}
		if oq != nil {
			parsedOrg, err := oq.Resources.Parse()
			if err != nil {
				return err
			}
			pqs, err := listProjectQuotas(tx, orgId)
			if err != nil {
				return err
			}
			allocations, err := projectAllocations(tx, orgId, pqs)
			if err != nil {
				return err
			}
			current := allocations[projectId]
			delete(allocations, projectId)
			if err := checkCapacity(parsedProj, current, sum(allocations), parsedOrg, ErrOrgQuotaExceeded); err != nil {
				return err
			}
		}

		saved, err = saveProjectQuota(tx, orgId, projectId, resources, parsedProj)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

// SetProjectAZQuota sets the quota of a project in one AZ, whether or not the project has a
// quota. The sum of the AZ quotas must fit in the project limit (see projectLimit and
// checkCapacity).
//
// A resource lowered in the AZ cannot go below what the project currently uses there, read
// from the cluster (see checkUsage): ErrBelowUsage, or ErrUsageUnavailable when it cannot be read.
// The lowered quota is then pushed to the AZ before the commit (see pushLowered), so the
// capacity is only freed in the database once the cluster enforces the lower limit.
func SetProjectAZQuota(ctx context.Context, orgId, projectId uuid.UUID, codeAZ string, resources model.QuotaResources) (*model.ProjectAZQuota, error) {
	parsedAZ, err := resources.Parse()
	if err != nil {
		return nil, err
	}
	if _, err := az.GetByCode(codeAZ, orgId.String()); err != nil {
		return nil, ErrAZNotFound
	}

	saved := model.ProjectAZQuota{ProjectId: projectId, CodeAZ: codeAZ, Resources: resources}
	err = db.Client.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockOrganization(tx, orgId); err != nil {
			return err
		}
		if err := checkProject(tx, orgId, projectId); err != nil {
			return err
		}

		pq, err := findProjectQuota(tx, projectId)
		if err != nil {
			return err
		}
		oq, err := findOrganizationQuota(tx, orgId)
		if err != nil {
			return err
		}
		azqs, err := listAZQuotas(tx, projectId)
		if err != nil {
			return err
		}
		var currentResources model.QuotaResources
		current, allocated := spxQuota.Quantities{}, spxQuota.Quantities{}
		for _, azq := range azqs {
			parsed, err := azq.Resources.Parse()
			if err != nil {
				return err
			}
			if azq.CodeAZ == codeAZ {
				current, currentResources = parsed, azq.Resources
			} else {
				allocated = allocated.Add(parsed)
			}
		}

		limit, err := projectLimit(oq, pq)
		if err != nil {
			return err
		}
		if limit != nil {
			if err := checkCapacity(parsedAZ, current, allocated, limit, ErrProjectQuotaExceeded); err != nil {
				return err
			}
		}

		var orgResources, projResources model.QuotaResources
		if oq != nil {
			orgResources = oq.Resources
		}
		if pq != nil {
			projResources = pq.Resources
		}
		lowered, err := loweredResources(
			spxQuota.EffectiveAZQuota(orgResources, projResources, currentResources),
			spxQuota.EffectiveAZQuota(orgResources, projResources, resources))
		if err != nil {
			return err
		}
		if err := checkUsage(ctx, orgId, projectId, codeAZ, lowered); err != nil {
			return err
		}

		if err := upsert(tx, &saved, []string{"project_id", "code_az"}, "resources"); err != nil {
			return err
		}
		if len(lowered) == 0 {
			return nil
		}
		return pushLowered(ctx, orgId, projectId, codeAZ, spxQuota.EffectiveAZQuota(orgResources, projResources, resources))
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

// projectLimit returns the limit the AZ quotas of a project must fit in, or nil when neither
// the project (pq) nor the organization (oq) has a quota: the project quota, plus 0 for every
// resource the organization limits but the project quota does not (the project gets nothing of
// it, like in EffectiveAZQuota, so its AZ quotas cannot escape the organization quota).
func projectLimit(oq *model.OrganizationQuota, pq *model.ProjectQuota) (spxQuota.Quantities, error) {
	if pq == nil && oq == nil {
		return nil, nil
	}

	limit := spxQuota.Quantities{}
	if oq != nil {
		for name := range oq.Resources {
			limit[name] = 0
		}
	}
	if pq != nil {
		parsed, err := pq.Resources.Parse()
		if err != nil {
			return nil, err
		}
		for name, v := range parsed {
			limit[name] = v
		}
	}
	return limit, nil
}

// DeleteOrganizationQuota removes an organization quota (platform administrators only).
// The project quotas are kept and keep being enforced.
func DeleteOrganizationQuota(ctx context.Context, orgId uuid.UUID) error {
	return db.Client.WithContext(ctx).Unscoped().Where("organization_id = ?", orgId).Delete(&model.OrganizationQuota{}).Error
}

// DeleteProjectQuota removes the quota of a project of the organization and its AZ quotas.
// It is refused while the organization has a quota, since the project would escape it.
func DeleteProjectQuota(ctx context.Context, orgId, projectId uuid.UUID) error {
	return db.Client.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockOrganization(tx, orgId); err != nil {
			return err
		}
		if err := checkProject(tx, orgId, projectId); err != nil {
			return err
		}
		oq, err := findOrganizationQuota(tx, orgId)
		if err != nil {
			return err
		}
		if oq != nil {
			return ErrProjectQuotaEnforced
		}
		if err := tx.Unscoped().Where("project_id = ?", projectId).Delete(&model.ProjectAZQuota{}).Error; err != nil {
			return err
		}
		return tx.Unscoped().Where("project_id = ?", projectId).Delete(&model.ProjectQuota{}).Error
	})
}

// checkCapacity checks that a child quota (requested, replacing current) fits in the parent
// limit, allocated being the sum of the other children. Resources the parent does not limit
// are free.
//
// No write can make the children exceed their parent: a parent is never set below its children
// (see checkNotBelow) and a child never grows past its parent. Only the resources the request
// increases are checked, which is the same as long as the children fit; it only lets a state
// stored before these rules (children above their parent) be lowered until it fits again.
func checkCapacity(requested, current, allocated, limit spxQuota.Quantities, errExceeded error) error {
	increased, checked := spxQuota.Quantities{}, spxQuota.Quantities{}
	for name, v := range requested {
		if l, limited := limit[name]; limited && v > current[name] {
			increased[name] = v
			checked[name] = l
		}
	}
	// Only the increased resources are checked: a resource already overcommitted by the other
	// children must not block a request that does not increase it.
	if msg, exceeded := allocated.Add(increased).Exceeds(checked); exceeded {
		return fmt.Errorf("%w: %s", errExceeded, msg)
	}
	return nil
}

// checkNotBelow checks that a parent quota (limit) is not lower than what its children are
// allocated, on every resource it limits. children names them in the error.
func checkNotBelow(limit, allocated spxQuota.Quantities, children string) error {
	if msg, exceeded := allocated.Exceeds(limit); exceeded {
		return fmt.Errorf("%w by %s: %s", ErrBelowAllocated, children, msg)
	}
	return nil
}

// loweredResources returns, from the quota enforced in an AZ after a change, the resources that
// were already limited before and are lowered.
func loweredResources(before, after model.QuotaResources) (spxQuota.Quantities, error) {
	parsedBefore, err := before.Parse()
	if err != nil {
		return nil, err
	}
	parsedAfter, err := after.Parse()
	if err != nil {
		return nil, err
	}
	lowered := spxQuota.Quantities{}
	for name, v := range parsedAfter {
		if old, limited := parsedBefore[name]; limited && v < old {
			lowered[name] = v
		}
	}
	return lowered, nil
}

// checkUsage checks that the lowered resources of a project in an AZ stay above what the
// project currently uses there, so that capacity still in use is never freed for another AZ or
// project. The usage is read from the cluster: when it cannot be read, or the resource is not
// enforced by the cluster (e.g. a previous push failed), the change is refused.
func checkUsage(ctx context.Context, orgId, projectId uuid.UUID, codeAZ string, lowered spxQuota.Quantities) error {
	if len(lowered) == 0 {
		return nil
	}
	if ReadUsage == nil {
		return errors.New("no quota usage reader configured")
	}
	usage, err := ReadUsage(ctx, orgId, projectId, codeAZ)
	if err != nil {
		log := logger.GetLogger(ctx)
		log.Err(err).Str("az", codeAZ).Str("projectId", projectId.String()).Msg("failed to read quota usage")
		return fmt.Errorf("%w: %s", ErrUsageUnavailable, codeAZ)
	}
	for _, name := range slices.Sorted(maps.Keys(lowered)) {
		_, enforced := usage.Hard[name]
		used, tracked := usage.Used[name]
		if !enforced || !tracked {
			return fmt.Errorf("%w: %s is not tracked in %s", ErrUsageUnavailable, name, codeAZ)
		}
		if used > lowered[name] {
			return fmt.Errorf("%w: %s %s used in %s, above the requested %s", ErrBelowUsage, name,
				spxQuota.Quantities{name: used}.Resources()[name], codeAZ,
				spxQuota.Quantities{name: lowered[name]}.Resources()[name])
		}
	}
	return nil
}

// pushLowered pushes the quota enforced in an AZ (effective) to its controller, from within the
// transaction that lowers it: if the controller refuses it (a workload admitted since the usage
// was read, ErrBelowUsage) or cannot be reached (ErrAZUnavailable), the change is rolled back, so
// the database never frees capacity the cluster still grants. If the commit fails after the push,
// the cluster is left stricter than the database until the next push (or re-sync).
func pushLowered(ctx context.Context, orgId, projectId uuid.UUID, codeAZ string, effective model.QuotaResources) error {
	if PushAZ == nil {
		return errors.New("no quota pusher configured")
	}
	err := PushAZ(ctx, orgId, projectId, codeAZ, effective.ToKubernetesQuota())
	if err == nil || errors.Is(err, ErrBelowUsage) {
		return err
	}
	log := logger.GetLogger(ctx)
	log.Err(err).Str("az", codeAZ).Str("projectId", projectId.String()).Msg("failed to push lowered quota")
	return fmt.Errorf("%w: %s", ErrAZUnavailable, codeAZ)
}

// saveProjectQuota writes a project quota (parsed being its parsed form). When the project has
// no AZ quota yet, the quota is split equally among the AZs of the organization; AZ quotas
// already defined are never overwritten.
func saveProjectQuota(tx *gorm.DB, orgId, projectId uuid.UUID, resources model.QuotaResources, parsed spxQuota.Quantities) (model.ProjectQuota, error) {
	pq := model.ProjectQuota{ProjectId: projectId, OrganizationId: orgId, Resources: resources}
	if err := upsert(tx, &pq, []string{"project_id"}, "organization_id", "resources"); err != nil {
		return pq, err
	}

	var defined int64
	if err := tx.Model(&model.ProjectAZQuota{}).Where("project_id = ?", projectId).Count(&defined).Error; err != nil {
		return pq, err
	}
	if defined > 0 {
		return pq, nil
	}

	azs := az.FindAll(orgId.String())
	slices.SortFunc(azs, func(a, b config.AZConfig) int { return strings.Compare(a.Code, b.Code) })
	share := parsed.SplitEqually(len(azs)).Resources()
	for _, azCfg := range azs {
		azq := model.ProjectAZQuota{ProjectId: projectId, CodeAZ: azCfg.Code, Resources: share}
		if err := upsert(tx, &azq, []string{"project_id", "code_az"}, "resources"); err != nil {
			return pq, err
		}
	}
	return pq, nil
}

// upsert inserts row, or updates the given columns when a row with the same conflict
// columns exists. A soft-deleted row is revived.
func upsert(tx *gorm.DB, row any, conflict []string, update ...string) error {
	columns := make([]clause.Column, len(conflict))
	for i, c := range conflict {
		columns[i] = clause.Column{Name: c}
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   columns,
		DoUpdates: clause.AssignmentColumns(append(update, "updated_at", "deleted_at")),
	}).Create(row).Error
}

// lockOrganization takes a row lock on the organization, serializing the quota writes of an
// organization so that concurrent requests cannot both pass the same capacity check.
func lockOrganization(tx *gorm.DB, orgId uuid.UUID) error {
	err := tx.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
		Select("id").Where("id = ?", orgId).Take(&model.Organization{}).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrOrganizationNotFound
	}
	return err
}

// checkProject ensures the project exists and belongs to the organization.
func checkProject(tx *gorm.DB, orgId, projectId uuid.UUID) error {
	err := tx.Select("id").Where("id = ? AND orga_id = ?", projectId, orgId).Take(&model.Project{}).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrProjectNotFound
	}
	return err
}

func findOrganizationQuota(tx *gorm.DB, orgId uuid.UUID) (*model.OrganizationQuota, error) {
	var oq model.OrganizationQuota
	return orNil(&oq, tx.Where("organization_id = ?", orgId).Take(&oq).Error)
}

func findProjectQuota(tx *gorm.DB, projectId uuid.UUID) (*model.ProjectQuota, error) {
	var pq model.ProjectQuota
	return orNil(&pq, tx.Where("project_id = ?", projectId).Take(&pq).Error)
}

func orNil[T any](row *T, err error) (*T, error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// listAZQuotas lists the AZ quotas of a project.
func listAZQuotas(tx *gorm.DB, projectId uuid.UUID) ([]model.ProjectAZQuota, error) {
	var azqs []model.ProjectAZQuota
	err := tx.Where("project_id = ?", projectId).Find(&azqs).Error
	return azqs, err
}

// listProjectQuotas lists the quotas of the live projects of the organization. Quotas of
// deleted projects are ignored, so they do not keep consuming the organization quota.
func listProjectQuotas(tx *gorm.DB, orgId uuid.UUID) ([]model.ProjectQuota, error) {
	var pqs []model.ProjectQuota
	err := tx.Where("organization_id = ? AND project_id IN (?)", orgId, liveProjectIds(tx, orgId)).
		Order("project_id").Find(&pqs).Error
	return pqs, err
}

// liveProjectIds is the subquery of the IDs of the live projects of the organization.
func liveProjectIds(tx *gorm.DB, orgId uuid.UUID) *gorm.DB {
	return tx.Session(&gorm.Session{NewDB: true}).Model(&model.Project{}).Select("id").Where("orga_id = ?", orgId)
}

// projectAllocations returns what each live project of the organization takes from the
// organization quota, pqs being their project quotas (see listProjectQuotas): per resource,
// the highest of its project quota and the sum of its AZ quotas (what is actually enforced).
// AZ quotas set while the organization had no quota therefore keep counting once it gets one,
// and lowering a project quota below its AZ quotas frees nothing for the other projects until
// the AZ quotas are lowered too.
func projectAllocations(tx *gorm.DB, orgId uuid.UUID, pqs []model.ProjectQuota) (map[uuid.UUID]spxQuota.Quantities, error) {
	allocations := make(map[uuid.UUID]spxQuota.Quantities, len(pqs))
	for _, pq := range pqs {
		parsed, err := pq.Resources.Parse()
		if err != nil {
			return nil, err
		}
		allocations[pq.ProjectId] = parsed
	}

	var azqs []model.ProjectAZQuota
	if err := tx.Where("project_id IN (?)", liveProjectIds(tx, orgId)).Order("project_id, code_az").Find(&azqs).Error; err != nil {
		return nil, err
	}
	azSums := map[uuid.UUID]spxQuota.Quantities{}
	for _, azq := range azqs {
		parsed, err := azq.Resources.Parse()
		if err != nil {
			return nil, err
		}
		azSums[azq.ProjectId] = azSums[azq.ProjectId].Add(parsed)
	}
	for projectId, q := range azSums {
		allocations[projectId] = allocations[projectId].Max(q)
	}
	return allocations, nil
}

func sum(allocations map[uuid.UUID]spxQuota.Quantities) spxQuota.Quantities {
	total := spxQuota.Quantities{}
	for _, q := range allocations {
		total = total.Add(q)
	}
	return total
}
