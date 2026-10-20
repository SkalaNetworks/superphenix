package quotasync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	crudQuota "github.com/super-phenix/superphenix/internal/superphenix-api/internal/db/crud/quota"
	"github.com/super-phenix/superphenix/internal/superphenix-api/internal/db/model"
	"github.com/super-phenix/superphenix/internal/superphenix-api/pkg/config"
	spxQuota "github.com/super-phenix/superphenix/internal/superphenix-api/pkg/quota"
)

func TestGetProjectUsage(t *testing.T) {
	orgId, projectId := uuid.New(), uuid.New()
	controller := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/"+orgId.String()+"/"+projectId.String()+"/quota", r.URL.Path)
			assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(body))
		}))
	}
	az1 := controller(`{"hard":{"requests.cpu":"4","limits.cpu":"4"},"used":{"requests.cpu":"1","limits.cpu":"2500m","requests.memory":"1Gi"}}`)
	defer az1.Close()
	az2 := controller(`{"hard":{"requests.cpu":"4"},"used":{"requests.cpu":"500m","requests.storage":"10Gi"}}`)
	defer az2.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer down.Close()

	old := config.Global.AZs
	config.Global.AZs = map[string]config.AZConfig{
		"az1": {Code: "az1", ControllerUrl: az1.URL, AuthSecret: "secret"},
		"az2": {Code: "az2", ControllerUrl: az2.URL, AuthSecret: "secret"},
	}
	t.Cleanup(func() { config.Global.AZs = old })

	usage := GetProjectUsage(context.Background(), orgId, projectId)
	assert.True(t, usage.Complete)
	assert.Equal(t, model.QuotaResources{"cpu": "3", "memory": "1Gi", "disk": "10Gi"}, usage.Used)
	assert.Len(t, usage.AZs, 2)
	assert.Equal(t, model.QuotaResources{"cpu": "2500m", "memory": "1Gi"}, usage.AZs[0].Used)
	assert.Equal(t, "2500m", usage.AZs[0].KubernetesUsed["limits.cpu"])

	// An unreachable AZ is reported as unavailable, the others are still summed.
	config.Global.AZs["az2"] = config.AZConfig{Code: "az2", ControllerUrl: down.URL, AuthSecret: "secret"}
	usage = GetProjectUsage(context.Background(), orgId, projectId)
	assert.False(t, usage.Complete)
	assert.True(t, usage.AZs[0].Available)
	assert.False(t, usage.AZs[1].Available)
	assert.Equal(t, model.QuotaResources{"cpu": "2500m", "memory": "1Gi"}, usage.Used)

	// The reader registered for the crud checks gives the enforced and used quantities of one AZ.
	azUsage, err := crudQuota.ReadUsage(context.Background(), orgId, projectId, "az1")
	require.NoError(t, err)
	assert.Equal(t, spxQuota.Quantities{"cpu": 4000}, azUsage.Hard)
	assert.Equal(t, spxQuota.Quantities{"cpu": 2500, "memory": 1 << 30}, azUsage.Used)
	_, err = crudQuota.ReadUsage(context.Background(), orgId, projectId, "az2")
	require.Error(t, err)
	_, err = crudQuota.ReadUsage(context.Background(), orgId, projectId, "az-unknown")
	require.Error(t, err)
}
