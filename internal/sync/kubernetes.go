package sync

import (
	"context"
	"fmt"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubernetesApplier projects the PostgreSQL desired state into the single
// InferScale-owned custom resource. Child resources remain exclusively owned
// by the controller.
type KubernetesApplier struct {
	Client     client.Client
	FieldOwner string
}

func (a KubernetesApplier) Apply(ctx context.Context, resource *platformv1alpha1.InferenceDeployment) error {
	if a.Client == nil {
		return fmt.Errorf("Kubernetes sync client is not configured")
	}
	fieldOwner := a.FieldOwner
	if fieldOwner == "" {
		fieldOwner = "inferscale-api"
	}
	if err := a.Client.Patch(ctx, resource, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply InferenceDeployment %s/%s: %w", resource.Namespace, resource.Name, err)
	}
	return nil
}

func (a KubernetesApplier) Delete(ctx context.Context, namespace, name string) error {
	if a.Client == nil {
		return fmt.Errorf("Kubernetes sync client is not configured")
	}
	resource := &platformv1alpha1.InferenceDeployment{}
	resource.Namespace = namespace
	resource.Name = name
	if err := a.Client.Delete(ctx, resource); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete InferenceDeployment %s/%s: %w", namespace, name, err)
	}
	return nil
}
