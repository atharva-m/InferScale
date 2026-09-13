package deployment

import "errors"

var (
	ErrNotFound                = errors.New("deployment not found")
	ErrAlreadyExists           = errors.New("deployment already exists")
	ErrGenerationConflict      = errors.New("deployment generation conflict")
	ErrInvalid                 = errors.New("invalid deployment")
	ErrBenchmarkNotFound       = errors.New("benchmark run not found")
	ErrBenchmarkTargetNotReady = errors.New("deployment is not ready for benchmarking")
	ErrQuotaExceeded           = errors.New("tenant deployment or GPU quota exceeded")
	ErrPreconditionRequired    = errors.New("If-Match precondition is required")
	ErrIdempotencyConflict     = errors.New("idempotency key was already used for a different request")
)
