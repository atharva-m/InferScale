package tenant

import "testing"

func TestValidateTenant(t *testing.T) {
	valid := Quota{MaxDeployments: 2, MaxGPUs: 4, RequestsPerMinute: 60, MaxConcurrentRequests: 4, MaxQueuedRequests: 16, DefaultPriorityClass: "standard"}
	if err := validate("acme", "Acme", valid); err != nil {
		t.Fatalf("valid tenant rejected: %v", err)
	}
	if err := validate("Acme!", "Acme", valid); err == nil {
		t.Fatal("invalid slug accepted")
	}
}
