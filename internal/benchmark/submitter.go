package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type KubernetesJobSubmitterOptions struct {
	Image            string
	CallbackBaseURL  string
	ResultsClaimName string
	Authority        ExecutionAuthority
}

// KubernetesJobSubmitter creates deterministic per-run credentials,
// configuration, and a Job. Create collisions are accepted only when the
// existing object belongs to the same run and contains the same immutable
// input, so retries cannot accidentally adopt an operator-created object.
type KubernetesJobSubmitter struct {
	client  client.Client
	tokens  *CallbackTokens
	options KubernetesJobSubmitterOptions
}

func NewKubernetesJobSubmitter(kubeClient client.Client, tokens *CallbackTokens, options KubernetesJobSubmitterOptions) (*KubernetesJobSubmitter, error) {
	if kubeClient == nil || tokens == nil {
		return nil, errors.New("Kubernetes client and callback token issuer are required")
	}
	if strings.TrimSpace(options.Image) == "" {
		return nil, errors.New("benchmark runner image is required")
	}
	options.Authority.RunnerImage = options.Image
	parsed, err := url.Parse(options.CallbackBaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("benchmark callback base URL must be an absolute HTTP(S) URL")
	}
	return &KubernetesJobSubmitter{client: kubeClient, tokens: tokens, options: options}, nil
}

// Prepare replaces all caller-controlled target and serving identity fields
// with the frozen revision and operator-owned execution environment. Scheduler
// persistence makes this snapshot immutable before any Job can start.
func (s *KubernetesJobSubmitter) Prepare(run Run) (Run, error) {
	return s.options.Authority.Prepare(run)
}

func (s *KubernetesJobSubmitter) Submit(ctx context.Context, run Run) error {
	if run.ID == "" || run.Namespace == "" || !json.Valid(run.Configuration) {
		return errors.New("benchmark run ID, tenant namespace, and valid scenario configuration are required")
	}
	prepared, err := s.Prepare(run)
	if err != nil {
		return err
	}
	run = prepared
	token, err := s.tokens.Issue(run.ID)
	if err != nil {
		return err
	}
	baseName := "benchmark-" + run.ID
	labels := map[string]string{
		"app.kubernetes.io/name":      "inferscale-benchmark",
		"inferscale.io/benchmark-run": run.ID,
	}
	immutable := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: baseName + "-callback", Namespace: run.Namespace, Labels: labels,
			Annotations: map[string]string{"inferscale.io/execution-digest": run.ExecutionDigest},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"token":         []byte(token),
			"scenario.json": append([]byte(nil), run.ExecutionConfiguration...),
		},
	}
	if err := s.createSecret(ctx, secret, run.ID); err != nil {
		return fmt.Errorf("create benchmark callback Secret: %w", err)
	}
	callbackURL, err := url.JoinPath(s.options.CallbackBaseURL, "internal", "v1", "benchmarks", run.ID, "result")
	if err != nil {
		return fmt.Errorf("build benchmark callback URL: %w", err)
	}
	job, err := BuildJob(run, JobOptions{
		Namespace:        run.Namespace,
		Image:            s.options.Image,
		CallbackURL:      callbackURL,
		CallbackSecret:   secret.Name,
		ScenarioSecret:   secret.Name,
		ResultsClaimName: s.options.ResultsClaimName,
	})
	if err != nil {
		return err
	}
	actualJob := job
	if err := s.client.Create(ctx, job); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create benchmark Job: %w", err)
		}
		var existing batchv1.Job
		if getErr := s.client.Get(ctx, client.ObjectKeyFromObject(job), &existing); getErr != nil {
			return fmt.Errorf("read existing benchmark Job: %w", getErr)
		}
		if existing.Labels["inferscale.io/benchmark-run"] != run.ID ||
			existing.Annotations["inferscale.io/execution-digest"] != run.ExecutionDigest {
			return fmt.Errorf("benchmark Job %s/%s is owned by another run", run.Namespace, job.Name)
		}
		if existing.Status.Failed > 0 {
			return fmt.Errorf("benchmark Job %s/%s failed before reporting a result", run.Namespace, job.Name)
		}
		if existing.Status.Succeeded > 0 {
			return fmt.Errorf("benchmark Job %s/%s completed without reporting a result", run.Namespace, job.Name)
		}
		actualJob = &existing
	}
	if err := s.adoptSecret(ctx, secret, actualJob); err != nil {
		return fmt.Errorf("attach benchmark Secret to Job: %w", err)
	}
	return nil
}

func (s *KubernetesJobSubmitter) createSecret(ctx context.Context, desired *corev1.Secret, runID string) error {
	if err := s.client.Create(ctx, desired); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return err
	}
	var existing corev1.Secret
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(desired), &existing); err != nil {
		return err
	}
	if existing.Labels["inferscale.io/benchmark-run"] != runID ||
		existing.Annotations["inferscale.io/execution-digest"] != desired.Annotations["inferscale.io/execution-digest"] ||
		!bytes.Equal(existing.Data["token"], desired.Data["token"]) ||
		!bytes.Equal(existing.Data["scenario.json"], desired.Data["scenario.json"]) {
		return fmt.Errorf("Secret %s/%s does not match run", desired.Namespace, desired.Name)
	}
	return nil
}

func (s *KubernetesJobSubmitter) adoptSecret(ctx context.Context, desired *corev1.Secret, job *batchv1.Job) error {
	var existing corev1.Secret
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(desired), &existing); err != nil {
		return err
	}
	for _, owner := range existing.OwnerReferences {
		if owner.UID == job.UID && owner.Kind == "Job" {
			return nil
		}
	}
	existing.OwnerReferences = append(existing.OwnerReferences, metav1.OwnerReference{
		APIVersion: batchv1.SchemeGroupVersion.String(),
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
	})
	return s.client.Update(ctx, &existing)
}
