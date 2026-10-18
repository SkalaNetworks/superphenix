// Package quota enforces the quota of a project in this AZ, as computed by the
// Superphenix API, through a Kubernetes ResourceQuota in the project namespace.
package quota

import (
	"context"
	"fmt"
	"strings"

	"github.com/super-phenix/superphenix/internal/superphenix-controller/internal/k8s"
	"github.com/super-phenix/superphenix/internal/superphenix-controller/pkg/api/utils"
	"github.com/super-phenix/superphenix/internal/superphenix-controller/pkg/config"

	spxId "github.com/super-phenix/superphenix/pkg/superphenix-id"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Name is the name of the ResourceQuota managed by the controller in a project namespace.
const Name = "spx-quota"

// MaxResources bounds the number of resources of a quota.
const MaxResources = 128

// Validate checks that hard only holds valid Kubernetes quota resource names and quantities.
func Validate(hard map[string]string) (v1.ResourceList, error) {
	if len(hard) > MaxResources {
		return nil, fmt.Errorf("at most %d resources are allowed", MaxResources)
	}
	list := make(v1.ResourceList, len(hard))
	for name, value := range hard {
		if errs := validation.IsQualifiedName(name); len(errs) > 0 {
			return nil, fmt.Errorf("invalid resource name %q: %s", name, strings.Join(errs, "; "))
		}
		q, err := resource.ParseQuantity(value)
		if err != nil {
			return nil, fmt.Errorf("invalid quantity %q for %s: %v", value, name, err)
		}
		if q.Sign() < 0 {
			return nil, fmt.Errorf("%s cannot be negative", name)
		}
		list[v1.ResourceName(name)] = q
	}
	return list, nil
}

// Apply creates or replaces the ResourceQuota of the project with hard, creating the
// project namespace if needed. Resources absent from hard are no longer limited.
func Apply(ctx context.Context, orgId, projectId string, hard v1.ResourceList) error {
	if err := k8s.CreateNamespaceIfNotExists(ctx, orgId, projectId); err != nil {
		return err
	}

	m := spxId.Metadata{OrgId: orgId, ProjectId: projectId}
	namespace := utils.GetNamespace(projectId)
	client := config.K8sClient.CoreV1().ResourceQuotas(namespace)

	current, err := client.Get(ctx, Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.Create(ctx, &v1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: Name, Namespace: namespace, Labels: m.GetLabels()},
			Spec:       v1.ResourceQuotaSpec{Hard: hard},
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	current.Spec.Hard = hard
	_, err = client.Update(ctx, current, metav1.UpdateOptions{})
	return err
}

// Delete removes the ResourceQuota of the project, if any.
func Delete(ctx context.Context, projectId string) error {
	err := config.K8sClient.CoreV1().ResourceQuotas(utils.GetNamespace(projectId)).Delete(ctx, Name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
