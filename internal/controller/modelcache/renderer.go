package modelcache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SharedVerificationNamespace permits read-only hostPath cache access without
// relaxing Pod Security enforcement for the control-plane namespace. It is
// installed by deploy/base/model-cache and denies all Pod network traffic.
const SharedVerificationNamespace = "inferscale-model-cache"

type Config struct {
	Image           string
	CacheRoot       string
	ImagePullSecret string
	HFTokenSecret   string
	NodeSelector    map[string]string
}

type Renderer struct {
	Config Config
}

func CacheKey(modelURI, immutableRevision string) string {
	sum := sha256.Sum256([]byte(modelURI + "\x00" + strings.ToLower(immutableRevision)))
	return "sha256-" + hex.EncodeToString(sum[:])
}

func (r Renderer) Path(spec platformruntime.Spec) string {
	return filepath.Join(r.Config.CacheRoot, CacheKey(spec.ModelURI, spec.ModelRevision))
}

func (r Renderer) Job(spec platformruntime.Spec, revision platformruntime.Revision) (*batchv1.Job, error) {
	if r.Config.Image == "" || r.Config.CacheRoot == "" {
		return nil, fmt.Errorf("model-cache image and cache root are required")
	}
	if spec.ModelURI == "" || spec.ModelRevision == "" {
		return nil, fmt.Errorf("model URI and immutable revision are required")
	}
	labels := kubeutil.Labels(
		kubeutil.ResourceName(spec.DeploymentName), revision.Name, string(spec.ResolvedBackend),
		kubeutil.ResourceName(spec.ModelName), kubeutil.ResourceName(spec.Tenant),
	)
	labels[kubeutil.LabelComponent] = "model-prefetch"
	backoffLimit := int32(3)
	ttl := int32(3600)
	automountServiceAccountToken := false
	container := corev1.Container{
		Name:  "prefetch",
		Image: r.Config.Image,
		Args: []string{
			"--uri", spec.ModelURI,
			"--revision", strings.ToLower(spec.ModelRevision),
			"--cache-root", "/cache",
			"--full-verification",
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}},
	}
	if r.Config.HFTokenSecret != "" {
		container.Env = append(container.Env, corev1.EnvVar{
			Name: "HF_TOKEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: r.Config.HFTokenSecret},
				Key:                  "token",
			}},
		})
	}
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: kubeutil.ResourceName(revision.Name, "prefetch"), Namespace: spec.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &automountServiceAccountToken,
					ImagePullSecrets:             platformruntime.PullSecrets(r.Config.ImagePullSecret),
					NodeSelector:                 r.Config.NodeSelector,
					Containers:                   []corev1.Container{container},
					Volumes: []corev1.Volume{{
						Name: "cache",
						VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
							Path: r.Config.CacheRoot, Type: platformruntime.HostPathTypePointer(corev1.HostPathDirectoryOrCreate),
						}},
					}},
				},
			},
		},
	}, nil
}

// VerificationJob renders a bounded, read-only proof that a previously
// completed cache entry still matches its immutable manifest. It deliberately
// has a different lifecycle from the prefetch Job: the controller retains the
// terminal status until it has consumed the result, then periodically replaces
// the Job to obtain a fresh proof.
func (r Renderer) VerificationJob(spec platformruntime.Spec, revision platformruntime.Revision) (*batchv1.Job, error) {
	if r.Config.Image == "" || r.Config.CacheRoot == "" {
		return nil, fmt.Errorf("model-cache image and cache root are required")
	}
	if spec.ModelURI == "" || spec.ModelRevision == "" {
		return nil, fmt.Errorf("model URI and immutable revision are required")
	}
	labels := kubeutil.Labels(
		kubeutil.ResourceName(spec.DeploymentName), revision.Name, string(spec.ResolvedBackend),
		kubeutil.ResourceName(spec.ModelName), kubeutil.ResourceName(spec.Tenant),
	)
	labels[kubeutil.LabelComponent] = "model-cache-verifier"
	backoffLimit := int32(0)
	activeDeadlineSeconds := int64(1800)
	automountServiceAccountToken := false
	readOnlyRootFilesystem := true
	runAsNonRoot := true
	runAsUser := int64(65532)
	allowPrivilegeEscalation := false
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: kubeutil.ResourceName(revision.Name, "cache-verify"), Namespace: spec.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &activeDeadlineSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &automountServiceAccountToken,
					ImagePullSecrets:             platformruntime.PullSecrets(r.Config.ImagePullSecret),
					NodeSelector:                 r.Config.NodeSelector,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
						RunAsUser:    &runAsUser,
						RunAsGroup:   &runAsUser,
						FSGroup:      &runAsUser,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:  "verify",
						Image: r.Config.Image,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: apiresource.MustParse("100m"), corev1.ResourceMemory: apiresource.MustParse("128Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: apiresource.MustParse("500m"), corev1.ResourceMemory: apiresource.MustParse("512Mi")},
						},
						Args: []string{
							"--uri", spec.ModelURI,
							"--revision", strings.ToLower(spec.ModelRevision),
							"--cache-root", "/cache",
							"--verify-only",
							"--full-verification",
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "cache", MountPath: "/cache", ReadOnly: true},
							{Name: "tmp", MountPath: "/tmp"},
						},
						SecurityContext: &corev1.SecurityContext{
							ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
							AllowPrivilegeEscalation: &allowPrivilegeEscalation,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "cache",
							VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
								Path: r.Config.CacheRoot, Type: platformruntime.HostPathTypePointer(corev1.HostPathDirectory),
							}},
						},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: apiresource.NewQuantity(64*1024*1024, apiresource.BinarySI)}}},
					},
				},
			},
		},
	}, nil
}

// SharedVerificationJob is a node-local proof shared by every revision and
// tenant consuming these exact bytes. It has no deployment owner or revision
// labels, so retiring one consumer cannot remove another consumer's proof.
func (r Renderer) SharedVerificationJob(spec platformruntime.Spec, nodeName, namespace string) (*batchv1.Job, error) {
	if nodeName == "" || namespace == "" {
		return nil, fmt.Errorf("shared verification requires an observed node and verification namespace")
	}
	job, err := r.VerificationJob(spec, platformruntime.Revision{})
	if err != nil {
		return nil, err
	}
	identity := sha256.Sum256([]byte(r.Path(spec) + "\x00" + nodeName))
	job.Name = "model-cache-verify-" + hex.EncodeToString(identity[:])[:40]
	job.Namespace = namespace
	labels := map[string]string{
		kubeutil.LabelManagedBy: kubeutil.ManagedByValue,
		kubeutil.LabelComponent: "model-cache-verifier",
	}
	job.Labels = labels
	job.Spec.Template.Labels = kubeutil.CopyLabels(labels)
	job.Spec.Template.Spec.NodeName = nodeName
	job.Spec.Template.Spec.NodeSelector = nil
	// Retain terminal evidence until a controller consumes it. In particular,
	// TTL cleanup during a controller outage must not erase checksum failures.
	// Live consumers replace successful proofs at the verification interval.
	return job, nil
}

func JobState(job *batchv1.Job) (complete bool, failed bool, message string) {
	for _, condition := range job.Status.Conditions {
		switch condition.Type {
		case batchv1.JobComplete:
			if condition.Status == corev1.ConditionTrue {
				return true, false, condition.Message
			}
		case batchv1.JobFailed:
			if condition.Status == corev1.ConditionTrue {
				return false, true, condition.Message
			}
		}
	}
	return false, false, ""
}
