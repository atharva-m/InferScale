package deployment

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/controller/modelcache"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	annotationCacheRepairCacheKey    = "inferscale.io/cache-repair-cache-key"
	annotationCacheRepairEpoch       = "inferscale.io/cache-repair-epoch"
	annotationCacheRepairState       = "inferscale.io/cache-repair-state"
	annotationCacheVerificationEpoch = "inferscale.io/cache-verification-epoch"
	annotationCacheRepairModelURI    = "inferscale.io/cache-repair-model-uri"
	annotationCacheRepairRevision    = "inferscale.io/cache-repair-model-revision"
	cacheRepairStateActive           = "repairing"
	cacheRepairStateComplete         = "complete"
	cacheRepairLeaseDuration         = 60 * time.Second
	cacheRepairLeaseRenewInterval    = 15 * time.Second
	cacheRepairLeaseMutationAttempts = 5
)

type cacheRepairRole uint8

const (
	cacheRepairMissing cacheRepairRole = iota
	cacheRepairOwner
	cacheRepairFollower
	cacheRepairComplete
	cacheRepairStaleVerification
)

// cacheRepairLeaseState is intentionally durable after a repair completes.
// The completed epoch lets a reconcile that was delayed before invalidation
// distinguish its old verifier from a verifier created against the repaired
// bytes. Without that tombstone, a late contender could acquire a newly empty
// lock and cause a second hold/release outage flap.
func (r *Reconciler) cacheRepairLeaseState(ctx context.Context, cacheKey string) (cacheRepairRole, string, error) {
	lease := &coordinationv1.Lease{}
	err := r.Get(ctx, r.cacheRepairLeaseKey(cacheKey), lease)
	if apierrors.IsNotFound(err) {
		return cacheRepairMissing, "", nil
	}
	if err != nil {
		return cacheRepairMissing, "", fmt.Errorf("get model-cache repair Lease: %w", err)
	}
	if err := validateCacheRepairLease(lease, cacheKey); err != nil {
		return cacheRepairMissing, "", err
	}
	epoch := lease.Annotations[annotationCacheRepairEpoch]
	switch lease.Annotations[annotationCacheRepairState] {
	case cacheRepairStateActive:
		return cacheRepairFollower, epoch, nil
	case cacheRepairStateComplete:
		return cacheRepairComplete, epoch, nil
	default:
		return cacheRepairMissing, "", fmt.Errorf("model-cache repair Lease %s/%s has invalid state %q", lease.Namespace, lease.Name, lease.Annotations[annotationCacheRepairState])
	}
}

// joinCacheRepair returns the sole active owner, electing this resource only
// when the previous owner is gone/expired. allowCreate is used to migrate a
// legacy cache hold/status that predates the Lease protocol.
func (r *Reconciler) joinCacheRepair(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	cacheKey string,
	now time.Time,
	allowCreate bool,
) (cacheRepairRole, string, error) {
	for attempt := 0; attempt < cacheRepairLeaseMutationAttempts; attempt++ {
		lease := &coordinationv1.Lease{}
		err := r.Get(ctx, r.cacheRepairLeaseKey(cacheKey), lease)
		if apierrors.IsNotFound(err) {
			if !allowCreate {
				return cacheRepairMissing, "", nil
			}
			epoch := "1"
			lease = r.newCacheRepairLease(resource, cacheKey, epoch, now)
			if err := r.Create(ctx, lease); err != nil {
				if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
					continue
				}
				return cacheRepairMissing, "", fmt.Errorf("create model-cache repair Lease: %w", err)
			}
			return cacheRepairOwner, epoch, nil
		}
		if err != nil {
			return cacheRepairMissing, "", fmt.Errorf("get model-cache repair Lease: %w", err)
		}
		if err := validateCacheRepairLease(lease, cacheKey); err != nil {
			return cacheRepairMissing, "", err
		}
		epoch := lease.Annotations[annotationCacheRepairEpoch]
		if lease.Annotations[annotationCacheRepairState] == cacheRepairStateComplete {
			return cacheRepairComplete, epoch, nil
		}
		if cacheRepairHolder(resource) == dereferenceString(lease.Spec.HolderIdentity) {
			if err := r.renewCacheRepairLease(ctx, lease, now); err != nil {
				if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
					continue
				}
				return cacheRepairMissing, "", err
			}
			return cacheRepairOwner, epoch, nil
		}
		active, err := r.cacheRepairOwnerActive(ctx, lease, now)
		if err != nil {
			return cacheRepairMissing, "", err
		}
		if active {
			return cacheRepairFollower, epoch, nil
		}
		claimCacheRepairLease(lease, resource, now, true)
		if err := r.Update(ctx, lease); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				continue
			}
			return cacheRepairMissing, "", fmt.Errorf("take over model-cache repair Lease: %w", err)
		}
		return cacheRepairOwner, epoch, nil
	}
	return cacheRepairMissing, "", fmt.Errorf("model-cache repair Lease changed during every ownership attempt")
}

// beginCacheRepair starts a new epoch only if the failed verifier was created
// against the latest completed epoch. A verifier from an older epoch is stale
// evidence and must be discarded without holding workers again.
func (r *Reconciler) beginCacheRepair(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	cacheKey, verificationEpoch string,
	now time.Time,
) (cacheRepairRole, string, error) {
	for attempt := 0; attempt < cacheRepairLeaseMutationAttempts; attempt++ {
		lease := &coordinationv1.Lease{}
		err := r.Get(ctx, r.cacheRepairLeaseKey(cacheKey), lease)
		if apierrors.IsNotFound(err) {
			epochNumber := int64(1)
			if prior, parseErr := strconv.ParseInt(verificationEpoch, 10, 64); parseErr == nil && prior >= epochNumber {
				epochNumber = prior + 1
			}
			epoch := strconv.FormatInt(epochNumber, 10)
			lease = r.newCacheRepairLease(resource, cacheKey, epoch, now)
			if err := r.Create(ctx, lease); err != nil {
				if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
					continue
				}
				return cacheRepairMissing, "", fmt.Errorf("create model-cache repair Lease: %w", err)
			}
			return cacheRepairOwner, epoch, nil
		}
		if err != nil {
			return cacheRepairMissing, "", fmt.Errorf("get model-cache repair Lease: %w", err)
		}
		if err := validateCacheRepairLease(lease, cacheKey); err != nil {
			return cacheRepairMissing, "", err
		}
		epoch := lease.Annotations[annotationCacheRepairEpoch]
		if lease.Annotations[annotationCacheRepairState] == cacheRepairStateComplete {
			if verificationEpoch != epoch {
				return cacheRepairStaleVerification, epoch, nil
			}
			epochNumber, err := strconv.ParseInt(epoch, 10, 64)
			if err != nil || epochNumber < 1 {
				return cacheRepairMissing, "", fmt.Errorf("model-cache repair Lease %s/%s has invalid epoch %q", lease.Namespace, lease.Name, epoch)
			}
			nextEpoch := strconv.FormatInt(epochNumber+1, 10)
			lease.Annotations[annotationCacheRepairEpoch] = nextEpoch
			lease.Annotations[annotationCacheRepairState] = cacheRepairStateActive
			claimCacheRepairLease(lease, resource, now, true)
			if err := r.Update(ctx, lease); err != nil {
				if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
					continue
				}
				return cacheRepairMissing, "", fmt.Errorf("begin next model-cache repair epoch: %w", err)
			}
			return cacheRepairOwner, nextEpoch, nil
		}
		return r.joinCacheRepair(ctx, resource, cacheKey, now, false)
	}
	return cacheRepairMissing, "", fmt.Errorf("model-cache repair Lease changed during every invalidation attempt")
}

func (r *Reconciler) completeCacheRepair(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	cacheKey string,
	now time.Time,
) (string, error) {
	for attempt := 0; attempt < cacheRepairLeaseMutationAttempts; attempt++ {
		lease := &coordinationv1.Lease{}
		if err := r.Get(ctx, r.cacheRepairLeaseKey(cacheKey), lease); err != nil {
			return "", fmt.Errorf("get model-cache repair Lease for completion: %w", err)
		}
		if err := validateCacheRepairLease(lease, cacheKey); err != nil {
			return "", err
		}
		epoch := lease.Annotations[annotationCacheRepairEpoch]
		if lease.Annotations[annotationCacheRepairState] == cacheRepairStateComplete {
			return epoch, nil
		}
		if dereferenceString(lease.Spec.HolderIdentity) != cacheRepairHolder(resource) {
			return "", fmt.Errorf("deployment %s/%s no longer owns model-cache repair epoch %s", resource.Namespace, resource.Name, epoch)
		}
		lease.Annotations[annotationCacheRepairState] = cacheRepairStateComplete
		renew := metav1.NewMicroTime(now)
		lease.Spec.RenewTime = &renew
		if err := r.Update(ctx, lease); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return "", fmt.Errorf("complete model-cache repair Lease: %w", err)
		}
		return epoch, nil
	}
	return "", fmt.Errorf("model-cache repair Lease changed during every completion attempt")
}

func (r *Reconciler) newCacheRepairLease(
	resource *platformv1alpha1.InferenceDeployment,
	cacheKey, epoch string,
	now time.Time,
) *coordinationv1.Lease {
	durationSeconds := int32(cacheRepairLeaseDuration / time.Second)
	transitions := int32(0)
	acquire := metav1.NewMicroTime(now)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.cacheRepairLeaseKey(cacheKey).Name,
			Namespace: r.cacheRepairLeaseKey(cacheKey).Namespace,
			Labels: map[string]string{
				kubeutil.LabelManagedBy: kubeutil.ManagedByValue,
				kubeutil.LabelComponent: "model-cache-repair",
			},
			Annotations: map[string]string{
				annotationCacheRepairCacheKey: cacheKey,
				annotationCacheRepairEpoch:    epoch,
				annotationCacheRepairState:    cacheRepairStateActive,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			LeaseDurationSeconds: &durationSeconds,
			AcquireTime:          &acquire,
			RenewTime:            &acquire,
			LeaseTransitions:     &transitions,
		},
	}
	claimCacheRepairLease(lease, resource, now, false)
	// Keep the repair input durable even if the owner accepts a newer desired
	// model while this entry is drained. The hash must match the held path.
	if resource.Spec.Model.URI != "" && modelcache.CacheKey(resource.Spec.Model.URI, resource.Spec.Model.Revision) == cacheKey {
		lease.Annotations[annotationCacheRepairModelURI] = resource.Spec.Model.URI
		lease.Annotations[annotationCacheRepairRevision] = resource.Spec.Model.Revision
	}
	return lease
}

func (r *Reconciler) renewCacheRepairLease(ctx context.Context, lease *coordinationv1.Lease, now time.Time) error {
	if lease.Spec.RenewTime != nil && lease.Spec.RenewTime.Add(cacheRepairLeaseRenewInterval).After(now) {
		return nil
	}
	renew := metav1.NewMicroTime(now)
	lease.Spec.RenewTime = &renew
	if err := r.Update(ctx, lease); err != nil {
		return fmt.Errorf("renew model-cache repair Lease: %w", err)
	}
	return nil
}

func (r *Reconciler) cacheRepairOwnerActive(ctx context.Context, lease *coordinationv1.Lease, now time.Time) (bool, error) {
	duration := cacheRepairLeaseDuration
	if lease.Spec.LeaseDurationSeconds != nil && *lease.Spec.LeaseDurationSeconds > 0 {
		duration = time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	}
	if lease.Spec.RenewTime == nil || !lease.Spec.RenewTime.Add(duration).After(now) {
		return false, nil
	}
	namespace, name, uid, ok := parseCacheRepairHolder(dereferenceString(lease.Spec.HolderIdentity))
	if !ok {
		return true, nil
	}
	owner := &platformv1alpha1.InferenceDeployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, owner); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get model-cache repair owner %s/%s: %w", namespace, name, err)
	}
	return owner.UID == uid && owner.DeletionTimestamp.IsZero(), nil
}

func (r *Reconciler) cacheRepairLeaseKey(cacheKey string) client.ObjectKey {
	return client.ObjectKey{
		Namespace: r.Config.ControlNamespace,
		Name:      kubeutil.ResourceName("model-cache-repair", cacheKey),
	}
}

func validateCacheRepairLease(lease *coordinationv1.Lease, cacheKey string) error {
	if lease.Annotations[annotationCacheRepairCacheKey] != cacheKey {
		return fmt.Errorf("model-cache repair Lease %s/%s belongs to cache key %q, not %q", lease.Namespace, lease.Name, lease.Annotations[annotationCacheRepairCacheKey], cacheKey)
	}
	if epoch, err := strconv.ParseInt(lease.Annotations[annotationCacheRepairEpoch], 10, 64); err != nil || epoch < 1 {
		return fmt.Errorf("model-cache repair Lease %s/%s has invalid epoch %q", lease.Namespace, lease.Name, lease.Annotations[annotationCacheRepairEpoch])
	}
	if state := lease.Annotations[annotationCacheRepairState]; state != cacheRepairStateActive && state != cacheRepairStateComplete {
		return fmt.Errorf("model-cache repair Lease %s/%s has invalid state %q", lease.Namespace, lease.Name, state)
	}
	return nil
}

func claimCacheRepairLease(
	lease *coordinationv1.Lease,
	resource *platformv1alpha1.InferenceDeployment,
	now time.Time,
	transition bool,
) {
	holder := cacheRepairHolder(resource)
	lease.Spec.HolderIdentity = &holder
	acquire := metav1.NewMicroTime(now)
	lease.Spec.AcquireTime = &acquire
	lease.Spec.RenewTime = &acquire
	if transition {
		value := int32(1)
		if lease.Spec.LeaseTransitions != nil {
			value = *lease.Spec.LeaseTransitions + 1
		}
		lease.Spec.LeaseTransitions = &value
	}
}

func cacheRepairHolder(resource *platformv1alpha1.InferenceDeployment) string {
	return resource.Namespace + "/" + resource.Name + "/" + string(resource.UID)
}

func parseCacheRepairHolder(holder string) (string, string, types.UID, bool) {
	parts := strings.Split(holder, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], types.UID(parts[2]), true
}

func dereferenceString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
