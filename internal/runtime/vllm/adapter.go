package vllm

import (
	"context"
	"fmt"
	"strconv"

	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() platformruntime.Backend { return platformruntime.BackendVLLM }

func (*Adapter) Capabilities() platformruntime.RuntimeCapabilities {
	return platformruntime.RuntimeCapabilities{
		RequiresModelCache: true,
		PrefixCache:        true, KVEventReporting: true, CacheIsolation: true,
		ContinuousBatching: true, BF16: true, FP8: true, TensorParallelism: true,
	}
}

func (a *Adapter) Validate(spec platformruntime.Spec) error {
	if err := platformruntime.ValidateCommon(spec); err != nil {
		return err
	}
	if spec.ResolvedBackend != platformruntime.BackendVLLM {
		return fmt.Errorf("vLLM adapter cannot render backend %q", spec.ResolvedBackend)
	}
	if spec.RoutingPolicy == "prefix-aware" && (!spec.PrefixCaching || !a.Capabilities().KVEventReporting) {
		return fmt.Errorf("prefix-aware routing requires prefix caching and KV-event reporting")
	}
	return nil
}

func (a *Adapter) Render(_ context.Context, render platformruntime.RenderContext) (platformruntime.Resources, error) {
	if err := a.Validate(render.Spec); err != nil {
		return platformruntime.Resources{}, err
	}
	if render.Images.VLLM == "" {
		return platformruntime.Resources{}, fmt.Errorf("vLLM image is required")
	}
	if render.ModelPath == "" {
		return platformruntime.Resources{}, fmt.Errorf("completed model cache path is required")
	}
	if render.Spec.Tracing && render.OTLPEndpoint == "" {
		return platformruntime.Resources{}, fmt.Errorf("vLLM tracing requires an OTLP endpoint")
	}

	labels := kubeutil.Labels(
		kubeutil.ResourceName(render.Spec.DeploymentName), render.Revision.Name,
		string(platformruntime.BackendVLLM), kubeutil.ResourceName(render.Spec.ModelName), kubeutil.ResourceName(render.Spec.Tenant),
	)
	labels[kubeutil.LabelComponent] = "model-server"
	workloadName := kubeutil.ResourceName(render.Revision.Name, "vllm")
	serviceName := kubeutil.ResourceName(render.Revision.Name, "runtime")
	gpu := resource.MustParse(strconv.FormatInt(int64(render.Spec.AcceleratorCount), 10))
	env, ports := serveEnvironment(render)
	allowPrivilegeEscalation := false
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: workloadName, Namespace: render.Spec.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:            render.ServiceAccountName,
					ImagePullSecrets:              platformruntime.PullSecrets(render.ImagePullSecret),
					NodeSelector:                  render.GPUNodeSelector,
					TerminationGracePeriodSeconds: platformruntime.Int64Pointer(platformruntime.WorkerTerminationGracePeriodSeconds),
					Containers: []corev1.Container{{
						Name: "runtime", Image: render.Images.VLLM, ImagePullPolicy: corev1.PullIfNotPresent,
						Env: env, Ports: ports,
						Lifecycle: platformruntime.WorkerDrainLifecycle(),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): gpu},
							Limits:   corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): gpu},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "model", MountPath: "/models/current", ReadOnly: true},
							{Name: "tmp", MountPath: "/tmp"},
							platformruntime.WorkerSharedMemoryMount(),
						},
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &allowPrivilegeEscalation},
						StartupProbe:    httpProbe("/health", 120, 5),
						ReadinessProbe:  httpProbe("/health", 3, 5),
						LivenessProbe:   httpProbe("/health", 3, 10),
					}},
					Volumes: []corev1.Volume{
						{Name: "model", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: render.ModelPath, Type: platformruntime.HostPathTypePointer(corev1.HostPathDirectory)}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						platformruntime.WorkerSharedMemoryVolume(render.Spec.AcceleratorCount),
					},
				},
			},
		},
	}
	service := platformruntime.RuntimeService(render.Spec.Namespace, serviceName, labels)
	return platformruntime.Resources{
		Serving: []client.Object{deployment, service}, WorkloadName: workloadName, ServiceName: serviceName,
	}, nil
}

func serveEnvironment(render platformruntime.RenderContext) ([]corev1.EnvVar, []corev1.ContainerPort) {
	precision := render.Spec.Precision
	quantization := render.Spec.Quantization
	if quantization == "" {
		quantization = "none"
	}
	port := render.KVEventsPort
	if port == 0 {
		port = 5557
	}
	env := []corev1.EnvVar{
		{Name: "MODEL_PATH", Value: "/models/current"},
		{Name: "SERVED_MODEL_NAME", Value: render.Spec.DeploymentName},
		{Name: "MODEL_REVISION", Value: render.Spec.ModelRevision},
		{Name: "PRECISION", Value: precision},
		{Name: "QUANTIZATION", Value: quantization},
		{Name: "TENSOR_PARALLELISM", Value: strconv.Itoa(int(render.Spec.TensorParallel))},
		{Name: "MAX_MODEL_LEN", Value: strconv.Itoa(int(render.Spec.MaxModelLen))},
		{Name: "PREFIX_CACHING", Value: strconv.FormatBool(render.Spec.PrefixCaching)},
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "KV_EVENTS_PORT", Value: strconv.Itoa(int(port))},
		{Name: "TRACING_ENABLED", Value: strconv.FormatBool(render.Spec.Tracing)},
	}
	if render.Spec.Tracing {
		env = append(env,
			corev1.EnvVar{Name: "OTLP_TRACES_ENDPOINT", Value: render.OTLPEndpoint},
			corev1.EnvVar{Name: "OTEL_SERVICE_NAME", Value: "inferscale-vllm"},
		)
	}
	ports := []corev1.ContainerPort{{Name: "http", ContainerPort: platformruntime.ServingPort, Protocol: corev1.ProtocolTCP}}
	if render.Spec.RoutingPolicy == "prefix-aware" {
		env = append(env,
			corev1.EnvVar{Name: "KV_EVENTS_ENABLED", Value: "true"},
			corev1.EnvVar{Name: "KV_EVENTS_MODEL", Value: render.Spec.DeploymentName},
		)
		ports = append(ports, corev1.ContainerPort{Name: "kv-events", ContainerPort: port, Protocol: corev1.ProtocolTCP})
	} else {
		env = append(env, corev1.EnvVar{Name: "KV_EVENTS_ENABLED", Value: "false"})
	}
	return env, ports
}

func httpProbe(path string, failureThreshold, periodSeconds int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromString("http")}},
		FailureThreshold: failureThreshold,
		PeriodSeconds:    periodSeconds,
		TimeoutSeconds:   2,
	}
}
