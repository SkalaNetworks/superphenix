package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/super-phenix/superphenix/internal/superphenix-api/internal/db/model"
)

func TestGetProjectQuotaOverview(t *testing.T) {
	orgID, projID := uuid.New(), uuid.New()

	t.Run("limited", func(t *testing.T) {
		mock := setupMockDB(t)
		expectProject(mock, orgID, projID)
		mock.ExpectQuery(sqlProjectQuota).WithArgs(projID, 1).
			WillReturnRows(sqlmock.NewRows([]string{"project_id", "organization_id", "resources"}).AddRow(projID, orgID, `{"cpu":"16","memory":"64Gi"}`))
		expectProjectAZQs(mock, projID,
			[2]string{"az-1", `{"cpu":"4","memory":"1Gi"}`},
			[2]string{"az-2", `{"cpu":"500m","disk":"10Gi"}`})

		overview, err := GetProjectQuotaOverview(context.Background(), orgID, projID)
		require.NoError(t, err)
		assert.False(t, overview.Unlimited)
		assert.Equal(t, model.QuotaResources{"cpu": "16", "memory": "64Gi"}, overview.Limit)
		assert.Equal(t, model.QuotaResources{"cpu": "4500m", "memory": "1Gi", "disk": "10Gi"}, overview.Allocated)
		// Only the resources the project limits remain.
		assert.Equal(t, model.QuotaResources{"cpu": "11500m", "memory": "63Gi"}, overview.Remaining)

		raw, err := json.Marshal(overview)
		require.NoError(t, err)
		assert.JSONEq(t, fmt.Sprintf(`{
			"projectId": %q, "organizationId": %q, "unlimited": false,
			"limit": {"cpu": "16", "memory": "64Gi"},
			"allocated": {"cpu": "4500m", "memory": "1Gi", "disk": "10Gi"},
			"remaining": {"cpu": "11500m", "memory": "63Gi"},
			"azQuotas": [
				{"codeAZ": "az-1", "limit": {"cpu": "4", "memory": "1Gi"}, "available": false, "used": {}},
				{"codeAZ": "az-2", "limit": {"cpu": "500m", "disk": "10Gi"}, "available": false, "used": {}}
			]}`, projID, orgID), string(raw))
	})

	t.Run("unlimited", func(t *testing.T) {
		mock := setupMockDB(t)
		expectProject(mock, orgID, projID)
		mock.ExpectQuery(sqlProjectQuota).WithArgs(projID, 1).WillReturnError(gorm.ErrRecordNotFound)
		expectProjectAZQs(mock, projID, [2]string{"az-1", `{"cpu":"2"}`})

		overview, err := GetProjectQuotaOverview(context.Background(), orgID, projID)
		require.NoError(t, err)
		assert.True(t, overview.Unlimited)
		assert.Nil(t, overview.Limit)
		assert.Nil(t, overview.Remaining)
		assert.Equal(t, model.QuotaResources{"cpu": "2"}, overview.Allocated)
	})
}

func TestOrganizationQuotaOverviewJSON(t *testing.T) {
	orgID, projID := uuid.New(), uuid.New()
	mock := setupMockDB(t)
	expectOrgQuota(mock, orgID, `{"cpu":"8"}`)
	expectListPQs(mock, orgID, [2]any{projID, `{"cpu":"2"}`})
	expectOrgAZQs(mock, orgID)

	overview, err := GetOrganizationQuotaOverview(context.Background(), orgID)
	require.NoError(t, err)
	raw, err := json.Marshal(overview)
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{
		"organizationId": %q, "unlimited": false,
		"limit": {"cpu": "8"}, "allocated": {"cpu": "2"}, "remaining": {"cpu": "6"},
		"projectQuotas": [{"projectId": %q, "organizationId": %q, "limit": {"cpu": "2"}}]
	}`, orgID, projID, orgID), string(raw))
}
