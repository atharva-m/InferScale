package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/inferscale/inferscale/internal/auth"
)

type APIKeyAdmin struct {
	Service *auth.Service
}

func (a APIKeyAdmin) Issue(ctx context.Context, tenantID, name string, scopes []string, expiresAt *time.Time) (*auth.APIKey, string, error) {
	if a.Service == nil {
		return nil, "", fmt.Errorf("API key service is not configured")
	}
	return a.Service.CreateAPIKey(ctx, tenantID, name, scopes, expiresAt)
}

func (a APIKeyAdmin) Revoke(ctx context.Context, tenantID, keyID string) error {
	if a.Service == nil {
		return fmt.Errorf("API key service is not configured")
	}
	return a.Service.Revoke(ctx, tenantID, keyID)
}
