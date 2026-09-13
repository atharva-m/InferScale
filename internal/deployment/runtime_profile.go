package deployment

import (
	"context"
	"encoding/json"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
)

// RuntimeProfile is append-only measured evidence used by backend:auto.
type RuntimeProfile struct {
	ID                       string
	ModelURI                 string
	ModelRevision            string
	Backend                  platformv1alpha1.RuntimeBackend
	BackendVersion           string
	RuntimeImageDigest       string
	GPUSKU                   string
	GPUCount                 int32
	Precision                string
	Quantization             string
	TensorParallelism        int32
	MaxContextBucket         int32
	DriverCUDAFingerprint    string
	ScenarioDigest           string
	Metrics                  json.RawMessage
	CostPerSuccessfulRequest float64
	Eligible                 bool
	ApprovedBy               string
	ApprovedAt               *time.Time
	MeasuredAt               time.Time
	SourceBenchmarkRunID     string
}

type RuntimeProfileFilter struct {
	ModelURI          string
	ModelRevision     string
	GPUSKU            string
	GPUCount          int32
	Precision         string
	Quantization      string
	TensorParallelism int32
	MaxContextBucket  int32
}

type RuntimeProfileRepository interface {
	AppendRuntimeProfile(context.Context, *RuntimeProfile) error
	ListEligibleRuntimeProfiles(context.Context, RuntimeProfileFilter) ([]RuntimeProfile, error)
}

func NewRuntimeProfileID() string { return newID("profile") }
