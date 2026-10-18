package k8s

import (
	"net/http"

	"github.com/super-phenix/superphenix/internal/superphenix-controller/internal/k8s/quota"
	"github.com/super-phenix/superphenix/internal/superphenix-controller/pkg/api/utils"
	"github.com/super-phenix/superphenix/pkg/utils/decoder"
	httpError "github.com/super-phenix/superphenix/pkg/utils/error"
	logger "github.com/super-phenix/superphenix/pkg/utils/log"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const baseQuotaEndpoint = "/quota"

// maxQuotaBodySizeMB bounds the size of a quota request body.
const maxQuotaBodySizeMB = 1

// QuotaBody is the Kubernetes quota of a project in this AZ, computed by the Superphenix API.
type QuotaBody struct {
	// Hard holds the ResourceQuota `spec.hard` entries (e.g. "requests.cpu": "4", "count/virtualmachines.kubevirt.io": "10").
	Hard map[string]string `json:"hard"`
}

// QuotaEndpoint is only called by the Superphenix API: it is not proxied to tenants.
func QuotaEndpoint(router chi.Router) {
	router.Route(baseQuotaEndpoint, func(r chi.Router) {
		r.Put("/", setQuota)
	})
}

// setQuota
//
//	@Summary		Set the project quota
//	@Description	Create or replace the ResourceQuota of the project in this AZ. An empty quota removes it.
//	@Tags			v1, Quota
//	@Accept			json
//	@Param			orgId		path	string		true	"Organization ID"
//	@Param			projectId	path	string		true	"Project ID"
//	@Param			body		body	QuotaBody	true	"Kubernetes quota"
//	@Success		204
//	@Failure		400
//	@Failure		500
//	@Router			/{orgId}/{projectId}/quota [put]
func setQuota(w http.ResponseWriter, r *http.Request) {
	log := logger.GetLogger(r.Context())
	orgId, projectId, ok := parseQuotaParams(w, r)
	if !ok {
		return
	}

	var body QuotaBody
	if err := decoder.HandleHTTPJSON(w, r, &body, maxQuotaBodySizeMB); err != nil {
		return
	}
	hard, err := quota.Validate(body.Hard)
	if err != nil {
		httpError.Http(w, r, http.StatusBadRequest).Msg(err.Error())
		return
	}

	if len(hard) == 0 {
		err = quota.Delete(r.Context(), projectId)
	} else {
		err = quota.Apply(r.Context(), orgId, projectId, hard)
	}
	if err != nil {
		log.Err(err).Str("projectId", projectId).Msg("failed to set project quota")
		httpError.Http(w, r, http.StatusInternalServerError).Msg("failed to set project quota")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseQuotaParams ensures the organization and project IDs are UUIDs, since they
// name the namespace and its labels.
func parseQuotaParams(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	orgId, projectId, _ := utils.GetRequestParams(r)
	if _, err := uuid.Parse(orgId); err != nil {
		httpError.Http(w, r, http.StatusBadRequest).Msg("invalid orgId")
		return "", "", false
	}
	if _, err := uuid.Parse(projectId); err != nil {
		httpError.Http(w, r, http.StatusBadRequest).Msg("invalid projectId")
		return "", "", false
	}
	return orgId, projectId, true
}
