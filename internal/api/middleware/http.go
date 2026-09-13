package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/inferscale/inferscale/internal/auth"
)

type Authenticator interface {
	Authenticate(context.Context, string) (*auth.Principal, error)
}

func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := safeRequestID(r.Header.Get("X-Request-ID"))
		if requestID == "" {
			id, err := uuid.NewV7()
			if err != nil {
				requestID = uuid.NewString()
			} else {
				requestID = id.String()
			}
		}
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), requestID)))
	})
}

// safeRequestID admits only a short opaque token. Request IDs are reflected in
// response headers, logs, problem bodies, and spans, so arbitrary client text
// must not cross those boundaries.
func safeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' || character == ':' {
			continue
		}
		return ""
	}
	return value
}

func Authenticate(authenticator Authenticator, onError func(http.ResponseWriter, *http.Request, error), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimSpace(r.Header.Get("X-API-Key"))
		if authorization := strings.TrimSpace(r.Header.Get("Authorization")); authorization != "" {
			kind, value, ok := strings.Cut(authorization, " ")
			if !ok || !strings.EqualFold(kind, "Bearer") {
				onError(w, r, auth.ErrUnauthenticated)
				return
			}
			raw = strings.TrimSpace(value)
		}
		if raw == "" {
			onError(w, r, auth.ErrUnauthenticated)
			return
		}
		principal, err := authenticator.Authenticate(r.Context(), raw)
		if err != nil {
			onError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
	})
}

func RequireScope(scope string, onError func(http.ResponseWriter, *http.Request, error), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := Principal(r.Context())
		if !ok {
			onError(w, r, auth.ErrUnauthenticated)
			return
		}
		if err := auth.RequireScope(principal, scope); err != nil {
			onError(w, r, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func Recover(logger *slog.Logger, onError func(http.ResponseWriter, *http.Request, error), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				logger.Error("HTTP handler panic", "panic_type", fmt.Sprintf("%T", value), "request_id", RequestID(r.Context()))
				onError(w, r, errPanic{})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type errPanic struct{}

func (errPanic) Error() string { return "internal server error" }
