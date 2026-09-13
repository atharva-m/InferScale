package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/inferscale/inferscale/internal/auth"
	"github.com/inferscale/inferscale/internal/benchmark"
)

func (s *Server) ingestBenchmarkResult(w http.ResponseWriter, r *http.Request) {
	if s.benchmarkResults == nil || s.benchmarkCallbacks == nil {
		s.writeError(w, r, errorsUnavailable("benchmark result ingestion unavailable"))
		return
	}
	runID := strings.TrimSpace(r.PathValue("id"))
	token, err := bearerToken(r.Header.Get("Authorization"))
	if err != nil || s.benchmarkCallbacks.Verify(runID, token) != nil {
		s.writeError(w, r, auth.ErrUnauthenticated)
		return
	}
	var report benchmark.ResultReport
	if err := decodeJSON(w, r, &report); err != nil {
		s.writeError(w, r, err)
		return
	}
	run, err := s.benchmarkResults.IngestResult(r.Context(), runID, report)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func bearerToken(value string) (string, error) {
	kind, token, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(kind, "Bearer") || strings.TrimSpace(token) == "" {
		return "", auth.ErrUnauthenticated
	}
	return strings.TrimSpace(token), nil
}

// errorsUnavailable deliberately avoids exposing scheduler configuration in
// an internal callback response while still producing a normal 5xx problem.
func errorsUnavailable(message string) error {
	return fmt.Errorf("%s", message)
}
