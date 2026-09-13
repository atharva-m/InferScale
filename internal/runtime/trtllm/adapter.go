package trtllm

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"

	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Adapter struct{}

const metricsExporterPort = int32(9000)

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() platformruntime.Backend { return platformruntime.BackendTRTLLM }

func (*Adapter) Capabilities() platformruntime.RuntimeCapabilities {
	return platformruntime.RuntimeCapabilities{
		RequiresModelCache: true,
		PrefixCache:        true, CacheIsolation: true, ContinuousBatching: true,
		BF16: true, FP8: false, TensorParallelism: true,
		// Precise KVEventReporting is intentionally false until a pinned
		// TensorRT-LLM/llm-d integration demonstrates the event contract.
	}
}

func (a *Adapter) Validate(spec platformruntime.Spec) error {
	if err := platformruntime.ValidateCommon(spec); err != nil {
		return err
	}
	if spec.ResolvedBackend != platformruntime.BackendTRTLLM {
		return fmt.Errorf("TensorRT-LLM adapter cannot render backend %q", spec.ResolvedBackend)
	}
	if spec.RoutingPolicy == "prefix-aware" {
		return fmt.Errorf("precise prefix-aware routing is not supported by the TensorRT-LLM v1 adapter")
	}
	if spec.Precision != "bf16" || (spec.Quantization != "" && spec.Quantization != "none") {
		return fmt.Errorf("the pinned TensorRT-LLM engine backend supports only bf16 with quantization=none")
	}
	return nil
}

func (a *Adapter) Render(_ context.Context, render platformruntime.RenderContext) (platformruntime.Resources, error) {
	if err := a.Validate(render.Spec); err != nil {
		return platformruntime.Resources{}, err
	}
	if render.Images.TensorRTLLM == "" || render.Images.TensorRTBuild == "" {
		return platformruntime.Resources{}, fmt.Errorf("TensorRT-LLM serving and engine-build images are required")
	}
	if render.ModelPath == "" || render.EngineCacheHostPath == "" {
		return platformruntime.Resources{}, fmt.Errorf("completed model path and engine cache root are required")
	}
	engineRoot, enginePath, err := engineCachePath(render)
	if err != nil {
		return platformruntime.Resources{}, err
	}
	if runtimeImageDigest(render.Spec.RuntimeVersion) == "" {
		return platformruntime.Resources{}, fmt.Errorf("TensorRT-LLM runtime image must be digest-pinned")
	}

	labels := kubeutil.Labels(
		kubeutil.ResourceName(render.Spec.DeploymentName), render.Revision.Name,
		string(platformruntime.BackendTRTLLM), kubeutil.ResourceName(render.Spec.ModelName), kubeutil.ResourceName(render.Spec.Tenant),
	)
	labels[kubeutil.LabelComponent] = "model-server"
	workloadName := kubeutil.ResourceName(render.Revision.Name, "trtllm")
	serviceName := kubeutil.ResourceName(render.Revision.Name, "runtime")
	gpu := resource.MustParse(strconv.FormatInt(int64(render.Spec.AcceleratorCount), 10))
	buildJob := engineBuildJob(render, labels, gpu, engineRoot, enginePath)
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
					Containers: []corev1.Container{
						{
							Name: "runtime", Image: render.Images.TensorRTLLM, ImagePullPolicy: corev1.PullIfNotPresent,
							Env:       serveEnvironment(render),
							Ports:     []corev1.ContainerPort{{Name: "http", ContainerPort: platformruntime.ServingPort, Protocol: corev1.ProtocolTCP}},
							Lifecycle: platformruntime.WorkerDrainLifecycle(),
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): gpu},
								Limits:   corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): gpu},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "model", MountPath: "/models/current", ReadOnly: true},
								{Name: "engine", MountPath: "/engines/current", ReadOnly: true},
								{Name: "tmp", MountPath: "/tmp"},
							},
							SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &allowPrivilegeEscalation},
							StartupProbe:    probe("/health", 120, 5),
							ReadinessProbe:  probe("/health", 3, 5),
							LivenessProbe:   probe("/health", 3, 10),
						},
						{
							Name:            "metrics-exporter",
							Image:           render.Images.TensorRTLLM,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"python3", "/opt/inferscale/metrics_exporter.py"},
							Env: []corev1.EnvVar{
								{Name: "TRTLLM_METRICS_URL", Value: "http://127.0.0.1:8000/metrics"},
								{Name: "METRICS_LISTEN_PORT", Value: strconv.Itoa(int(metricsExporterPort))},
							},
							Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: metricsExporterPort, Protocol: corev1.ProtocolTCP}},
							SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &allowPrivilegeEscalation},
							ReadinessProbe:  metricsProbe(),
							LivenessProbe:   metricsProbe(),
						},
					},
					Volumes: []corev1.Volume{
						{Name: "model", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: render.ModelPath, Type: platformruntime.HostPathTypePointer(corev1.HostPathDirectory)}}},
						{Name: "engine", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: enginePath, Type: platformruntime.HostPathTypePointer(corev1.HostPathDirectory)}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
	service := platformruntime.RuntimeService(render.Spec.Namespace, serviceName, labels)
	service.Spec.Ports = append(service.Spec.Ports, corev1.ServicePort{
		Name: "metrics", Port: metricsExporterPort, TargetPort: intstr.FromString("metrics"), Protocol: corev1.ProtocolTCP,
	})
	return platformruntime.Resources{
		Prerequisites: []client.Object{buildJob},
		Serving:       []client.Object{deployment, service},
		WorkloadName:  workloadName,
		ServiceName:   serviceName,
	}, nil
}

func engineBuildJob(render platformruntime.RenderContext, labels map[string]string, gpu resource.Quantity, engineRoot, enginePath string) *batchv1.Job {
	jobLabels := kubeutil.CopyLabels(labels)
	jobLabels[kubeutil.LabelComponent] = "engine-builder"
	backoffLimit := int32(2)
	automountServiceAccountToken := false
	args := []string{
		"--model", "/models/current", "--output", filepath.Join("/engines", filepath.Base(enginePath)),
		"--precision", render.Spec.Precision,
		"--tensor-parallelism", strconv.Itoa(int(render.Spec.TensorParallel)),
		"--max-model-len", strconv.Itoa(int(render.Spec.MaxModelLen)),
		"--model-revision", render.Spec.ModelRevision,
		"--runtime-version", render.Spec.RuntimeVersion,
		"--runtime-image-digest", runtimeImageDigest(render.Spec.RuntimeVersion),
		"--gpu-architecture", render.Spec.AcceleratorType,
	}
	if render.Spec.Quantization != "" {
		args = append(args, "--quantization", render.Spec.Quantization)
	}
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: kubeutil.ResourceName(render.Revision.Name, "engine"), Namespace: render.Spec.Namespace, Labels: jobLabels},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: jobLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &automountServiceAccountToken,
					ImagePullSecrets:             platformruntime.PullSecrets(render.ImagePullSecret),
					NodeSelector:                 render.GPUNodeSelector,
					ServiceAccountName:           render.ServiceAccountName,
					Containers: []corev1.Container{{
						Name: "builder", Image: render.Images.TensorRTBuild,
						Command: []string{"/opt/inferscale/build-engine"}, Args: args,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): gpu},
							Limits:   corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): gpu},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "model", MountPath: "/models/current", ReadOnly: true},
							// The builder atomically publishes a keyed sibling beneath
							// /engines. Mount the host cache root at that exact parent;
							// mounting it at /engines/current would leave the requested
							// /engines/<key> output in the ephemeral container layer.
							{Name: "engine", MountPath: "/engines"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "model", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: render.ModelPath, Type: platformruntime.HostPathTypePointer(corev1.HostPathDirectory)}}},
						{Name: "engine", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: engineRoot, Type: platformruntime.HostPathTypePointer(corev1.HostPathDirectoryOrCreate)}}},
					},
				},
			},
		},
	}
}

func serveEnvironment(render platformruntime.RenderContext) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "ENGINE_PATH", Value: "/engines/current"},
		{Name: "TOKENIZER_PATH", Value: "/models/current"},
		{Name: "SERVED_MODEL_NAME", Value: render.Spec.DeploymentName},
		{Name: "MODEL_REVISION", Value: render.Spec.ModelRevision},
		{Name: "TP_SIZE", Value: strconv.Itoa(int(render.Spec.TensorParallel))},
		{Name: "PRECISION", Value: render.Spec.Precision},
		{Name: "QUANTIZATION", Value: normalizedQuantization(render.Spec.Quantization)},
		{Name: "MAX_MODEL_LEN", Value: strconv.Itoa(int(render.Spec.MaxModelLen))},
		{Name: "RUNTIME_VERSION", Value: render.Spec.RuntimeVersion},
		{Name: "RUNTIME_IMAGE_DIGEST", Value: runtimeImageDigest(render.Spec.RuntimeVersion)},
		{Name: "GPU_ARCHITECTURE", Value: render.Spec.AcceleratorType},
		{Name: "PORT", Value: strconv.Itoa(int(platformruntime.ServingPort))},
	}
}

func probe(path string, failureThreshold, periodSeconds int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromString("http")}},
		FailureThreshold: failureThreshold,
		PeriodSeconds:    periodSeconds,
		TimeoutSeconds:   2,
	}
}

func metricsProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:   corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("metrics")}},
		PeriodSeconds:  10,
		TimeoutSeconds: 2,
	}
}
