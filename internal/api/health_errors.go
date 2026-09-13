package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/inferscale/inferscale/internal/api/middleware"
	"github.com/inferscale/inferscale/internal/auth"
	"github.com/inferscale/inferscale/internal/benchmark"
	"github.com/inferscale/inferscale/internal/deployment"
)

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	failures := make(map[string]string)
	for name, check := range s.readiness {
		if err := check(r.Context()); err != nil {
			// Dependency error strings can contain database hosts, usernames,
			// Kubernetes object names, RBAC topology, or credentials. Emit only
			// the bounded component identity at this generic boundary; dedicated
			// dependency metrics and component logs carry safe diagnostics.
			s.logger.Error("readiness check failed", "component", name, "request_id", middleware.RequestID(r.Context()))
			failures[name] = "unavailable"
		}
	}
	if len(failures) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "checks": failures})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type problem struct {
	Type       string      `json:"type"`
	Title      string      `json:"title"`
	Status     int         `json:"status"`
	Code       string      `json:"code"`
	Detail     string      `json:"detail,omitempty"`
	RequestID  string      `json:"request_id,omitempty"`
	Violations []violation `json:"violations,omitempty"`
}

type violation struct {
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := http.StatusInternalServerError, "internal_error"
	switch {
	case errors.Is(err, auth.ErrUnavailable):
		status, code = http.StatusServiceUnavailable, "authentication_unavailable"
	case errors.Is(err, auth.ErrUnauthenticated):
		status, code = http.StatusUnauthorized, "unauthenticated"
	case errors.Is(err, auth.ErrForbidden):
		status, code = http.StatusForbidden, "forbidden"
	case errors.Is(err, deployment.ErrNotFound), errors.Is(err, deployment.ErrBenchmarkNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, benchmark.ErrRunNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, benchmark.ErrInvalidTransition):
		status, code = http.StatusConflict, "invalid_transition"
	case errors.Is(err, deployment.ErrAlreadyExists):
		status, code = http.StatusConflict, "already_exists"
	case errors.Is(err, deployment.ErrIdempotencyConflict):
		status, code = http.StatusConflict, "idempotency_conflict"
	case errors.Is(err, deployment.ErrBenchmarkTargetNotReady):
		status, code = http.StatusConflict, "benchmark_target_not_ready"
	case errors.Is(err, deployment.ErrGenerationConflict):
		status, code = http.StatusPreconditionFailed, "etag_mismatch"
	case errors.Is(err, deployment.ErrPreconditionRequired):
		status, code = http.StatusPreconditionRequired, "precondition_required"
	case errors.Is(err, deployment.ErrQuotaExceeded):
		status, code = http.StatusTooManyRequests, "quota_exceeded"
	case errors.Is(err, deployment.ErrInvalid), errors.Is(err, benchmark.ErrInvalidReport):
		status, code = http.StatusUnprocessableEntity, "invalid_request"
	}
	if status >= 500 {
		s.logger.Error("HTTP request failed", "code", code, "error_type", fmt.Sprintf("%T", err), "request_id", middleware.RequestID(r.Context()))
	}
	titles := map[string]string{
		"unauthenticated": "Authentication required", "forbidden": "Insufficient scope",
		"not_found": "Resource not found", "already_exists": "Resource already exists",
		"idempotency_conflict":       "Idempotency key conflict",
		"benchmark_target_not_ready": "Benchmark target is not ready",
		"invalid_transition":         "Invalid state transition",
		"etag_mismatch":              "Deployment changed", "precondition_required": "Precondition required", "quota_exceeded": "Tenant quota exceeded",
		"invalid_request": "Request validation failed", "internal_error": "Internal server error",
	}
	detail := err.Error()
	if status >= 500 {
		detail = ""
	}
	value := problem{
		Type: "https://inferscale.io/problems/" + code, Title: titles[code], Status: status,
		Code: code, Detail: detail, RequestID: middleware.RequestID(r.Context()),
	}
	if errors.Is(err, deployment.ErrInvalid) || errors.Is(err, benchmark.ErrInvalidReport) {
		value.Violations = []violation{{Message: err.Error()}}
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
