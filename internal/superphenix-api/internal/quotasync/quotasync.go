// Package quotasync forwards the quotas stored by the API to the controller of each AZ,
// which enforces them through a Kubernetes ResourceQuota in the project namespace.
//
// The API database is the source of truth: a push sends the whole effective quota of a
// project in an AZ (never a diff), so pushing again is always safe and fixes any drift.
package quotasync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/super-phenix/superphenix/internal/superphenix-api/internal/az"
	crudQuota "github.com/super-phenix/superphenix/internal/superphenix-api/internal/db/crud/quota"
	"github.com/super-phenix/superphenix/internal/superphenix-api/pkg/api/publicHttp/proxy"
	"github.com/super-phenix/superphenix/internal/superphenix-api/pkg/config"
	logger "github.com/super-phenix/superphenix/pkg/utils/log"
)

const quotaEndpoint = "quota"

// ErrNotApplied is returned when the quota is saved but could not be pushed to every AZ.
var ErrNotApplied = errors.New("quota saved but not applied on every availability zone, retry the request")

// controllerBody is the body of PUT {controller}/{orgId}/{projectId}/quota.
type controllerBody struct {
	Hard map[string]string `json:"hard"`
}

// Project pushes the quota of a project to the given AZs (every AZ of the organization
// when none is given).
func Project(ctx context.Context, orgId, projectId uuid.UUID, azCodes ...string) error {
	h, err := crudQuota.GetHierarchy(ctx, orgId, projectId)
	if err != nil {
		return err
	}

	log := logger.GetLogger(ctx)
	var failed []string
	for _, azCfg := range targets(orgId, azCodes) {
		if err := push(ctx, azCfg, orgId, projectId, h.AZ(azCfg.Code).ToKubernetesQuota()); err != nil {
			log.Err(err).Str("az", azCfg.Code).Str("projectId", projectId.String()).Msg("failed to apply quota")
			failed = append(failed, azCfg.Code)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w: %s", ErrNotApplied, strings.Join(failed, ", "))
	}
	return nil
}

// Organization pushes the quota of every project of the organization to every AZ.
func Organization(ctx context.Context, orgId uuid.UUID) error {
	ids, err := crudQuota.ListProjectIds(ctx, orgId)
	if err != nil {
		return err
	}
	var errs []error
	for _, projectId := range ids {
		if err := Project(ctx, orgId, projectId); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w (%d project(s))", ErrNotApplied, len(errs))
	}
	return nil
}

func targets(orgId uuid.UUID, azCodes []string) []config.AZConfig {
	azs := az.FindAll(orgId.String())
	if len(azCodes) > 0 {
		azs = slices.DeleteFunc(azs, func(a config.AZConfig) bool { return !slices.Contains(azCodes, a.Code) })
	}
	slices.SortFunc(azs, func(a, b config.AZConfig) int { return strings.Compare(a.Code, b.Code) })
	return azs
}

// push sends the Kubernetes quota of a project to the controller of an AZ. An empty
// quota removes the ResourceQuota.
func push(ctx context.Context, azCfg config.AZConfig, orgId, projectId uuid.UUID, hard map[string]string) error {
	body, err := json.Marshal(controllerBody{Hard: hard})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/%s/%s/%s", azCfg.ControllerUrl, orgId, projectId, quotaEndpoint)
	resp, err := proxy.SendRequest(ctx, url, http.MethodPut, bytes.NewReader(body), azCfg.AuthSecret)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("controller answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
