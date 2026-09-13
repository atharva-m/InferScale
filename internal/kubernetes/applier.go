package kubernetes

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Applier owns the controller's server-side-apply field set. Applying the same
// desired objects repeatedly is safe and repairs external drift.
type Applier struct {
	Client     client.Client
	Scheme     *runtime.Scheme
	FieldOwner string
}

func (a Applier) Apply(ctx context.Context, owner client.Object, object client.Object) error {
	if a.Client == nil || a.Scheme == nil {
		return fmt.Errorf("kubernetes applier is not configured")
	}
	if object.GetNamespace() == "" {
		object.SetNamespace(owner.GetNamespace())
	}
	if object.GetNamespace() == owner.GetNamespace() {
		if err := controllerutil.SetControllerReference(owner, object, a.Scheme); err != nil {
			return fmt.Errorf("set owner reference on %T %s/%s: %w", object, object.GetNamespace(), object.GetName(), err)
		}
	}
	fieldOwner := a.FieldOwner
	if fieldOwner == "" {
		fieldOwner = ManagedByValue
	}
	if err := a.Client.Patch(ctx, object, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %T %s/%s: %w", object, object.GetNamespace(), object.GetName(), err)
	}
	return nil
}

func (a Applier) ApplyAll(ctx context.Context, owner client.Object, objects []client.Object) error {
	for _, object := range objects {
		if err := a.Apply(ctx, owner, object); err != nil {
			return err
		}
	}
	return nil
}
