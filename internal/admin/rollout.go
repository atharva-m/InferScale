package admin

import (
	"context"
	"fmt"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/deployment"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const AnnotationRolloutControl = kubeutil.AnnotationRolloutControl

type RolloutAction string

const (
	RolloutPause  RolloutAction = "pause"
	RolloutResume RolloutAction = "resume"
	RolloutAbort  RolloutAction = "abort"
)

type RolloutAdmin struct {
	Deployments interface {
		Get(context.Context, string, string) (*deployment.Deployment, error)
	}
	Client client.Client
}

// Set applies an operator override to the live desired-state projection. A
// resume value is retained until the controller observes it so the rollout
// state machine can exclude operator-paused wall time from the stage soak.
func (a RolloutAdmin) Set(ctx context.Context, tenantID, deploymentID string, action RolloutAction) error {
	if a.Deployments == nil || a.Client == nil {
		return fmt.Errorf("rollout admin dependencies are not configured")
	}
	if action != RolloutPause && action != RolloutResume && action != RolloutAbort {
		return fmt.Errorf("unsupported rollout action %q", action)
	}
	value, err := a.Deployments.Get(ctx, tenantID, deploymentID)
	if err != nil {
		return err
	}
	resource := &platformv1alpha1.InferenceDeployment{}
	if err := a.Client.Get(ctx, client.ObjectKey{Namespace: value.Namespace, Name: value.Name}, resource); err != nil {
		return fmt.Errorf("get InferenceDeployment: %w", err)
	}
	before := resource.DeepCopy()
	annotations := resource.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[AnnotationRolloutControl] = string(action)
	// Reassert the immutable public identity while changing operator-owned
	// metadata so a malformed manual edit cannot redirect the route.
	annotations[kubeutil.AnnotationDeploymentID] = value.ID
	resource.SetAnnotations(annotations)
	if err := a.Client.Patch(ctx, resource, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("patch rollout control: %w", err)
	}
	return nil
}
