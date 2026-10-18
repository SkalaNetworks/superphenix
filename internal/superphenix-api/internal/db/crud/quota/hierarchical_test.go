package quota

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/super-phenix/superphenix/internal/superphenix-api/internal/db"
	"github.com/super-phenix/superphenix/internal/superphenix-api/internal/db/model"
	"github.com/super-phenix/superphenix/internal/superphenix-api/pkg/config"
)

var (
	sqlLockOrg      = regexp.QuoteMeta(`SELECT "id" FROM "organizations" WHERE id = $1 AND "organizations"."deleted_at" IS NULL LIMIT $2 FOR UPDATE`)
	sqlCheckProject = regexp.QuoteMeta(`SELECT "id" FROM "projects" WHERE (id = $1 AND orga_id = $2) AND "projects"."deleted_at" IS NULL LIMIT $3`)
	sqlOrgQuota     = regexp.QuoteMeta(`SELECT * FROM "organization_quotas" WHERE organization_id = $1 AND "organization_quotas"."deleted_at" IS NULL LIMIT $2`)
	sqlProjectQuota = regexp.QuoteMeta(`SELECT * FROM "project_quotas" WHERE project_id = $1 AND "project_quotas"."deleted_at" IS NULL LIMIT $2`)
	sqlListPQs      = regexp.QuoteMeta(`SELECT * FROM "project_quotas" WHERE (organization_id = $1 AND project_id IN (SELECT "id" FROM "projects" WHERE orga_id = $2 AND "projects"."deleted_at" IS NULL)) AND "project_quotas"."deleted_at" IS NULL ORDER BY project_id`)
	sqlOtherAZs     = regexp.QuoteMeta(`SELECT * FROM "project_az_quotas" WHERE (project_id = $1 AND code_az <> $2) AND "project_az_quotas"."deleted_at" IS NULL`)
	sqlProjects     = regexp.QuoteMeta(`SELECT * FROM "projects" WHERE orga_id = $1 AND "projects"."deleted_at" IS NULL ORDER BY id`)
	sqlUpsertOQ     = regexp.QuoteMeta(`INSERT INTO "organization_quotas"`) + `.*ON CONFLICT \("organization_id"\) DO UPDATE SET "resources"="excluded"."resources"`
	sqlUpsertPQ     = regexp.QuoteMeta(`INSERT INTO "project_quotas"`) + `.*ON CONFLICT \("project_id"\) DO UPDATE SET "organization_id"="excluded"."organization_id","resources"="excluded"."resources"`
	sqlUpsertAZQ    = regexp.QuoteMeta(`INSERT INTO "project_az_quotas"`) + `.*ON CONFLICT \("project_id","code_az"\) DO UPDATE SET "resources"="excluded"."resources"`
)

func setupMockDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)

	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	require.NoError(t, err)

	oldClient := db.Client
	db.Client = gormDB
	t.Cleanup(func() {
		assert.NoError(t, mock.ExpectationsWereMet())
		db.Client = oldClient
		_ = sqlDB.Close()
	})
	return mock
}

func setupAZs(t *testing.T, codes ...string) {
	t.Helper()
	old := config.Global.AZs
	config.Global.AZs = map[string]config.AZConfig{}
	for _, code := range codes {
		config.Global.AZs[code] = config.AZConfig{Code: code}
	}
	t.Cleanup(func() { config.Global.AZs = old })
}

func expectLockOrg(mock sqlmock.Sqlmock, orgID uuid.UUID) {
	mock.ExpectQuery(sqlLockOrg).WithArgs(orgID, 1).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(orgID))
}

func expectProject(mock sqlmock.Sqlmock, orgID, projID uuid.UUID) {
	mock.ExpectQuery(sqlCheckProject).WithArgs(projID, orgID, 1).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(projID))
}

func expectOrgQuota(mock sqlmock.Sqlmock, orgID uuid.UUID, resources string) {
	q := mock.ExpectQuery(sqlOrgQuota).WithArgs(orgID, 1)
	if resources == "" {
		q.WillReturnError(gorm.ErrRecordNotFound)
		return
	}
	q.WillReturnRows(sqlmock.NewRows([]string{"organization_id", "resources"}).AddRow(orgID, resources))
}

func expectListPQs(mock sqlmock.Sqlmock, orgID uuid.UUID, rows ...[2]any) {
	r := sqlmock.NewRows([]string{"project_id", "organization_id", "resources"})
	for _, row := range rows {
		r.AddRow(row[0], orgID, row[1])
	}
	mock.ExpectQuery(sqlListPQs).WithArgs(orgID, orgID).WillReturnRows(r)
}

func expectUpsertPQ(mock sqlmock.Sqlmock, orgID, projID uuid.UUID, resources string) {
	mock.ExpectExec(sqlUpsertPQ).
		WithArgs(projID, orgID, sqlmock.AnyArg(), sqlmock.AnyArg(), nil, resources).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

func expectUpsertAZQ(mock sqlmock.Sqlmock, projID uuid.UUID, codeAZ, resources string) {
	mock.ExpectQuery(sqlUpsertAZQ).
		WithArgs(projID, codeAZ, sqlmock.AnyArg(), sqlmock.AnyArg(), nil, resources).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.New()))
}

func TestGetOrganizationQuotaOverview(t *testing.T) {
	orgID, proj1, proj2 := uuid.New(), uuid.New(), uuid.New()

	t.Run("limited", func(t *testing.T) {
		mock := setupMockDB(t)
		expectOrgQuota(mock, orgID, `{"cpu":"64","memory":"256Gi","pods":"100"}`)
		expectListPQs(mock, orgID,
			[2]any{proj1, `{"cpu":"16","memory":"64Gi","pods":"50"}`},
			[2]any{proj2, `{"cpu":"500m"}`})

		overview, err := GetOrganizationQuotaOverview(context.Background(), orgID)
		require.NoError(t, err)
		assert.False(t, overview.Unlimited)
		assert.Equal(t, model.QuotaResources{"cpu": "16500m", "memory": "64Gi", "pods": "50"}, overview.Allocated)
		assert.Equal(t, model.QuotaResources{"cpu": "47500m", "memory": "192Gi", "pods": "50"}, overview.Remaining)
		assert.Len(t, overview.ProjectQuotas, 2)
	})

	t.Run("unlimited", func(t *testing.T) {
		mock := setupMockDB(t)
		expectOrgQuota(mock, orgID, "")
		expectListPQs(mock, orgID)

		overview, err := GetOrganizationQuotaOverview(context.Background(), orgID)
		require.NoError(t, err)
		assert.True(t, overview.Unlimited)
		assert.Nil(t, overview.Remaining)
	})
}

func TestSetOrganizationQuota(t *testing.T) {
	orgID, proj1, proj2 := uuid.New(), uuid.New(), uuid.New()

	t.Run("auto-split among projects and AZs when no project quota exists", func(t *testing.T) {
		setupAZs(t, "az-2", "az-1")
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectListPQs(mock, orgID)
		mock.ExpectQuery(sqlProjects).WithArgs(orgID).
			WillReturnRows(sqlmock.NewRows([]string{"id", "orga_id"}).AddRow(proj1, orgID).AddRow(proj2, orgID))
		for _, p := range []uuid.UUID{proj1, proj2} {
			expectUpsertPQ(mock, orgID, p, `{"count/virtualmachines.kubevirt.io":"1","cpu":"32"}`)
			// A share that rounds down to 0 stays limited at 0.
			expectUpsertAZQ(mock, p, "az-1", `{"count/virtualmachines.kubevirt.io":"0","cpu":"16"}`)
			expectUpsertAZQ(mock, p, "az-2", `{"count/virtualmachines.kubevirt.io":"0","cpu":"16"}`)
		}
		mock.ExpectExec(sqlUpsertOQ).WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		oq, err := SetOrganizationQuota(context.Background(), orgID, model.QuotaResources{"cpu": "64", "count/virtualmachines.kubevirt.io": "2"})
		require.NoError(t, err)
		assert.Equal(t, "64", oq.Resources["cpu"])
	})

	t.Run("existing project quotas must fit", func(t *testing.T) {
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectListPQs(mock, orgID, [2]any{proj1, `{"cpu":"32"}`}, [2]any{proj2, `{"cpu":"33"}`})
		mock.ExpectRollback()

		_, err := SetOrganizationQuota(context.Background(), orgID, model.QuotaResources{"cpu": "64"})
		require.ErrorIs(t, err, ErrOrgQuotaExceeded)
	})

	t.Run("unknown organization", func(t *testing.T) {
		mock := setupMockDB(t)

		mock.ExpectBegin()
		mock.ExpectQuery(sqlLockOrg).WithArgs(orgID, 1).WillReturnError(gorm.ErrRecordNotFound)
		mock.ExpectRollback()

		_, err := SetOrganizationQuota(context.Background(), orgID, model.QuotaResources{"cpu": "64"})
		require.ErrorIs(t, err, ErrOrganizationNotFound)
	})

	t.Run("invalid resources are rejected before any query", func(t *testing.T) {
		setupMockDB(t)
		_, err := SetOrganizationQuota(context.Background(), orgID, model.QuotaResources{"limits.cpu": "64"})
		require.ErrorIs(t, err, ErrInvalidQuantity)
	})
}

func TestSetProjectQuota(t *testing.T) {
	orgID, projID, otherProjID := uuid.New(), uuid.New(), uuid.New()

	t.Run("within the organization quota, split among AZs", func(t *testing.T) {
		setupAZs(t, "az-1")
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectProject(mock, orgID, projID)
		expectOrgQuota(mock, orgID, `{"cpu":"64","memory":"256Gi"}`)
		expectListPQs(mock, orgID, [2]any{otherProjID, `{"cpu":"32","memory":"128Gi"}`}, [2]any{projID, `{"cpu":"60"}`})
		expectUpsertPQ(mock, orgID, projID, `{"cpu":"32","memory":"128Gi"}`)
		expectUpsertAZQ(mock, projID, "az-1", `{"cpu":"32","memory":"128Gi"}`)
		mock.ExpectCommit()

		pq, err := SetProjectQuota(context.Background(), orgID, projID, model.QuotaResources{"cpu": "32", "memory": "128Gi"})
		require.NoError(t, err)
		assert.Equal(t, orgID, pq.OrganizationId)
	})

	t.Run("unlimited organization", func(t *testing.T) {
		setupAZs(t)
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectProject(mock, orgID, projID)
		expectOrgQuota(mock, orgID, "")
		expectUpsertPQ(mock, orgID, projID, `{"pods":"1000"}`)
		mock.ExpectCommit()

		_, err := SetProjectQuota(context.Background(), orgID, projID, model.QuotaResources{"pods": "1000"})
		require.NoError(t, err)
	})

	t.Run("exceeds the organization quota", func(t *testing.T) {
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectProject(mock, orgID, projID)
		expectOrgQuota(mock, orgID, `{"cpu":"64"}`)
		expectListPQs(mock, orgID, [2]any{otherProjID, `{"cpu":"48"}`})
		mock.ExpectRollback()

		_, err := SetProjectQuota(context.Background(), orgID, projID, model.QuotaResources{"cpu": "17"})
		require.ErrorIs(t, err, ErrOrgQuotaExceeded)
	})

	t.Run("omitting a resource limited by the organization counts as 0", func(t *testing.T) {
		setupAZs(t)
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectProject(mock, orgID, projID)
		expectOrgQuota(mock, orgID, `{"cpu":"64","memory":"1Gi"}`)
		expectListPQs(mock, orgID, [2]any{otherProjID, `{"cpu":"32","memory":"1Gi"}`})
		expectUpsertPQ(mock, orgID, projID, `{"cpu":"32"}`)
		mock.ExpectCommit()

		_, err := SetProjectQuota(context.Background(), orgID, projID, model.QuotaResources{"cpu": "32"})
		require.NoError(t, err)
	})

	t.Run("resource not limited by the organization", func(t *testing.T) {
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectProject(mock, orgID, projID)
		expectOrgQuota(mock, orgID, `{"cpu":"64"}`)
		expectListPQs(mock, orgID)
		mock.ExpectRollback()

		_, err := SetProjectQuota(context.Background(), orgID, projID, model.QuotaResources{"cpu": "1", "pods": "10000"})
		require.ErrorIs(t, err, ErrOrgQuotaExceeded)
		assert.Contains(t, err.Error(), "pods not limited by the organization quota")
	})

	t.Run("project of another organization", func(t *testing.T) {
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		mock.ExpectQuery(sqlCheckProject).WithArgs(projID, orgID, 1).WillReturnError(gorm.ErrRecordNotFound)
		mock.ExpectRollback()

		_, err := SetProjectQuota(context.Background(), orgID, projID, model.QuotaResources{"cpu": "1"})
		require.ErrorIs(t, err, ErrProjectNotFound)
	})
}

func TestSetProjectAZQuota(t *testing.T) {
	orgID, projID := uuid.New(), uuid.New()

	expectOtherAZs := func(mock sqlmock.Sqlmock, resources string) {
		mock.ExpectQuery(sqlOtherAZs).WithArgs(projID, "az-1").
			WillReturnRows(sqlmock.NewRows([]string{"project_id", "code_az", "resources"}).AddRow(projID, "az-2", resources))
	}
	expectPQ := func(mock sqlmock.Sqlmock, resources string) {
		q := mock.ExpectQuery(sqlProjectQuota).WithArgs(projID, 1)
		if resources == "" {
			q.WillReturnError(gorm.ErrRecordNotFound)
			return
		}
		q.WillReturnRows(sqlmock.NewRows([]string{"project_id", "organization_id", "resources"}).AddRow(projID, orgID, resources))
	}

	t.Run("within the project quota", func(t *testing.T) {
		setupAZs(t, "az-1", "az-2")
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectProject(mock, orgID, projID)
		expectPQ(mock, `{"cpu":"32","pods":"20"}`)
		expectOtherAZs(mock, `{"cpu":"16","pods":"10"}`)
		expectUpsertAZQ(mock, projID, "az-1", `{"cpu":"16","pods":"10"}`)
		mock.ExpectCommit()

		azq, err := SetProjectAZQuota(context.Background(), orgID, projID, "az-1", model.QuotaResources{"cpu": "16", "pods": "10"})
		require.NoError(t, err)
		assert.Equal(t, "az-1", azq.CodeAZ)
	})

	t.Run("exceeds the project quota", func(t *testing.T) {
		setupAZs(t, "az-1", "az-2")
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectProject(mock, orgID, projID)
		expectPQ(mock, `{"cpu":"32"}`)
		expectOtherAZs(mock, `{"cpu":"20"}`)
		mock.ExpectRollback()

		_, err := SetProjectAZQuota(context.Background(), orgID, projID, "az-1", model.QuotaResources{"cpu": "16"})
		require.ErrorIs(t, err, ErrProjectQuotaExceeded)
	})

	t.Run("resource not limited by the project", func(t *testing.T) {
		setupAZs(t, "az-1")
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectProject(mock, orgID, projID)
		expectPQ(mock, `{"cpu":"32"}`)
		expectOtherAZs(mock, `{}`)
		mock.ExpectRollback()

		_, err := SetProjectAZQuota(context.Background(), orgID, projID, "az-1", model.QuotaResources{"cpu": "1", "pods": "1000"})
		require.ErrorIs(t, err, ErrProjectQuotaExceeded)
	})

	t.Run("project without quota", func(t *testing.T) {
		setupAZs(t, "az-1")
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectProject(mock, orgID, projID)
		expectPQ(mock, "")
		mock.ExpectRollback()

		_, err := SetProjectAZQuota(context.Background(), orgID, projID, "az-1", model.QuotaResources{"pods": "1000"})
		require.ErrorIs(t, err, ErrProjectQuotaRequired)
	})

	t.Run("unknown or non whitelisted AZ", func(t *testing.T) {
		setupMockDB(t)
		config.Global.AZs = map[string]config.AZConfig{"az-1": {Code: "az-1", Whitelist: []string{uuid.NewString()}}}
		t.Cleanup(func() { config.Global.AZs = nil })

		for _, code := range []string{"az-1", "az-unknown"} {
			_, err := SetProjectAZQuota(context.Background(), orgID, projID, code, model.QuotaResources{"cpu": "1"})
			require.ErrorIs(t, err, ErrAZNotFound)
		}
	})
}

func TestDeleteProjectQuota(t *testing.T) {
	orgID, projID := uuid.New(), uuid.New()

	t.Run("deletes the project and AZ quotas", func(t *testing.T) {
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectProject(mock, orgID, projID)
		expectOrgQuota(mock, orgID, "")
		mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM "project_az_quotas" WHERE project_id = $1`)).WithArgs(projID).WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM "project_quotas" WHERE project_id = $1`)).WithArgs(projID).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		require.NoError(t, DeleteProjectQuota(context.Background(), orgID, projID))
	})

	t.Run("refused while the organization has a quota", func(t *testing.T) {
		mock := setupMockDB(t)

		mock.ExpectBegin()
		expectLockOrg(mock, orgID)
		expectProject(mock, orgID, projID)
		expectOrgQuota(mock, orgID, `{"cpu":"64"}`)
		mock.ExpectRollback()

		require.ErrorIs(t, DeleteProjectQuota(context.Background(), orgID, projID), ErrProjectQuotaEnforced)
	})
}
