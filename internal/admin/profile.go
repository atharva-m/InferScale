package admin

import (
	"context"
	"fmt"
	"time"
)

type ProfileEligibilityRepository interface {
	SetRuntimeProfileEligibility(context.Context, string, bool, string, time.Time) error
}

type ProfileAdmin struct {
	Repository ProfileEligibilityRepository
	Now        func() time.Time
}

func (a ProfileAdmin) SetEligibility(ctx context.Context, profileID, operator string, eligible bool) error {
	if a.Repository == nil {
		return fmt.Errorf("runtime profile repository is not configured")
	}
	if profileID == "" || operator == "" {
		return fmt.Errorf("profile ID and operator are required")
	}
	now := a.Now
	if now == nil {
		now = time.Now
	}
	return a.Repository.SetRuntimeProfileEligibility(ctx, profileID, eligible, operator, now().UTC())
}
