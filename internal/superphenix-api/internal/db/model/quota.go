package model

import (
	"github.com/google/uuid"

	"github.com/super-phenix/superphenix/internal/superphenix-api/pkg/quota"
)

// QuotaResources is the abstract quota of a level, e.g. {"cpu": "16", "memory": "64Gi", "count/virtualmachines.kubevirt.io": "20"}.
type QuotaResources = quota.Resources

type OrganizationQuota struct {
	OrganizationId uuid.UUID `gorm:"primaryKey;type:uuid;not null"`
	TracingModel
	Resources QuotaResources `gorm:"serializer:json;not null"`
}

func (OrganizationQuota) TableName() string { return "organization_quotas" }

type ProjectQuota struct {
	ProjectId      uuid.UUID `gorm:"primaryKey;type:uuid;not null"`
	OrganizationId uuid.UUID `gorm:"type:uuid;not null;index"`
	TracingModel
	Resources QuotaResources `gorm:"serializer:json;not null"`
}

func (ProjectQuota) TableName() string { return "project_quotas" }

type ProjectAZQuota struct {
	ID        uuid.UUID `gorm:"primaryKey;type:uuid;default:gen_random_uuid();not null"`
	ProjectId uuid.UUID `gorm:"type:uuid;not null;index:idx_proj_az,unique"`
	CodeAZ    string    `gorm:"not null;index:idx_proj_az,unique"`
	TracingModel
	Resources QuotaResources `gorm:"serializer:json;not null"`
}

func (ProjectAZQuota) TableName() string { return "project_az_quotas" }
