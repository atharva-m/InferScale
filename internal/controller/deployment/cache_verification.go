package deployment

import (
	"context"
	"fmt"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/controller/modelcache"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Only share a verification result after observing the physical node on which
// this revision consumes the cache. A SKU selector alone is insufficient: two
// nodes can have that same selector while storing different local bytes.
func (r *Reconciler) modelCacheVerifier(
	ctx context.Context,
	renderer modelcache.Renderer,
	spec platformruntime.Spec,
	revision platformruntime.Revision,
) (*batchv1.Job, error) {
	legacy, err := renderer.VerificationJob(spec, revision)
	if err != nil {
		return nil, err
	}
	// Consume any existing per-revision result before switching to a shared
	// proof. In particular, migration must never hide failed verification.
	previous := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(legacy), previous); err == nil {
		return legacy, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get existing model-cache verifier: %w", err)
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(spec.Namespace), client.MatchingLabels{
		kubeutil.LabelManagedBy: kubeutil.ManagedByValue,
		kubeutil.LabelRevision:  revision.Name,
		kubeutil.LabelComponent: "model-server",
	}); err != nil {
		return nil, fmt.Errorf("observe model-cache consumer node: %w", err)
	}
	nodes := map[string]struct{}{}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Spec.NodeName != "" && podUsesCachePath(pod, renderer.Path(spec)) &&
			pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			nodes[pod.Spec.NodeName] = struct{}{}
		}
	}
	if len(nodes) == 1 {
		for node := range nodes {
			return renderer.SharedVerificationJob(spec, node, modelcache.SharedVerificationNamespace)
		}
	}
	// Startup and scale-to-zero may have no observable node yet. Retain the
	// existing per-revision verification until placement is known, rather than
	// accepting a proof from an arbitrary node.
	return legacy, nil
}

func (r *Reconciler) createModelCacheVerifier(ctx context.Context, owner *platformv1alpha1.InferenceDeployment, verifier *batchv1.Job) error {
	if verifier.Labels[kubeutil.LabelRevision] != "" {
		return r.Applier.Apply(ctx, owner, verifier)
	}
	// The deterministic shared name is the election primitive. Create avoids
	// SSA races over immutable Job templates, and no tenant owns shared proof.
	if err := r.Create(ctx, verifier); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create shared model-cache verifier: %w", err)
	}
	return nil
}

func (r *Reconciler) deleteModelCacheJob(ctx context.Context, verifier *batchv1.Job) error {
	// A delayed consumer of the previous proof must never delete its newer
	// replacement, which intentionally has the same deterministic name.
	preconditions := &metav1.Preconditions{}
	if verifier.UID != "" {
		uid := verifier.UID
		preconditions.UID = &uid
	}
	if verifier.ResourceVersion != "" {
		version := verifier.ResourceVersion
		preconditions.ResourceVersion = &version
	}
	// Job deletion can default to orphaning its Pods. Periodic proof renewal
	// must collect the old completed Pods as well as the Job, otherwise every
	// five-minute verification leaves permanent objects in the tenant namespace.
	propagation := metav1.DeletePropagationBackground
	err := r.Delete(ctx, verifier, &client.DeleteOptions{
		Preconditions: preconditions, PropagationPolicy: &propagation,
	})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}
