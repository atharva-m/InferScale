//go:build e2e

package local_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	inferscaleapi "github.com/inferscale/inferscale/internal/api"
	"github.com/inferscale/inferscale/internal/auth"
	controllerdeployment "github.com/inferscale/inferscale/internal/controller/deployment"
	"github.com/inferscale/inferscale/internal/deployment"
	deploymentsync "github.com/inferscale/inferscale/internal/sync"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestAPIOutboxCRDControllerVerticalSlice exercises the hardware-independent
// acceptance path using the same public HTTP handler, auth service, deployment
// service, durable-event worker, CRD type, and first controller reconciliation
// used by the deployable services. PostgreSQL and the API server are replaced
// by transaction-shaped in-memory fakes so the test remains deterministic in
// unit CI and does not pretend to cover live database or Kubernetes semantics.
func TestAPIOutboxCRDControllerVerticalSlice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	authRepository := &memoryAuthRepository{
		tenantID:  "0198b9b7-22f0-7c6d-8f2e-7d1f9b9f1420",
		namespace: "tenant-acceptance",
	}
	authService := auth.NewService(authRepository)
	_, rawKey, err := authService.CreateAPIKey(ctx, authRepository.tenantID, "acceptance", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	repository := newMemoryControlRepository()
	handler := inferscaleapi.NewServer(inferscaleapi.Dependencies{
		Deployments: deployment.NewService(repository),
		Auth:        authService,
	}).Handler()

	createBody := []byte(`{
		"name":"qwen-chat",
		"model":{"uri":"hf://Qwen/Qwen3-8B","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"backend":"vllm",
		"gpu":{"type":"RTX_5090","count":1},
		"max_model_len":8192,
		"max_replicas":2,
		"admission":{"maxConcurrentRequests":8,"maxQueuedRequests":16,"priorityClass":"standard"}
	}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/deployments", bytes.NewReader(createBody)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+rawKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "acceptance-create")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", response.StatusCode, recorder.Body.Bytes())
	}
	var mutation struct {
		Deployment struct {
			ID string `json:"id"`
		} `json:"deployment"`
	}
	if err := json.NewDecoder(response.Body).Decode(&mutation); err != nil {
		t.Fatal(err)
	}
	if mutation.Deployment.ID == "" {
		t.Fatal("API response omitted the public deployment ID")
	}
	if got := repository.pendingEvents(); got != 1 {
		t.Fatalf("accepted API mutation queued %d outbox events, want 1", got)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&platformv1alpha1.InferenceDeployment{}).Build()
	worker := deploymentsync.NewWorker(repository, repository, memoryKubernetesApplier{kubeClient}, nil)
	completed, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if completed != 1 || repository.pendingEvents() != 0 {
		t.Fatalf("sync completed=%d pending=%d, want 1/0", completed, repository.pendingEvents())
	}

	key := types.NamespacedName{Namespace: authRepository.namespace, Name: "qwen-chat"}
	resource := &platformv1alpha1.InferenceDeployment{}
	if err := kubeClient.Get(ctx, key, resource); err != nil {
		t.Fatalf("projected CRD not found: %v", err)
	}
	if resource.Annotations["inferscale.io/deployment-id"] != mutation.Deployment.ID {
		t.Fatalf("CRD public ID=%q, want %q", resource.Annotations["inferscale.io/deployment-id"], mutation.Deployment.ID)
	}
	if resource.Spec.Runtime.Backend != platformv1alpha1.RuntimeBackendVLLM {
		t.Fatalf("projected backend=%q", resource.Spec.Runtime.Backend)
	}

	reconciler := &controllerdeployment.Reconciler{Client: kubeClient, Scheme: scheme}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("first controller reconciliation: %v", err)
	}
	if !result.Requeue {
		t.Fatal("first reconciliation should persist the finalizer and requeue")
	}
	if err := kubeClient.Get(ctx, key, resource); err != nil {
		t.Fatal(err)
	}
	if !contains(resource.Finalizers, "platform.inferscale.io/finalizer") {
		t.Fatalf("controller finalizers=%v", resource.Finalizers)
	}
}

func TestOutboxFailureIsRetriedWithoutCompletingTheEvent(t *testing.T) {
	t.Parallel()
	repository := newMemoryControlRepository()
	value := validDeployment("retry-chat")
	operation := deployment.NewOperation(value.TenantID, value.ID, deployment.OperationCreate, "request", "retry-create", time.Now())
	if err := repository.Create(context.Background(), value, nil, operation); err != nil {
		t.Fatal(err)
	}
	applier := &failingApplier{err: errors.New("Kubernetes unavailable")}
	worker := deploymentsync.NewWorker(repository, repository, applier, nil)
	completed, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if completed != 0 || repository.pendingEvents() != 1 || applier.calls != 1 {
		t.Fatalf("completed=%d pending=%d apply_calls=%d", completed, repository.pendingEvents(), applier.calls)
	}
	if repository.lastRetry == "" {
		t.Fatal("failed apply did not retain retry evidence")
	}
}

func TestPublicAPIProblemsAlwaysCarryTheRequestID(t *testing.T) {
	t.Parallel()
	handler := inferscaleapi.NewServer(inferscaleapi.Dependencies{
		Deployments: deployment.NewService(newMemoryControlRepository()),
		Auth:        auth.NewService(&memoryAuthRepository{}),
	}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/v1/deployments", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.Bytes())
	}
	if recorder.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("content type=%q", recorder.Header().Get("Content-Type"))
	}
	var problem struct {
		Code      string `json:"code"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "unauthenticated" || problem.RequestID == "" {
		t.Fatalf("problem=%+v", problem)
	}
	if got := recorder.Header().Get("X-Request-ID"); got != problem.RequestID {
		t.Fatalf("X-Request-ID=%q body request_id=%q", got, problem.RequestID)
	}
}

type memoryAuthRepository struct {
	mu        sync.Mutex
	key       *auth.APIKey
	tenantID  string
	namespace string
}

func (r *memoryAuthRepository) CreateAPIKey(_ context.Context, key *auth.APIKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := *key
	r.key = &copy
	return nil
}

func (r *memoryAuthRepository) LookupAPIKey(_ context.Context, id string) (*auth.APIKey, *auth.Principal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.key == nil || r.key.ID != id {
		return nil, nil, auth.ErrKeyNotFound
	}
	copy := *r.key
	return &copy, &auth.Principal{
		APIKeyID: copy.ID, TenantID: r.tenantID, TenantSlug: "acceptance", TenantNamespace: r.namespace,
	}, nil
}

func (r *memoryAuthRepository) TouchAPIKey(context.Context, string, time.Time) error { return nil }
func (r *memoryAuthRepository) RevokeAPIKey(context.Context, string, string, time.Time) error {
	return nil
}

type memoryControlRepository struct {
	mu         sync.Mutex
	deployment *deployment.Deployment
	operation  *deployment.Operation
	events     []deploymentsync.Event
	nextID     int64
	lastRetry  string
}

func newMemoryControlRepository() *memoryControlRepository {
	return &memoryControlRepository{nextID: 1}
}

func (r *memoryControlRepository) Create(_ context.Context, value *deployment.Deployment, _ *deployment.Revision, operation *deployment.Operation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := *value
	r.deployment = &copy
	r.operation = operation
	r.enqueueLocked(deploymentsync.EventDeploymentUpsert, value.ID, nil)
	return nil
}

func (r *memoryControlRepository) Get(_ context.Context, tenantID, id string) (*deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deployment == nil || r.deployment.TenantID != tenantID || r.deployment.ID != id {
		return nil, deployment.ErrNotFound
	}
	copy := *r.deployment
	return &copy, nil
}

func (r *memoryControlRepository) GetByID(_ context.Context, id string) (*deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deployment == nil || r.deployment.ID != id {
		return nil, deployment.ErrNotFound
	}
	copy := *r.deployment
	return &copy, nil
}

func (r *memoryControlRepository) List(context.Context, string, deployment.ListOptions) (*deployment.ListPage, error) {
	return &deployment.ListPage{}, nil
}

func (r *memoryControlRepository) Update(_ context.Context, value *deployment.Deployment, _ *deployment.Revision, operation *deployment.Operation, _ int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := *value
	r.deployment = &copy
	r.operation = operation
	r.enqueueLocked(deploymentsync.EventDeploymentUpsert, value.ID, nil)
	return nil
}

func (r *memoryControlRepository) SoftDelete(_ context.Context, tenantID, id string, _ int64, now time.Time, operation *deployment.Operation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deployment == nil || r.deployment.TenantID != tenantID || r.deployment.ID != id {
		return deployment.ErrNotFound
	}
	r.deployment.DeletedAt = &now
	r.deployment.State = deployment.StateDeleting
	r.operation = operation
	payload, _ := json.Marshal(deploymentsync.DeleteTarget{Namespace: r.deployment.Namespace, Name: r.deployment.Name})
	r.enqueueLocked(deploymentsync.EventDeploymentDelete, id, payload)
	return nil
}

func (r *memoryControlRepository) GetOperation(_ context.Context, tenantID, id string) (*deployment.Operation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.operation == nil || r.operation.TenantID != tenantID || r.operation.ID != id {
		return nil, deployment.ErrNotFound
	}
	copy := *r.operation
	return &copy, nil
}

func (r *memoryControlRepository) FindOperationByIdempotency(_ context.Context, tenantID, key string) (*deployment.Operation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.operation == nil || r.operation.TenantID != tenantID || r.operation.IdempotencyKey != key {
		return nil, deployment.ErrNotFound
	}
	copy := *r.operation
	return &copy, nil
}

func (r *memoryControlRepository) UpdateObservedStatus(context.Context, string, platformv1alpha1.InferenceDeploymentStatus, time.Time) error {
	return nil
}

func (r *memoryControlRepository) Claim(_ context.Context, limit int, _ time.Time, _ time.Duration) ([]deploymentsync.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit > len(r.events) {
		limit = len(r.events)
	}
	return append([]deploymentsync.Event(nil), r.events[:limit]...), nil
}

func (r *memoryControlRepository) Complete(_ context.Context, id int64, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.events {
		if r.events[index].ID == id {
			r.events = append(r.events[:index], r.events[index+1:]...)
			return nil
		}
	}
	return fmt.Errorf("event %d not found", id)
}

func (r *memoryControlRepository) Retry(_ context.Context, id int64, message string, next time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.events {
		if r.events[index].ID == id {
			r.events[index].Attempts++
			r.events[index].NextAttemptAt = next
			r.lastRetry = message
			return nil
		}
	}
	return fmt.Errorf("event %d not found", id)
}

func (r *memoryControlRepository) enqueueLocked(eventType deploymentsync.EventType, aggregateID string, payload []byte) {
	r.events = append(r.events, deploymentsync.Event{
		ID: r.nextID, AggregateID: aggregateID, Type: eventType, Payload: append([]byte(nil), payload...), CreatedAt: time.Now().UTC(),
	})
	r.nextID++
}

func (r *memoryControlRepository) pendingEvents() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

type memoryKubernetesApplier struct{ client client.Client }

func (a memoryKubernetesApplier) Apply(ctx context.Context, object *platformv1alpha1.InferenceDeployment) error {
	current := &platformv1alpha1.InferenceDeployment{}
	key := client.ObjectKeyFromObject(object)
	if err := a.client.Get(ctx, key, current); err == nil {
		object.ResourceVersion = current.ResourceVersion
		return a.client.Update(ctx, object)
	}
	return a.client.Create(ctx, object)
}

func (a memoryKubernetesApplier) Delete(ctx context.Context, namespace, name string) error {
	object := &platformv1alpha1.InferenceDeployment{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	err := a.client.Delete(ctx, object)
	return client.IgnoreNotFound(err)
}

type failingApplier struct {
	err   error
	calls int
}

func (a *failingApplier) Apply(context.Context, *platformv1alpha1.InferenceDeployment) error {
	a.calls++
	return a.err
}

func (a *failingApplier) Delete(context.Context, string, string) error {
	a.calls++
	return a.err
}

func validDeployment(name string) *deployment.Deployment {
	value, _, err := deployment.New(deployment.CreateInput{
		TenantID: "tenant", Namespace: "tenant-acceptance", Name: name,
		Spec: platformv1alpha1.InferenceDeploymentSpec{
			Model: platformv1alpha1.ModelSpec{URI: "hf://Qwen/Qwen3-8B", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			Runtime: platformv1alpha1.RuntimeSpec{
				Backend: platformv1alpha1.RuntimeBackendVLLM, Precision: "bf16", Quantization: "none",
				TensorParallelism: 1, MaxModelLen: 8192, PrefixCaching: platformv1alpha1.PrefixCachingSpec{Enabled: true},
			},
			Accelerator:   platformv1alpha1.AcceleratorSpec{Vendor: "nvidia", Type: "RTX_5090", Count: 1},
			Scaling:       platformv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, Policy: "saturation"},
			Admission:     platformv1alpha1.AdmissionSpec{MaxConcurrentRequests: 8, MaxQueuedRequests: 16, PriorityClass: "standard"},
			Routing:       platformv1alpha1.RoutingSpec{Policy: platformv1alpha1.RoutingPolicyLoadAware},
			Rollout:       platformv1alpha1.RolloutSpec{Strategy: "progressive", ShadowPercent: 10},
			Observability: platformv1alpha1.ObservabilitySpec{Tracing: true},
		},
	}, time.Now())
	if err != nil {
		panic(err)
	}
	return value
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
