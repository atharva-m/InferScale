package postgres

import (
	"testing"

	"github.com/inferscale/inferscale/internal/tenant"
)

func TestAllocationWithinQuota(t *testing.T) {
	quota := tenant.Quota{MaxDeployments: 2, MaxGPUs: 4, MaxConcurrentRequests: 32, MaxQueuedRequests: 64}
	if !allocationWithinQuota(quota, 1, 2, 16, 32, 2, 16, 32) {
		t.Fatal("allocation exactly at each quota was rejected")
	}
	if allocationWithinQuota(quota, 1, 2, 24, 32, 1, 16, 16) {
		t.Fatal("concurrency over-allocation was accepted")
	}
	if allocationWithinQuota(quota, 1, 2, 16, 48, 1, 8, 32) {
		t.Fatal("queue over-allocation was accepted")
	}
}
