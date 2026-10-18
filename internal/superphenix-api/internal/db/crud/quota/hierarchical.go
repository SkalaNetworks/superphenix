package quota

import (
	"context"
	"errors"
	"fmt"
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
	ErrProjectQuotaRequired = errors.New("the project has no quota: AZ quotas can only split a project quota")
	ErrProjectQuotaEnforced = errors.New("the project quota cannot be removed while the organization has a quota")
)

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
// the sum allocated to its projects and what remains.
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
	allocated, err := sumProjectQuotas(pqs)
	if err != nil {
		return overview, err
	}

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

// GetProjectQuota retrieves a project's quota, or nil if not defined.
func GetProjectQuota(ctx context.Context, projectId uuid.UUID) (*model.ProjectQuota, error) {
	return findProjectQuota(db.Client.WithContext(ctx), projectId)
}

// SetOrganizationQuota creates or updates an organization quota (platform administrators only).
//
// When no project quota exists yet, the organization quota is split equally among the
// projects of the organization, and each project quota among the AZs of the organization.
// Otherwise the existing project quotas must fit in the new organization quota.
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

		if len(pqs) > 0 {
			allocated, err := sumProjectQuotas(pqs)
			if err != nil {
				return err
			}
			if msg, exceeded := allocated.Exceeds(parsedOrg); exceeded {
				return fmt.Errorf("%w: existing project quotas: %s", ErrOrgQuotaExceeded, msg)
			}
		} else {
			var projects []model.Project
			if err := tx.Where("orga_id = ?", orgId).Order("id").Find(&projects).Error; err != nil {
				return err
			}
			share := parsedOrg.SplitEqually(len(projects))
			for _, p := range projects {
				if _, err := saveProjectQuota(tx, orgId, p.ID, share.Resources(), share); err != nil {
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

// SetProjectQuota sets the quota of a project of the organization, and splits it equally
// among the AZs of the organization (overwriting the previous AZ quotas).
//
// When the organization has a quota, the project can only limit resources limited by the
// organization, and the sum of the project quotas must fit in the organization quota.
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
			others := slices.DeleteFunc(pqs, func(pq model.ProjectQuota) bool { return pq.ProjectId == projectId })
			allocated, err := sumProjectQuotas(others)
			if err != nil {
				return err
			}
			if err := checkWithin(parsedProj, allocated, parsedOrg, ErrOrgQuotaExceeded, "organization"); err != nil {
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

// SetProjectAZQuota sets the quota of a project in one AZ. The project must have a quota:
// the AZ can only limit resources limited by the project, and the sum of the AZ quotas
// must fit in the project quota.
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
		if pq == nil {
			return ErrProjectQuotaRequired
		}
		parsedProj, err := pq.Resources.Parse()
		if err != nil {
			return err
		}

		var others []model.ProjectAZQuota
		if err := tx.Where("project_id = ? AND code_az <> ?", projectId, codeAZ).Find(&others).Error; err != nil {
			return err
		}
		allocated := spxQuota.Quantities{}
		for _, azq := range others {
			parsed, err := azq.Resources.Parse()
			if err != nil {
				return err
			}
			allocated = allocated.Add(parsed)
		}
		if err := checkWithin(parsedAZ, allocated, parsedProj, ErrProjectQuotaExceeded, "project"); err != nil {
			return err
		}

		return upsert(tx, &saved, []string{"project_id", "code_az"}, "resources")
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
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

// checkWithin checks that requested only limits resources of limit, and that
// allocated + requested fits in limit.
func checkWithin(requested, allocated, limit spxQuota.Quantities, errExceeded error, level string) error {
	if names := requested.NotIn(limit); len(names) > 0 {
		return fmt.Errorf("%w: %s not limited by the %s quota", errExceeded, strings.Join(names, ", "), level)
	}
	if msg, exceeded := allocated.Add(requested).Exceeds(limit); exceeded {
		return fmt.Errorf("%w: %s", errExceeded, msg)
	}
	return nil
}

// saveProjectQuota writes a project quota (parsed being its parsed form), then splits it
// equally among the AZs of the organization, overwriting the previous AZ quotas.
func saveProjectQuota(tx *gorm.DB, orgId, projectId uuid.UUID, resources model.QuotaResources, parsed spxQuota.Quantities) (model.ProjectQuota, error) {
	pq := model.ProjectQuota{ProjectId: projectId, OrganizationId: orgId, Resources: resources}
	if err := upsert(tx, &pq, []string{"project_id"}, "organization_id", "resources"); err != nil {
		return pq, err
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

// listProjectQuotas lists the quotas of the live projects of the organization. Quotas of
// deleted projects are ignored, so they do not keep consuming the organization quota.
func listProjectQuotas(tx *gorm.DB, orgId uuid.UUID) ([]model.ProjectQuota, error) {
	var pqs []model.ProjectQuota
	err := tx.Where("organization_id = ? AND project_id IN (?)", orgId,
		tx.Session(&gorm.Session{NewDB: true}).Model(&model.Project{}).Select("id").Where("orga_id = ?", orgId)).
		Order("project_id").Find(&pqs).Error
	return pqs, err
}

func sumProjectQuotas(pqs []model.ProjectQuota) (spxQuota.Quantities, error) {
	total := spxQuota.Quantities{}
	for _, pq := range pqs {
		parsed, err := pq.Resources.Parse()
		if err != nil {
			return nil, err
		}
		total = total.Add(parsed)
	}
	return total, nil
}
