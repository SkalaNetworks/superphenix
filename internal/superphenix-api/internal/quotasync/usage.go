package quotasync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/super-phenix/superphenix/internal/superphenix-api/internal/az"
	crudQuota "github.com/super-phenix/superphenix/internal/superphenix-api/internal/db/crud/quota"
	"github.com/super-phenix/superphenix/internal/superphenix-api/internal/db/model"
	"github.com/super-phenix/superphenix/internal/superphenix-api/pkg/api/publicHttp/proxy"
	"github.com/super-phenix/superphenix/internal/superphenix-api/pkg/config"
	spxQuota "github.com/super-phenix/superphenix/internal/superphenix-api/pkg/quota"
	logger "github.com/super-phenix/superphenix/pkg/utils/log"
)

const (
	// usageTimeout bounds the call to one AZ controller, so that an unreachable AZ only
	// makes its own usage unavailable.
	usageTimeout = 10 * time.Second
	// usageConcurrency bounds the number of AZ controller calls running at once.
	usageConcurrency = 8
	// maxUsageBodySize bounds the size of a controller answer.
	maxUsageBodySize = 1 << 20
)

// controllerStatus is the body answered by GET {controller}/{orgId}/{projectId}/quota.
type controllerStatus struct {
	Hard map[string]string `json:"hard"`
	Used map[string]string `json:"used"`
}

// AZUsage is what a project currently uses in one AZ, read from the `status.used` of its
// ResourceQuota. Kubernetes only tracks the resources the quota limits, so a resource the
// project has no limit for in this AZ is absent.
type AZUsage struct {
	CodeAZ string `json:"codeAZ"`
	// Available is false when the AZ controller could not be reached: Used is then empty.
	Available bool                 `json:"available"`
	Used      model.QuotaResources `json:"used"`
}

// ProjectUsage is what a project currently uses, per AZ and summed over its AZs.
type ProjectUsage struct {
	ProjectId string `json:"projectId"`
	// Complete is false when the usage of at least one AZ is unavailable (Used then misses it).
	Complete bool                 `json:"complete"`
	Used     model.QuotaResources `json:"used"`
	AZs      []AZUsage            `json:"azs"`
}

// OrganizationUsage is what an organization currently uses, per project and summed over its projects.
type OrganizationUsage struct {
	OrganizationId string `json:"organizationId"`
	// Complete is false when the usage of at least one project is incomplete.
	Complete bool                 `json:"complete"`
	Used     model.QuotaResources `json:"used"`
	Projects []ProjectUsage       `json:"projects"`
}

func init() {
	crudQuota.ReadUsage = readAZUsage
}

// readAZUsage reads the quota enforced and used by a project in one AZ (see crudQuota.ReadUsage).
func readAZUsage(ctx context.Context, orgId, projectId uuid.UUID, codeAZ string) (crudQuota.AZUsage, error) {
	azCfg, err := az.GetByCode(codeAZ, orgId.String())
	if err != nil {
		return crudQuota.AZUsage{}, err
	}
	status, err := fetch(ctx, azCfg, orgId, projectId)
	if err != nil {
		return crudQuota.AZUsage{}, err
	}
	return crudQuota.AZUsage{
		Hard: spxQuota.FromKubernetesUsage(status.Hard),
		Used: spxQuota.FromKubernetesUsage(status.Used),
	}, nil
}

// GetProjectUsage reads the usage of a project in every AZ of the organization. The caller must
// have checked that the project belongs to the organization. An unreachable AZ does not fail
// the call: it is reported as unavailable and the usage is flagged incomplete.
func GetProjectUsage(ctx context.Context, orgId, projectId uuid.UUID) ProjectUsage {
	usage, _ := projectUsage(ctx, orgId, projectId, targets(orgId, nil))
	return usage
}

// GetOrganizationUsage reads the usage of every project of the organization in every AZ.
func GetOrganizationUsage(ctx context.Context, orgId uuid.UUID) (OrganizationUsage, error) {
	usage := OrganizationUsage{OrganizationId: orgId.String(), Complete: true, Used: model.QuotaResources{}, Projects: []ProjectUsage{}}
	ids, err := crudQuota.ListProjectIds(ctx, orgId)
	if err != nil {
		return usage, err
	}

	azs := targets(orgId, nil)
	usage.Projects = make([]ProjectUsage, len(ids))
	used := make([]spxQuota.Quantities, len(ids))
	g := new(errgroup.Group)
	g.SetLimit(usageConcurrency)
	for i, projectId := range ids {
		g.Go(func() error {
			usage.Projects[i], used[i] = projectUsage(ctx, orgId, projectId, azs)
			return nil
		})
	}
	_ = g.Wait()

	total := spxQuota.Quantities{}
	for i, p := range usage.Projects {
		usage.Complete = usage.Complete && p.Complete
		total = total.Add(used[i])
	}
	usage.Used = total.Resources()
	return usage, nil
}

// projectUsage reads the usage of a project in the given AZs, also returned summed as quantities.
func projectUsage(ctx context.Context, orgId, projectId uuid.UUID, azs []config.AZConfig) (ProjectUsage, spxQuota.Quantities) {
	usage := ProjectUsage{ProjectId: projectId.String(), Complete: true, AZs: make([]AZUsage, 0, len(azs))}
	total := spxQuota.Quantities{}
	log := logger.GetLogger(ctx)
	for _, azCfg := range azs {
		azUsage := AZUsage{CodeAZ: azCfg.Code, Used: model.QuotaResources{}}
		status, err := fetch(ctx, azCfg, orgId, projectId)
		if err != nil {
			log.Err(err).Str("az", azCfg.Code).Str("projectId", projectId.String()).Msg("failed to get quota usage")
			usage.Complete = false
		} else {
			used := spxQuota.FromKubernetesUsage(status.Used)
			azUsage.Available = true
			azUsage.Used = used.Resources()
			total = total.Add(used)
		}
		usage.AZs = append(usage.AZs, azUsage)
	}
	usage.Used = total.Resources()
	return usage, total
}

// fetch reads the ResourceQuota of a project from the controller of an AZ.
func fetch(ctx context.Context, azCfg config.AZConfig, orgId, projectId uuid.UUID) (controllerStatus, error) {
	var status controllerStatus
	ctx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/%s/%s/%s", azCfg.ControllerUrl, orgId, projectId, quotaEndpoint)
	resp, err := proxy.SendRequest(ctx, url, http.MethodGet, nil, azCfg.AuthSecret)
	if err != nil {
		return status, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return status, fmt.Errorf("controller answered %d", resp.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, maxUsageBodySize)).Decode(&status)
	return status, err
}
