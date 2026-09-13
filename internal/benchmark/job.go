package benchmark

import (
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const benchmarkJobDeadline = 6 * time.Hour

// The benchmark image is built with this fixed non-root identity. Secret and
// PVC volumes use the matching fsGroup so their group-readable files are
// actually accessible without weakening them to world-readable.
const benchmarkRunnerIdentity int64 = 65532

type JobOptions struct {
	Namespace        string
	Image            string
	CallbackURL      string
	CallbackSecret   string
	ScenarioSecret   string
	ResultsClaimName string
}

func BuildJob(run Run, options JobOptions) (*batchv1.Job, error) {
	if run.ID == "" || options.Namespace == "" || options.Image == "" || options.CallbackURL == "" || options.CallbackSecret == "" || options.ScenarioSecret == "" {
		return nil, fmt.Errorf("run ID, namespace, image, callback URL, and callback/scenario Secret are required")
	}
	if err := run.Execution.Validate(run.Serving); err != nil {
		return nil, err
	}
	if len(run.ExecutionConfiguration) == 0 || ConfigurationDigest(run.ExecutionConfiguration) != run.ExecutionDigest {
		return nil, fmt.Errorf("authoritative benchmark execution configuration is missing or corrupt")
	}
	if run.RentalPriceUSD <= 0 {
		return nil, fmt.Errorf("scheduled benchmark GPU hourly price must be positive")
	}
	backoffLimit := int32(0)
	activeDeadlineSeconds := int64(benchmarkJobDeadline / time.Second)
	ttlSecondsAfterFinished := int32(24 * 60 * 60)
	falseValue := false
	secretMode := int32(0440)
	fsGroupChangePolicy := corev1.FSGroupChangeOnRootMismatch
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "benchmark-" + run.ID,
			Namespace: options.Namespace,
			Annotations: map[string]string{
				"inferscale.io/execution-digest": run.ExecutionDigest,
			},
			Labels: map[string]string{
				"app.kubernetes.io/name":      "inferscale-benchmark",
				"inferscale.io/benchmark-run": run.ID,
				"inferscale.io/deployment":    run.DeploymentID,
				"inferscale.io/revision":      run.RevisionID,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			ActiveDeadlineSeconds:   &activeDeadlineSeconds,
			TTLSecondsAfterFinished: &ttlSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":      "inferscale-benchmark",
						"inferscale.io/benchmark-run": run.ID,
						"inferscale.io/deployment":    run.DeploymentID,
						"inferscale.io/revision":      run.RevisionID,
					},
					Annotations: map[string]string{"inferscale.io/execution-digest": run.ExecutionDigest},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &falseValue,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:        boolPtr(true),
						RunAsUser:           int64Ptr(benchmarkRunnerIdentity),
						RunAsGroup:          int64Ptr(benchmarkRunnerIdentity),
						FSGroup:             int64Ptr(benchmarkRunnerIdentity),
						FSGroupChangePolicy: &fsGroupChangePolicy,
						SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Volumes: []corev1.Volume{
						{Name: "scenario", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: options.ScenarioSecret, DefaultMode: &secretMode}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
					Containers: []corev1.Container{{
						Name:  "runner",
						Image: options.Image,
						Args: []string{
							"run", "--run-id", run.ID,
							"--scenario", "/config/scenario.json",
							"--callback", options.CallbackURL,
							"--deployment-id", run.DeploymentID,
							"--deployment-namespace", run.Namespace,
							"--runtime-pod-prefix", run.Execution.RuntimePodPrefix,
							"--epp-service", run.Execution.EPPService,
							"--provider", run.Execution.Provider,
							"--gpu-hourly-price", strconv.FormatFloat(run.RentalPriceUSD, 'f', -1, 64),
							"--runtime-version", run.Execution.RuntimeVersion,
							"--container-image-digest", run.Execution.RuntimeImageDigest,
							"--runner-image-digest", run.Execution.RunnerImageDigest,
							"--driver-version", run.Execution.DriverVersion,
							"--cuda-version", run.Execution.CUDAVersion,
							"--prometheus-url", run.Execution.PrometheusURL,
							"--configuration-digest", run.ExecutionDigest,
							"--selection-scenario-digest", run.Execution.SelectionScenarioDigest,
						},
						Env: []corev1.EnvVar{
							{Name: "INFERSCALE_BENCHMARK_TOKEN", ValueFrom: secretKey(options.CallbackSecret, "token")},
							{Name: "INFERSCALE_API_KEY", ValueFrom: secretKey(options.CallbackSecret, "token")},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "scenario", MountPath: "/config", ReadOnly: true},
							{Name: "tmp", MountPath: "/tmp"},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: boolPtr(false),
							ReadOnlyRootFilesystem:   boolPtr(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
	if options.ResultsClaimName != "" {
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{Name: "results", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: options.ResultsClaimName}}})
		job.Spec.Template.Spec.Containers[0].VolumeMounts = append(job.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "results", MountPath: "/results"})
	}
	return job, nil
}

func boolPtr(value bool) *bool { return &value }

func int64Ptr(value int64) *int64 { return &value }

func secretKey(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key,
	}}
}
