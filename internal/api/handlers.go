package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/api/middleware"
	"github.com/inferscale/inferscale/internal/deployment"
)

type createDeploymentRequest struct {
	Name              string                              `json:"name"`
	Model             platformv1alpha1.ModelSpec          `json:"model"`
	Backend           platformv1alpha1.RuntimeBackend     `json:"backend"`
	GPU               gpuRequest                          `json:"gpu"`
	Precision         string                              `json:"precision"`
	Quantization      string                              `json:"quantization,omitempty"`
	TensorParallelism int32                               `json:"tensor_parallelism"`
	MaxModelLen       *int32                              `json:"max_model_len"`
	PrefixCaching     *bool                               `json:"prefix_caching,omitempty"`
	MinReplicas       *int32                              `json:"min_replicas,omitempty"`
	MaxReplicas       *int32                              `json:"max_replicas"`
	SLO               *sloRequest                         `json:"slo,omitempty"`
	Admission         *platformv1alpha1.AdmissionSpec     `json:"admission,omitempty"`
	Routing           *platformv1alpha1.RoutingSpec       `json:"routing,omitempty"`
	Rollout           *platformv1alpha1.RolloutSpec       `json:"rollout,omitempty"`
	Observability     *platformv1alpha1.ObservabilitySpec `json:"observability,omitempty"`
}

type gpuRequest struct {
	Type  string `json:"type"`
	Count *int32 `json:"count"`
}

type sloRequest struct {
	TTFTP95MS int64 `json:"ttft_p95_ms"`
	TPOTP95MS int64 `json:"tpot_p95_ms"`
}

func (r createDeploymentRequest) spec() platformv1alpha1.InferenceDeploymentSpec {
	backend := r.Backend
	if backend == "" {
		backend = platformv1alpha1.RuntimeBackendAuto
	}
	precision := r.Precision
	if precision == "" {
		precision = "bf16"
	}
	quantization := r.Quantization
	if quantization == "" {
		quantization = "none"
	}
	gpuCount := *r.GPU.Count
	tp := r.TensorParallelism
	if tp == 0 {
		tp = gpuCount
	}
	maxModelLen := *r.MaxModelLen
	maxReplicas := *r.MaxReplicas
	minReplicas := int32(1)
	if r.MinReplicas != nil {
		minReplicas = *r.MinReplicas
	}
	prefixCaching := true
	if r.PrefixCaching != nil {
		prefixCaching = *r.PrefixCaching
	}
	admission := *r.Admission
	routing := platformv1alpha1.RoutingSpec{Policy: platformv1alpha1.RoutingPolicyLoadAware}
	if r.Routing != nil {
		routing = *r.Routing
	}
	rollout := platformv1alpha1.RolloutSpec{Strategy: "progressive", ShadowPercent: 10}
	if r.Rollout != nil {
		rollout = *r.Rollout
	}
	observability := platformv1alpha1.ObservabilitySpec{Tracing: true}
	if r.Observability != nil {
		observability = *r.Observability
	}
	var slo *platformv1alpha1.SLOSpec
	if r.SLO != nil {
		slo = &platformv1alpha1.SLOSpec{
			TTFT: platformv1alpha1.MetricSLO{Percentile: 95, TargetMS: r.SLO.TTFTP95MS},
			TPOT: platformv1alpha1.MetricSLO{Percentile: 95, TargetMS: r.SLO.TPOTP95MS},
		}
	}
	return platformv1alpha1.InferenceDeploymentSpec{
		Model: r.Model,
		Runtime: platformv1alpha1.RuntimeSpec{
			Backend: backend, Precision: precision, Quantization: quantization,
			TensorParallelism: tp, MaxModelLen: maxModelLen,
			PrefixCaching: platformv1alpha1.PrefixCachingSpec{Enabled: prefixCaching},
		},
		Accelerator: platformv1alpha1.AcceleratorSpec{Vendor: "nvidia", Type: r.GPU.Type, Count: gpuCount},
		Scaling:     platformv1alpha1.ScalingSpec{MinReplicas: minReplicas, MaxReplicas: maxReplicas, Policy: "saturation"},
		SLO:         slo, Admission: admission, Routing: routing, Rollout: rollout, Observability: observability,
	}
}

type deploymentResponse struct {
	ID                  string                                     `json:"id"`
	Name                string                                     `json:"name"`
	Namespace           string                                     `json:"namespace"`
	Generation          int64                                      `json:"generation"`
	Spec                platformv1alpha1.InferenceDeploymentSpec   `json:"spec"`
	Status              platformv1alpha1.InferenceDeploymentStatus `json:"status"`
	StableRevisionID    string                                     `json:"stableRevisionId,omitempty"`
	CandidateRevisionID string                                     `json:"candidateRevisionId,omitempty"`
	CreatedAt           time.Time                                  `json:"createdAt"`
	UpdatedAt           time.Time                                  `json:"updatedAt"`
}

type mutationResponse struct {
	Deployment deploymentResponse    `json:"deployment"`
	Operation  *deployment.Operation `json:"operation"`
}

func deploymentDTO(value *deployment.Deployment) deploymentResponse {
	return deploymentResponse{
		ID: value.ID, Name: value.Name, Namespace: value.Namespace, Generation: value.Generation,
		Spec: value.Spec, Status: value.ObservedStatus, StableRevisionID: value.StableRevisionID,
		CandidateRevisionID: value.CandidateRevisionID, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	principal, _ := middleware.Principal(r.Context())
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		s.writeError(w, r, fmt.Errorf("%w: Idempotency-Key header is required", deployment.ErrInvalid))
		return
	}
	if len(idempotencyKey) > 200 {
		s.writeError(w, r, fmt.Errorf("%w: Idempotency-Key must not exceed 200 characters", deployment.ErrInvalid))
		return
	}
	var request createDeploymentRequest
	if err := decodeJSON(w, r, &request); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := validateCreateRequest(request); err != nil {
		s.writeError(w, r, err)
		return
	}
	requestDigest, err := canonicalRequestDigest(struct {
		Name string                                   `json:"name"`
		Spec platformv1alpha1.InferenceDeploymentSpec `json:"spec"`
	}{Name: request.Name, Spec: request.spec()})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	replayed, err := s.replayIdempotency(r.Context(), w, principal.TenantID, "deployment:create", idempotencyKey, requestDigest)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if replayed {
		return
	}
	mutation, err := s.deployments.Create(r.Context(), deployment.CreateInput{
		TenantID: principal.TenantID, Namespace: principal.TenantNamespace,
		Name: request.Name, Spec: request.spec(), RequestID: middleware.RequestID(r.Context()),
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("ETag", deploymentETag(mutation.Deployment.Generation))
	s.writeIdempotentJSON(r.Context(), w, principal.TenantID, "deployment:create", idempotencyKey, requestDigest, http.StatusAccepted, mutationResponse{Deployment: deploymentDTO(mutation.Deployment), Operation: mutation.Operation})
}

func validateCreateRequest(request createDeploymentRequest) error {
	var missing []string
	if strings.TrimSpace(request.GPU.Type) == "" {
		missing = append(missing, "gpu.type")
	}
	if request.GPU.Count == nil {
		missing = append(missing, "gpu.count")
	}
	if request.MaxModelLen == nil {
		missing = append(missing, "max_model_len")
	}
	if request.MaxReplicas == nil {
		missing = append(missing, "max_replicas")
	}
	if request.Admission == nil {
		missing = append(missing, "admission")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: required fields missing: %s", deployment.ErrInvalid, strings.Join(missing, ", "))
	}
	return nil
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	principal, _ := middleware.Principal(r.Context())
	options, err := deploymentListOptions(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	page, err := s.deployments.List(r.Context(), principal.TenantID, options)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items := make([]deploymentResponse, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, deploymentDTO(&page.Items[i]))
	}
	next := ""
	if page.NextCursor != nil {
		next = encodeCursor(*page.NextCursor)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	principal, _ := middleware.Principal(r.Context())
	value, err := s.deployments.Get(r.Context(), principal.TenantID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("ETag", deploymentETag(value.Generation))
	writeJSON(w, http.StatusOK, deploymentDTO(value))
}

func (s *Server) patchDeployment(w http.ResponseWriter, r *http.Request) {
	principal, _ := middleware.Principal(r.Context())
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/merge-patch+json") {
		s.writeError(w, r, fmt.Errorf("%w: Content-Type must be application/merge-patch+json", deployment.ErrInvalid))
		return
	}
	current, err := s.deployments.Get(r.Context(), principal.TenantID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if ifMatch == "" {
		s.writeError(w, r, deployment.ErrPreconditionRequired)
		return
	}
	if ifMatch != deploymentETag(current.Generation) {
		s.writeError(w, r, deployment.ErrGenerationConflict)
		return
	}
	patch, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || !json.Valid(patch) {
		s.writeError(w, r, fmt.Errorf("%w: invalid JSON Merge Patch", deployment.ErrInvalid))
		return
	}
	currentJSON, _ := json.Marshal(current.Spec)
	merged, err := jsonpatch.MergePatch(currentJSON, patch)
	if err != nil {
		s.writeError(w, r, fmt.Errorf("%w: invalid JSON Merge Patch: %v", deployment.ErrInvalid, err))
		return
	}
	var spec platformv1alpha1.InferenceDeploymentSpec
	if err := decodeStrictBytes(merged, &spec); err != nil {
		s.writeError(w, r, fmt.Errorf("%w: merged spec is invalid: %v", deployment.ErrInvalid, err))
		return
	}
	mutation, err := s.deployments.Update(r.Context(), deployment.UpdateInput{
		TenantID: principal.TenantID, DeploymentID: r.PathValue("id"),
		ExpectedGeneration: current.Generation, Spec: spec,
		RequestID: middleware.RequestID(r.Context()), IdempotencyKey: r.Header.Get("Idempotency-Key"),
		ForceReselect: patchExplicitlyRequestsAuto(patch),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("ETag", deploymentETag(mutation.Deployment.Generation))
	writeJSON(w, http.StatusAccepted, mutationResponse{Deployment: deploymentDTO(mutation.Deployment), Operation: mutation.Operation})
}

func deploymentETag(generation int64) string { return fmt.Sprintf(`"%d"`, generation) }

func patchExplicitlyRequestsAuto(patch []byte) bool {
	var object map[string]any
	if json.Unmarshal(patch, &object) != nil {
		return false
	}
	runtimeObject, ok := object["runtime"].(map[string]any)
	if !ok {
		return false
	}
	backend, _ := runtimeObject["backend"].(string)
	return backend == string(platformv1alpha1.RuntimeBackendAuto)
}

func decodeStrictBytes(value []byte, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func (s *Server) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	principal, _ := middleware.Principal(r.Context())
	expected, err := optionalPositiveInt64(r.URL.Query().Get("expected_generation"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	operation, err := s.deployments.Delete(r.Context(), deployment.DeleteInput{
		TenantID: principal.TenantID, DeploymentID: r.PathValue("id"), ExpectedGeneration: expected,
		RequestID: middleware.RequestID(r.Context()), IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": operation})
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	principal, _ := middleware.Principal(r.Context())
	operation, err := s.deployments.GetOperation(r.Context(), principal.TenantID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, operation)
}

type createBenchmarkRequest struct {
	Scenario                 string          `json:"scenario"`
	Configuration            json.RawMessage `json:"configuration"`
	RentalPriceUSDPerGPUHour float64         `json:"rental_price_usd_per_gpu_hour"`
}

func (s *Server) createBenchmark(w http.ResponseWriter, r *http.Request) {
	if s.benchmarks == nil {
		s.writeError(w, r, errors.New("benchmark service unavailable"))
		return
	}
	principal, _ := middleware.Principal(r.Context())
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		s.writeError(w, r, fmt.Errorf("%w: valid Idempotency-Key header is required", deployment.ErrInvalid))
		return
	}
	deploymentID := r.PathValue("id")
	cacheScope := "benchmark:create:" + deploymentID
	var request createBenchmarkRequest
	if err := decodeJSON(w, r, &request); err != nil {
		s.writeError(w, r, err)
		return
	}
	canonicalConfiguration, err := canonicalJSON(request.Configuration)
	if err != nil {
		s.writeError(w, r, fmt.Errorf("%w: invalid benchmark configuration: %v", deployment.ErrInvalid, err))
		return
	}
	requestDigest, err := canonicalRequestDigest(struct {
		DeploymentID             string          `json:"deployment_id"`
		Scenario                 string          `json:"scenario"`
		Configuration            json.RawMessage `json:"configuration"`
		RentalPriceUSDPerGPUHour float64         `json:"rental_price_usd_per_gpu_hour"`
	}{deploymentID, request.Scenario, canonicalConfiguration, request.RentalPriceUSDPerGPUHour})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	replayed, err := s.replayIdempotency(r.Context(), w, principal.TenantID, cacheScope, idempotencyKey, requestDigest)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if replayed {
		return
	}
	run, err := s.benchmarks.Create(r.Context(), principal.TenantID, deploymentID, request.Scenario, request.Configuration, request.RentalPriceUSDPerGPUHour, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeIdempotentJSON(r.Context(), w, principal.TenantID, cacheScope, idempotencyKey, requestDigest, http.StatusAccepted, run)
}

func (s *Server) listBenchmarks(w http.ResponseWriter, r *http.Request) {
	if s.benchmarks == nil {
		s.writeError(w, r, errors.New("benchmark service unavailable"))
		return
	}
	principal, _ := middleware.Principal(r.Context())
	options, err := benchmarkListOptions(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	page, err := s.benchmarks.List(r.Context(), principal.TenantID, r.PathValue("id"), options)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	next := ""
	if page.NextCursor != nil {
		next = encodeCursor(*page.NextCursor)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": page.Items, "next_cursor": next})
}

func (s *Server) getMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		s.writeError(w, r, errors.New("metrics service unavailable"))
		return
	}
	principal, _ := middleware.Principal(r.Context())
	until := time.Now().UTC()
	since := until.Add(-time.Hour)
	var err error
	if raw := r.URL.Query().Get("since"); raw != "" {
		since, err = time.Parse(time.RFC3339, raw)
	}
	if err == nil {
		if raw := r.URL.Query().Get("until"); raw != "" {
			until, err = time.Parse(time.RFC3339, raw)
		}
	}
	if err != nil || !since.Before(until) {
		s.writeError(w, r, fmt.Errorf("%w: invalid metrics time range", deployment.ErrInvalid))
		return
	}
	metrics, err := s.metrics.ReadDeploymentMetrics(r.Context(), principal.TenantID, r.PathValue("id"), since, until)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}

func benchmarkListOptions(r *http.Request) (deployment.ListOptions, error) {
	limit, err := optionalPositiveInt(r.URL.Query().Get("limit"))
	if err != nil {
		return deployment.ListOptions{}, err
	}
	if limit == 0 {
		limit = 50
	}
	if limit > 200 {
		return deployment.ListOptions{}, fmt.Errorf("%w: limit must not exceed 200", deployment.ErrInvalid)
	}
	options := deployment.ListOptions{Limit: limit}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cursor, err := decodeCursor(raw)
		if err != nil {
			return deployment.ListOptions{}, fmt.Errorf("%w: invalid cursor", deployment.ErrInvalid)
		}
		options.Cursor = cursor
	}
	return options, nil
}

func deploymentListOptions(r *http.Request) (deployment.ListOptions, error) {
	limit, err := optionalPositiveInt(r.URL.Query().Get("limit"))
	if err != nil {
		return deployment.ListOptions{}, err
	}
	if limit == 0 {
		limit = 50
	}
	if limit > 200 {
		return deployment.ListOptions{}, fmt.Errorf("%w: limit must not exceed 200", deployment.ErrInvalid)
	}
	options := deployment.ListOptions{Limit: limit}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cursor, err := decodeCursor(raw)
		if err != nil {
			return deployment.ListOptions{}, fmt.Errorf("%w: invalid cursor", deployment.ErrInvalid)
		}
		options.Cursor = cursor
	}
	return options, nil
}

func encodeCursor(cursor deployment.ListCursor) string {
	payload, _ := json.Marshal(struct {
		CreatedAt time.Time `json:"t"`
		ID        string    `json:"i"`
	}{cursor.CreatedAt, cursor.ID})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeCursor(raw string) (*deployment.ListCursor, error) {
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	var value struct {
		CreatedAt time.Time `json:"t"`
		ID        string    `json:"i"`
	}
	if err := json.Unmarshal(payload, &value); err != nil || value.CreatedAt.IsZero() || value.ID == "" {
		return nil, errors.New("malformed cursor")
	}
	return &deployment.ListCursor{CreatedAt: value.CreatedAt, ID: value.ID}, nil
}

func optionalPositiveInt(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%w: value must be positive", deployment.ErrInvalid)
	}
	return value, nil
}

func optionalPositiveInt64(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%w: value must be positive", deployment.ErrInvalid)
	}
	return value, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	if contentType := r.Header.Get("Content-Type"); contentType != "" && !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		return fmt.Errorf("%w: Content-Type must be application/json", deployment.ErrInvalid)
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: invalid JSON: %v", deployment.ErrInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: request must contain one JSON object", deployment.ErrInvalid)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
