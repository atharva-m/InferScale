package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRequestIDMiddlewareReplacesUnsafeClientValue(t *testing.T) {
	var observed string
	handler := RequestIDMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		observed = RequestID(request.Context())
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/deployments", nil)
	request.Header.Set("X-Request-ID", "secret value/with separators")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if observed == "" || observed == request.Header.Get("X-Request-ID") {
		t.Fatalf("unsafe request ID was retained: %q", observed)
	}
	if _, err := uuid.Parse(observed); err != nil {
		t.Fatalf("replacement request ID %q is not a UUID: %v", observed, err)
	}
	if got := response.Header().Get("X-Request-ID"); got != observed {
		t.Fatalf("response request ID=%q, context=%q", got, observed)
	}
}

func TestSafeRequestIDBounds(t *testing.T) {
	if got := safeRequestID(strings.Repeat("r", 128)); got == "" {
		t.Fatal("128-character safe request ID was rejected")
	}
	for _, value := range []string{strings.Repeat("r", 129), "has space", "has/slash", "line\nbreak"} {
		if got := safeRequestID(value); got != "" {
			t.Fatalf("safeRequestID(%q)=%q, want empty", value, got)
		}
	}
}
