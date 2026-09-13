package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

type Check func(context.Context) error

func HealthHandler(checks map[string]Check, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), timeout)
		defer cancel()
		results := make(map[string]string, len(checks))
		var mu sync.Mutex
		var wg sync.WaitGroup
		healthy := true
		for name, check := range checks {
			name, check := name, check
			wg.Add(1)
			go func() {
				defer wg.Done()
				status := "ok"
				if err := check(ctx); err != nil {
					status = "unavailable"
				}
				mu.Lock()
				results[name] = status
				if status != "ok" {
					healthy = false
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
		w.Header().Set("content-type", "application/json")
		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": healthy, "checks": results})
	})
}
