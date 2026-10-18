package quota

import (
	"context"
	"testing"

	"github.com/super-phenix/superphenix/internal/superphenix-controller/pkg/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	orgId     = "11111111-1111-1111-1111-111111111111"
	projectId = "22222222-2222-2222-2222-222222222222"
	namespace = "spx-22222222-2222-2222-2222-222222222222"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		hard    map[string]string
		wantErr bool
	}{
		{"empty", map[string]string{}, false},
		{"valid", map[string]string{"requests.cpu": "4", "limits.memory": "8Gi", "count/virtualmachines.kubevirt.io": "0"}, false},
		{"invalid name", map[string]string{"bad name": "1"}, true},
		{"yaml injection", map[string]string{"pods\n  secrets": "1"}, true},
		{"invalid quantity", map[string]string{"pods": "abc"}, true},
		{"negative", map[string]string{"pods": "-1"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Validate(tt.hard)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestApplyAndDelete(t *testing.T) {
	fakeClient := fake.NewClientset()
	config.K8sClient = fakeClient
	config.Global.SpxPrefix = "spx"
	ctx := context.Background()

	hard, err := Validate(map[string]string{"requests.cpu": "4", "pods": "10"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, orgId, projectId, hard); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := fakeClient.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("namespace not created: %v", err)
	}

	// Replacing the quota drops the resources that are no longer limited.
	hard, _ = Validate(map[string]string{"pods": "0"})
	if err := Apply(ctx, orgId, projectId, hard); err != nil {
		t.Fatalf("update: %v", err)
	}
	rq, err := fakeClient.CoreV1().ResourceQuotas(namespace).Get(ctx, Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("quota not found: %v", err)
	}
	if len(rq.Spec.Hard) != 1 || rq.Spec.Hard.Pods().String() != "0" {
		t.Errorf("unexpected hard %v", rq.Spec.Hard)
	}
	if rq.Labels == nil || len(rq.Labels) == 0 {
		t.Errorf("expected spx labels on the quota")
	}

	if err := Delete(ctx, projectId); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := Delete(ctx, projectId); err != nil {
		t.Fatalf("delete should be idempotent: %v", err)
	}
	if _, err := fakeClient.CoreV1().ResourceQuotas(namespace).Get(ctx, Name, metav1.GetOptions{}); err == nil {
		t.Errorf("quota should be deleted")
	}
}
