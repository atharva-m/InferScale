// Package fake renders a CPU-only OpenAI-compatible worker for the local-wsl
// control-plane acceptance path. It is registered only when the explicit
// development feature is enabled and is never a selectable public backend.
package fake

import (
	"context"
	"fmt"

	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

// Name intentionally replaces the vLLM adapter in an explicitly configured
// local controller. The CR/API surface therefore remains identical to the real
// M2→M3 path and no fake backend can leak into the public contract.
func (*Adapter) Name() platformruntime.Backend { return platformruntime.BackendVLLM }

func (*Adapter) Capabilities() platformruntime.RuntimeCapabilities {
	return platformruntime.RuntimeCapabilities{
		ContinuousBatching: true,
		BF16:               true,
		TensorParallelism:  true,
	}
}

func (*Adapter) Validate(spec platformruntime.Spec) error {
	if err := platformruntime.ValidateCommon(spec); err != nil {
		return err
	}
	if spec.ResolvedBackend != platformruntime.BackendVLLM {
		return fmt.Errorf("local fake adapter cannot render backend %q", spec.ResolvedBackend)
	}
	if spec.RoutingPolicy == "prefix-aware" {
		return fmt.Errorf("local fake runtime does not support prefix-aware routing")
	}
	return nil
}

func (a *Adapter) Render(_ context.Context, render platformruntime.RenderContext) (platformruntime.Resources, error) {
	if err := a.Validate(render.Spec); err != nil {
		return platformruntime.Resources{}, err
	}
	if render.Images.VLLM == "" {
		return platformruntime.Resources{}, fmt.Errorf("local fake runtime image is required")
	}
	labels := kubeutil.Labels(
		kubeutil.ResourceName(render.Spec.DeploymentName), render.Revision.Name,
		string(platformruntime.BackendVLLM), kubeutil.ResourceName(render.Spec.ModelName), kubeutil.ResourceName(render.Spec.Tenant),
	)
	labels[kubeutil.LabelComponent] = "model-server"
	// Keep the canonical vLLM workload name so controller retirement and drift
	// repair exercise exactly the same object identity as the real adapter.
	workloadName := kubeutil.ResourceName(render.Revision.Name, "vllm")
	serviceName := kubeutil.ResourceName(render.Revision.Name, "runtime")
	falseValue := false
	trueValue := true
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: workloadName, Namespace: render.Spec.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:            render.ServiceAccountName,
					AutomountServiceAccountToken:  &falseValue,
					ImagePullSecrets:              platformruntime.PullSecrets(render.ImagePullSecret),
					TerminationGracePeriodSeconds: platformruntime.Int64Pointer(15),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &trueValue,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name: "runtime", Image: render.Images.VLLM, ImagePullPolicy: corev1.PullIfNotPresent,
						Env:            []corev1.EnvVar{{Name: "SERVED_MODEL_NAME", Value: render.Spec.DeploymentName}},
						Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: platformruntime.ServingPort}},
						ReadinessProbe: fakeProbe("/health"), LivenessProbe: fakeProbe("/health"),
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &falseValue, ReadOnlyRootFilesystem: &trueValue,
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
	service := platformruntime.RuntimeService(render.Spec.Namespace, serviceName, labels)
	return platformruntime.Resources{
		Serving: []client.Object{deployment, service}, WorkloadName: workloadName, ServiceName: serviceName,
	}, nil
}

func fakeProbe(path string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: path, Port: intstr.FromString("http"),
		}},
		PeriodSeconds: 2, TimeoutSeconds: 1, FailureThreshold: 3,
	}
}
