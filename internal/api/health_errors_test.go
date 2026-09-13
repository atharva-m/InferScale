package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferscale/inferscale/internal/api/middleware"
)

func TestReadinessDoesNotExposeDependencyError(t *testing.T) {
	secret := "postgres://db-user:db-password@internal-db.example"
	server := NewServer(Dependencies{
		Readiness: map[string]HealthCheck{
			"postgres": func(_ context.Context) error { return errors.New(secret) },
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) || !strings.Contains(response.Body.String(), `"postgres":"unavailable"`) {
		t.Fatalf("unsafe readiness response: %s", response.Body.String())
	}
}

func TestInternalProblemDoesNotExposeUnderlyingError(t *testing.T) {
	secret := "dial tcp internal-db.example:5432: password=db-password"
	server := NewServer(Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	request := httptest.NewRequest(http.MethodGet, "/v1/deployments", nil)
	request = request.WithContext(middleware.WithRequestID(request.Context(), "req-safe"))
	response := httptest.NewRecorder()
	server.writeError(response, request, errors.New(secret))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), secret) || strings.Contains(response.Body.String(), "internal-db") {
		t.Fatalf("unsafe internal problem response: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"internal_error"`) || !strings.Contains(response.Body.String(), `"request_id":"req-safe"`) {
		t.Fatalf("internal problem contract was lost: %s", response.Body.String())
	}
}
