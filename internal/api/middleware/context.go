package middleware

import (
	"context"

	"github.com/inferscale/inferscale/internal/auth"
)

type contextKey string

const (
	principalKey contextKey = "principal"
	requestIDKey contextKey = "request-id"
)

func WithPrincipal(ctx context.Context, principal *auth.Principal) context.Context {
	return context.WithValue(ctx, principalKey, principal)
}

func Principal(ctx context.Context) (*auth.Principal, bool) {
	principal, ok := ctx.Value(principalKey).(*auth.Principal)
	return principal, ok && principal != nil
}

func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey, requestID)
}

func RequestID(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey).(string)
	return requestID
}
