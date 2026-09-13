package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/api/middleware"
	"github.com/inferscale/inferscale/internal/auth"
	"github.com/inferscale/inferscale/internal/deployment"
)

type apiRepository struct {
	value      *deployment.Deployment
	operations map[string]*deployment.Operation
}

type responseCache struct {
	values map[string][]byte
	ttl    time.Duration
}

type apiBenchmarkRepository struct {
	runs map[string]*deployment.BenchmarkRun
}

func (r *apiBenchmarkRepository) CreateBenchmark(_ context.Context, run *deployment.BenchmarkRun) error {
	if r.runs == nil {
		r.runs = make(map[string]*deployment.BenchmarkRun)
	}
	copy := *run
	r.runs[run.IdempotencyKey] = &copy
	return nil
}

func (r *apiBenchmarkRepository) FindBenchmarkByIdempotency(_ context.Context, tenantID, key string) (*deployment.BenchmarkRun, error) {
	run := r.runs[key]
	if run == nil || run.TenantID != tenantID {
		return nil, deployment.ErrBenchmarkNotFound
	}
	copy := *run
	return &copy, nil
}

func (r *apiBenchmarkRepository) ListBenchmarks(context.Context, string, string, deployment.ListOptions) (*deployment.BenchmarkPage, error) {
	return &deployment.BenchmarkPage{}, nil
}

func (c *responseCache) GetIdempotency(_ context.Context, tenantID, key string) ([]byte, bool, error) {
	value, ok := c.values[tenantID+":"+key]
	return append([]byte(nil), value...), ok, nil
}

func (c *responseCache) PutIdempotency(_ context.Context, tenantID, key string, value []byte, ttl time.Duration) (bool, error) {
	if c.values == nil {
		c.values = make(map[string][]byte)
	}
	cacheKey := tenantID + ":" + key
	if _, exists := c.values[cacheKey]; exists {
		return false, nil
	}
	c.values[cacheKey] = append([]byte(nil), value...)
	c.ttl = ttl
	return true, nil
}

func (r *apiRepository) Create(_ context.Context, value *deployment.Deployment, _ *deployment.Revision, operation *deployment.Operation) error {
	r.value = value
	if r.operations == nil {
		r.operations = map[string]*deployment.Operation{}
	}
	r.operations[operation.ID] = operation
	return nil
}
func (r *apiRepository) Get(_ context.Context, tenantID, id string) (*deployment.Deployment, error) {
	if r.value == nil || r.value.TenantID != tenantID || r.value.ID != id {
		return nil, deployment.ErrNotFound
	}
	copy := *r.value
	return &copy, nil
}
func (r *apiRepository) GetByID(ctx context.Context, id string) (*deployment.Deployment, error) {
	if r.value == nil {
		return nil, deployment.ErrNotFound
	}
	return r.Get(ctx, r.value.TenantID, id)
}
func (r *apiRepository) List(context.Context, string, deployment.ListOptions) (*deployment.ListPage, error) {
	return &deployment.ListPage{}, nil
}
func (r *apiRepository) Update(_ context.Context, value *deployment.Deployment, _ *deployment.Revision, operation *deployment.Operation, _ int64) error {
	r.value = value
	if r.operations == nil {
		r.operations = map[string]*deployment.Operation{}
	}
	r.operations[operation.ID] = operation
	return nil
}
func (r *apiRepository) SoftDelete(context.Context, string, string, int64, time.Time, *deployment.Operation) error {
	return nil
}
func (r *apiRepository) GetOperation(_ context.Context, _, id string) (*deployment.Operation, error) {
	value, ok := r.operations[id]
	if !ok {
		return nil, deployment.ErrNotFound
	}
	return value, nil
}
func (r *apiRepository) FindOperationByIdempotency(_ context.Context, _ string, key string) (*deployment.Operation, error) {
	for _, value := range r.operations {
		if value.IdempotencyKey == key {
			return value, nil
		}
	}
	return nil, deployment.ErrNotFound
}
func (r *apiRepository) UpdateObservedStatus(context.Context, string, platformv1alpha1.InferenceDeploymentStatus, time.Time) error {
	return nil
}

func TestCreateUsesLockedDefaultsAndReturnsETag(t *testing.T) {
	repository := &apiRepository{}
	cache := &responseCache{}
	server := NewServer(Dependencies{Deployments: deployment.NewService(repository), Idempotency: cache})
	body := []byte(`{
		"name":"qwen-chat",
		"model":{"uri":"hf://Qwen/Qwen3-8B","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"gpu":{"type":"RTX_5090","count":1},
		"max_model_len":8192,
		"max_replicas":2,
		"admission":{"maxConcurrentRequests":8,"maxQueuedRequests":16,"priorityClass":"standard"}
	}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/deployments", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-qwen")
	request = request.WithContext(middleware.WithPrincipal(request.Context(), &auth.Principal{TenantID: "tenant", TenantNamespace: "tenant-acme"}))
	recorder := httptest.NewRecorder()
	server.createDeployment(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	if recorder.Header().Get("ETag") != `"1"` {
		t.Fatalf("unexpected ETag: %s", recorder.Header().Get("ETag"))
	}
	if repository.value.Spec.Scaling.MinReplicas != 1 {
		t.Fatal("minReplicas did not default to 1")
	}
	if repository.value.Spec.Routing.Policy != platformv1alpha1.RoutingPolicyLoadAware {
		t.Fatal("routing did not default to load-aware")
	}
	firstID := repository.value.ID
	request = httptest.NewRequest(http.MethodPost, "/v1/deployments", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-qwen")
	request = request.WithContext(middleware.WithPrincipal(request.Context(), &auth.Principal{TenantID: "tenant", TenantNamespace: "tenant-acme"}))
	recorder = httptest.NewRecorder()
	server.createDeployment(recorder, request)
	if recorder.Code != http.StatusAccepted || repository.value.ID != firstID || len(repository.operations) != 1 {
		t.Fatal("idempotent create was not replayed")
	}
	if recorder.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("second create was not replayed from the 24-hour response cache")
	}
	if cache.ttl != 24*time.Hour {
		t.Fatalf("idempotency response TTL = %s", cache.ttl)
	}

	differentBody := bytes.Replace(body, []byte(`"qwen-chat"`), []byte(`"qwen-other"`), 1)
	request = httptest.NewRequest(http.MethodPost, "/v1/deployments", bytes.NewReader(differentBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-qwen")
	request = request.WithContext(middleware.WithPrincipal(request.Context(), &auth.Principal{TenantID: "tenant", TenantNamespace: "tenant-acme"}))
	recorder = httptest.NewRecorder()
	server.createDeployment(recorder, request)
	if recorder.Code != http.StatusConflict || repository.value.ID != firstID || len(repository.operations) != 1 {
		t.Fatalf("same key with a different deployment request was not rejected: status=%d body=%s", recorder.Code, recorder.Body)
	}
	var problem map[string]any
	if json.Unmarshal(recorder.Body.Bytes(), &problem) != nil || problem["code"] != "idempotency_conflict" {
		t.Fatalf("unexpected idempotency problem: %s", recorder.Body)
	}
	serverWithoutCache := NewServer(Dependencies{Deployments: deployment.NewService(repository)})
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/deployments", bytes.NewReader(differentBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-qwen")
	request = request.WithContext(middleware.WithPrincipal(request.Context(), &auth.Principal{TenantID: "tenant", TenantNamespace: "tenant-acme"}))
	serverWithoutCache.createDeployment(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("database idempotency defense did not reject a different request: status=%d body=%s", recorder.Code, recorder.Body)
	}
}

func TestBenchmarkIdempotencyBindsCanonicalRequest(t *testing.T) {
	t.Parallel()
	repository := &apiRepository{}
	value, stableRevision, _ := deployment.New(deployment.CreateInput{
		TenantID: "tenant", Namespace: "tenant-acme", Name: "qwen-chat",
		Spec: platformv1alpha1.InferenceDeploymentSpec{
			Model:       platformv1alpha1.ModelSpec{URI: "hf://Qwen/Qwen3-8B", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			Runtime:     platformv1alpha1.RuntimeSpec{Backend: platformv1alpha1.RuntimeBackendVLLM, Precision: "bf16", Quantization: "none", TensorParallelism: 1, MaxModelLen: 8192},
			Accelerator: platformv1alpha1.AcceleratorSpec{Vendor: "nvidia", Type: "RTX_5090", Count: 1},
			Scaling:     platformv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, Policy: "saturation"},
			Admission:   platformv1alpha1.AdmissionSpec{MaxConcurrentRequests: 8, MaxQueuedRequests: 16, PriorityClass: "standard"},
			Routing:     platformv1alpha1.RoutingSpec{Policy: platformv1alpha1.RoutingPolicyLoadAware},
			Rollout:     platformv1alpha1.RolloutSpec{Strategy: "progressive"},
		},
	}, time.Now())
	repository.value = value
	repository.value.State = deployment.StateReady
	repository.value.StableRevisionID = stableRevision.ID
	repository.value.CandidateRevisionID = ""
	repository.value.ObservedStatus = platformv1alpha1.InferenceDeploymentStatus{
		Phase:              platformv1alpha1.DeploymentPhaseReady,
		ObservedGeneration: repository.value.Generation,
		Revision: platformv1alpha1.RevisionStatus{
			Stable: "qwen-chat-a1b2c3d4", StableID: stableRevision.ID,
		},
	}
	benchmarkRepository := &apiBenchmarkRepository{}
	cache := &responseCache{}
	server := NewServer(Dependencies{
		Deployments: deployment.NewService(repository),
		Benchmarks:  deployment.NewBenchmarkService(repository, benchmarkRepository),
		Idempotency: cache,
	})
	invoke := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/deployments/"+repository.value.ID+"/benchmarks", bytes.NewBufferString(body))
		request.SetPathValue("id", repository.value.ID)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "benchmark-qwen")
		request = request.WithContext(middleware.WithPrincipal(request.Context(), &auth.Principal{TenantID: "tenant", TenantNamespace: "tenant-acme"}))
		recorder := httptest.NewRecorder()
		server.createBenchmark(recorder, request)
		return recorder
	}

	first := invoke(`{"scenario":"selection","configuration":{"batch":2,"seed":7},"rental_price_usd_per_gpu_hour":1.5}`)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body)
	}
	repository.value.State = deployment.StateUpdating
	repository.value.CandidateRevisionID = "candidate-revision-id"
	repository.value.ObservedStatus.Phase = platformv1alpha1.DeploymentPhaseUpdating
	replay := invoke(`{"configuration":{"seed":7,"batch":2},"rental_price_usd_per_gpu_hour":1.5,"scenario":"selection"}`)
	if replay.Code != http.StatusAccepted || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Body.String() != first.Body.String() {
		t.Fatalf("canonical benchmark request was not replayed exactly: status=%d body=%s", replay.Code, replay.Body)
	}
	conflict := invoke(`{"scenario":"selection","configuration":{"batch":3,"seed":7},"rental_price_usd_per_gpu_hour":1.5}`)
	if conflict.Code != http.StatusConflict || len(benchmarkRepository.runs) != 1 {
		t.Fatalf("different benchmark request did not conflict: status=%d body=%s", conflict.Code, conflict.Body)
	}
	var problem map[string]any
	if json.Unmarshal(conflict.Body.Bytes(), &problem) != nil || problem["code"] != "idempotency_conflict" {
		t.Fatalf("unexpected idempotency problem: %s", conflict.Body)
	}
	server = NewServer(Dependencies{
		Deployments: deployment.NewService(repository),
		Benchmarks:  deployment.NewBenchmarkService(repository, benchmarkRepository),
	})
	durableReplay := invoke(`{"configuration":{"seed":7,"batch":2},"rental_price_usd_per_gpu_hour":1.5,"scenario":"selection"}`)
	if durableReplay.Code != http.StatusAccepted || durableReplay.Body.String() != first.Body.String() {
		t.Fatalf("durable replay was revalidated against mutable deployment state: status=%d body=%s", durableReplay.Code, durableReplay.Body)
	}
	durableConflict := invoke(`{"scenario":"selection","configuration":{"batch":4,"seed":7},"rental_price_usd_per_gpu_hour":1.5}`)
	if durableConflict.Code != http.StatusConflict {
		t.Fatalf("database benchmark idempotency defense did not reject a different request: status=%d body=%s", durableConflict.Code, durableConflict.Body)
	}
}

func TestBenchmarkCreationRejectsUnreadyTargetWithoutLeakingCrossTenantState(t *testing.T) {
	t.Parallel()
	repository := &apiRepository{}
	repository.value, _, _ = deployment.New(deployment.CreateInput{
		TenantID: "tenant", Namespace: "tenant-acme", Name: "qwen-chat",
		Spec: platformv1alpha1.InferenceDeploymentSpec{
			Model:       platformv1alpha1.ModelSpec{URI: "hf://Qwen/Qwen3-8B", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			Runtime:     platformv1alpha1.RuntimeSpec{Backend: platformv1alpha1.RuntimeBackendVLLM, Precision: "bf16", Quantization: "none", TensorParallelism: 1, MaxModelLen: 8192},
			Accelerator: platformv1alpha1.AcceleratorSpec{Vendor: "nvidia", Type: "RTX_5090", Count: 1},
			Scaling:     platformv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, Policy: "saturation"},
			Admission:   platformv1alpha1.AdmissionSpec{MaxConcurrentRequests: 8, MaxQueuedRequests: 16, PriorityClass: "standard"},
			Routing:     platformv1alpha1.RoutingSpec{Policy: platformv1alpha1.RoutingPolicyLoadAware},
			Rollout:     platformv1alpha1.RolloutSpec{Strategy: "progressive"},
		},
	}, time.Now())
	benchmarkRepository := &apiBenchmarkRepository{}
	server := NewServer(Dependencies{
		Deployments: deployment.NewService(repository),
		Benchmarks:  deployment.NewBenchmarkService(repository, benchmarkRepository),
	})
	invoke := func(tenantID, key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/deployments/"+repository.value.ID+"/benchmarks", bytes.NewBufferString(`{"scenario":"selection","configuration":{"seed":7},"rental_price_usd_per_gpu_hour":1.5}`))
		request.SetPathValue("id", repository.value.ID)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		request = request.WithContext(middleware.WithPrincipal(request.Context(), &auth.Principal{TenantID: tenantID, TenantNamespace: "tenant-acme"}))
		recorder := httptest.NewRecorder()
		server.createBenchmark(recorder, request)
		return recorder
	}

	unready := invoke("tenant", "unready")
	if unready.Code != http.StatusConflict {
		t.Fatalf("unready status=%d body=%s", unready.Code, unready.Body)
	}
	var problem map[string]any
	if err := json.Unmarshal(unready.Body.Bytes(), &problem); err != nil || problem["code"] != "benchmark_target_not_ready" {
		t.Fatalf("unexpected unready problem: %s", unready.Body)
	}
	crossTenant := invoke("another-tenant", "cross-tenant")
	if crossTenant.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant status=%d body=%s", crossTenant.Code, crossTenant.Body)
	}
	if benchmarkRepository.runs != nil {
		t.Fatalf("rejected requests created runs: %#v", benchmarkRepository.runs)
	}
}

func TestPatchRequiresIfMatchAndMergePatchContentType(t *testing.T) {
	repository := &apiRepository{}
	server := NewServer(Dependencies{Deployments: deployment.NewService(repository)})
	repository.value, _, _ = deployment.New(deployment.CreateInput{
		TenantID: "tenant", Namespace: "tenant-acme", Name: "qwen-chat",
		Spec: platformv1alpha1.InferenceDeploymentSpec{
			Model:       platformv1alpha1.ModelSpec{URI: "hf://Qwen/Qwen3-8B", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			Runtime:     platformv1alpha1.RuntimeSpec{Backend: platformv1alpha1.RuntimeBackendVLLM, Precision: "bf16", Quantization: "none", TensorParallelism: 1, MaxModelLen: 8192},
			Accelerator: platformv1alpha1.AcceleratorSpec{Vendor: "nvidia", Type: "RTX_5090", Count: 1},
			Scaling:     platformv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, Policy: "saturation"},
			Admission:   platformv1alpha1.AdmissionSpec{MaxConcurrentRequests: 8, MaxQueuedRequests: 16, PriorityClass: "standard"},
			Routing:     platformv1alpha1.RoutingSpec{Policy: platformv1alpha1.RoutingPolicyLoadAware},
			Rollout:     platformv1alpha1.RolloutSpec{Strategy: "progressive"},
		},
	}, time.Now())
	request := httptest.NewRequest(http.MethodPatch, "/v1/deployments/"+repository.value.ID, bytes.NewBufferString(`{"runtime":{"backend":"auto"}}`))
	request.SetPathValue("id", repository.value.ID)
	request.Header.Set("Content-Type", "application/merge-patch+json")
	request = request.WithContext(middleware.WithPrincipal(request.Context(), &auth.Principal{TenantID: "tenant", TenantNamespace: "tenant-acme"}))
	recorder := httptest.NewRecorder()
	server.patchDeployment(recorder, request)
	if recorder.Code != http.StatusPreconditionRequired {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	if recorder.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("unexpected problem content type: %s", recorder.Header().Get("Content-Type"))
	}
	var value map[string]any
	if json.Unmarshal(recorder.Body.Bytes(), &value) != nil || value["code"] != "precondition_required" {
		t.Fatal("RFC problem response missing code")
	}

	request = httptest.NewRequest(http.MethodPatch, "/v1/deployments/"+repository.value.ID, bytes.NewBufferString(`{"runtime":{"backend":"auto"}}`))
	request.SetPathValue("id", repository.value.ID)
	request.Header.Set("Content-Type", "application/merge-patch+json")
	request.Header.Set("If-Match", `"1"`)
	request = request.WithContext(middleware.WithPrincipal(request.Context(), &auth.Principal{TenantID: "tenant", TenantNamespace: "tenant-acme"}))
	recorder = httptest.NewRecorder()
	server.patchDeployment(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	if repository.value.Spec.Runtime.Backend != platformv1alpha1.RuntimeBackendAuto || repository.value.Generation != 2 {
		t.Fatal("merge patch did not select auto and create a new generation")
	}
}

func TestBenchmarkListOptionsUsesOpaqueCursor(t *testing.T) {
	want := deployment.ListCursor{
		CreatedAt: time.Date(2026, time.August, 16, 12, 30, 0, 0, time.UTC),
		ID:        "0198b9b7-22f0-7c6d-8f2e-7d1f9b9f1420",
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/deployments/deployment/benchmarks?limit=25&cursor="+encodeCursor(want), nil)
	options, err := benchmarkListOptions(request)
	if err != nil {
		t.Fatal(err)
	}
	if options.Limit != 25 || options.Cursor == nil || options.Cursor.ID != want.ID || !options.Cursor.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("options = %#v, want limit and decoded opaque cursor", options)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/deployments/deployment/benchmarks?offset=10", nil)
	options, err = benchmarkListOptions(request)
	if err != nil {
		t.Fatal(err)
	}
	if options.Cursor != nil || options.Offset != 0 {
		t.Fatalf("legacy offset affected options: %#v", options)
	}
}
