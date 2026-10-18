package quota

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	crudQuota "github.com/super-phenix/superphenix/internal/superphenix-api/internal/db/crud/quota"
	"github.com/super-phenix/superphenix/internal/superphenix-api/internal/db/model"
	"github.com/super-phenix/superphenix/internal/superphenix-api/internal/quotasync"
	"github.com/super-phenix/superphenix/pkg/utils/decoder"
	httpError "github.com/super-phenix/superphenix/pkg/utils/error"
	logger "github.com/super-phenix/superphenix/pkg/utils/log"
)

// maxBodySizeMB bounds the size of a quota request body.
const maxBodySizeMB = 1

// GetOrganizationQuota handles GET /organization/{orgaId}/quota
func (h *Service) GetOrganizationQuota(w http.ResponseWriter, r *http.Request) {
	orgID, ok := ParseID(w, r, "orgaId")
	if !ok {
		return
	}
	overview, err := crudQuota.GetOrganizationQuotaOverview(r.Context(), orgID)
	if err != nil {
		WriteError(w, r, err, "failed to get organization quota")
		return
	}
	WriteJSON(w, http.StatusOK, overview)
}

// GetProjectQuota handles GET /organization/{orgaId}/project/{projectId}/quota
func (h *Service) GetProjectQuota(w http.ResponseWriter, r *http.Request) {
	orgID, projID, ok := parseOrgAndProject(w, r)
	if !ok {
		return
	}
	overview, err := crudQuota.GetProjectQuotaOverview(r.Context(), orgID, projID)
	if err != nil {
		WriteError(w, r, err, "failed to get project quota")
		return
	}
	WriteJSON(w, http.StatusOK, overview)
}

// SetProjectQuota handles PUT /organization/{orgaId}/project/{projectId}/quota
func (h *Service) SetProjectQuota(w http.ResponseWriter, r *http.Request) {
	orgID, projID, ok := parseOrgAndProject(w, r)
	if !ok {
		return
	}
	req, ok := DecodeResources(w, r)
	if !ok {
		return
	}
	pq, err := crudQuota.SetProjectQuota(r.Context(), orgID, projID, req)
	if err != nil {
		WriteError(w, r, err, "failed to set project quota")
		return
	}
	if err := quotasync.Project(r.Context(), orgID, projID); err != nil {
		WriteError(w, r, err, "failed to apply project quota")
		return
	}
	WriteJSON(w, http.StatusOK, pq)
}

// SetProjectAZQuota handles PUT /organization/{orgaId}/project/{projectId}/quota/az/{codeAZ}
func (h *Service) SetProjectAZQuota(w http.ResponseWriter, r *http.Request) {
	orgID, projID, ok := parseOrgAndProject(w, r)
	if !ok {
		return
	}
	req, ok := DecodeResources(w, r)
	if !ok {
		return
	}
	codeAZ := chi.URLParam(r, "codeAZ")
	azq, err := crudQuota.SetProjectAZQuota(r.Context(), orgID, projID, codeAZ, req)
	if err != nil {
		WriteError(w, r, err, "failed to set project az quota")
		return
	}
	if err := quotasync.Project(r.Context(), orgID, projID, codeAZ); err != nil {
		WriteError(w, r, err, "failed to apply project az quota")
		return
	}
	WriteJSON(w, http.StatusOK, azq)
}

// DeleteProjectQuota handles DELETE /organization/{orgaId}/project/{projectId}/quota
func (h *Service) DeleteProjectQuota(w http.ResponseWriter, r *http.Request) {
	orgID, projID, ok := parseOrgAndProject(w, r)
	if !ok {
		return
	}
	if err := crudQuota.DeleteProjectQuota(r.Context(), orgID, projID); err != nil {
		WriteError(w, r, err, "failed to delete project quota")
		return
	}
	if err := quotasync.Project(r.Context(), orgID, projID); err != nil {
		WriteError(w, r, err, "failed to apply project quota")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseOrgAndProject(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	orgID, ok := ParseID(w, r, "orgaId")
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	projID, ok := ParseID(w, r, "projectId")
	return orgID, projID, ok
}

// ParseID parses the UUID path parameter param, answering 400 when it is invalid.
func ParseID(w http.ResponseWriter, r *http.Request, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		httpError.Http(w, r, http.StatusBadRequest).Msgf("invalid %s", param)
		return uuid.Nil, false
	}
	return id, true
}

// DecodeResources decodes a size-bounded quota request body, answering 400/413/415 on failure.
// The quantities themselves are validated by the crud layer.
func DecodeResources(w http.ResponseWriter, r *http.Request) (model.QuotaResources, bool) {
	var req model.QuotaResources
	if err := decoder.DecodeJSONFromHTTP(w, r, &req, maxBodySizeMB); err != nil {
		var decodeErr *decoder.HTTPJSONDecodeError
		if errors.As(err, &decodeErr) && decodeErr.Status != http.StatusInternalServerError {
			httpError.Http(w, r, decodeErr.Status).Msg(decodeErr.Message)
		} else {
			httpError.Http(w, r, http.StatusBadRequest).Msg("invalid request body")
		}
		return nil, false
	}
	if req == nil {
		req = model.QuotaResources{}
	}
	return req, true
}

// WriteError maps the business errors of the quota crud to an HTTP status. Any other error
// is logged and answered with a generic message, to not leak internal details.
func WriteError(w http.ResponseWriter, r *http.Request, err error, msg string) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, crudQuota.ErrInvalidQuantity),
		errors.Is(err, crudQuota.ErrOrgQuotaExceeded),
		errors.Is(err, crudQuota.ErrProjectQuotaExceeded):
		status = http.StatusBadRequest
	case errors.Is(err, crudQuota.ErrOrganizationNotFound),
		errors.Is(err, crudQuota.ErrProjectNotFound),
		errors.Is(err, crudQuota.ErrAZNotFound):
		status = http.StatusNotFound
	case errors.Is(err, crudQuota.ErrProjectQuotaEnforced):
		status = http.StatusConflict
	case errors.Is(err, quotasync.ErrNotApplied):
		// Logged by quotasync; the message only holds AZ codes.
		status = http.StatusBadGateway
	}
	if status == http.StatusInternalServerError {
		log := logger.GetLogger(r.Context())
		log.Err(err).Str("url", r.URL.Path).Msg(msg)
		httpError.Http(w, r, status).Msg(msg)
		return
	}
	httpError.Http(w, r, status).Msg(err.Error())
}

// WriteJSON answers body as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
