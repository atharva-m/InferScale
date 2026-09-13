package rollout

import (
	"strings"
	"testing"
	"time"
)

func TestRolloutProgressionAndWeights(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	policy := DefaultPolicy()
	machine := Machine{Policy: policy}
	decision := machine.Evaluate(State{Stage: StagePending, Since: now}, true, Metrics{}, now)
	if decision.Stage != StageCandidateReady {
		t.Fatalf("stage=%s", decision.Stage)
	}
	decision = machine.Evaluate(State{Stage: StageCandidateReady, Since: now}, true, Metrics{}, now)
	if decision.Stage != StageShadow || !decision.Shadow {
		t.Fatalf("decision=%#v", decision)
	}
	good := Metrics{Available: true, CandidateRequests: policy.MinimumRequests}
	decision = machine.Evaluate(State{Stage: StageShadow, Since: now}, true, good, now.Add(policy.ShadowDuration))
	if decision.Stage != StageCanary5 || decision.CandidateWeight != 5 {
		t.Fatalf("decision=%#v", decision)
	}
}

func TestShadowDoesNotEvaluateEPPQueueRegression(t *testing.T) {
	t.Parallel()
	now := time.Unix(500, 0)
	policy := DefaultPolicy()
	metrics := Metrics{
		Source: MetricsSourceShadowRuntime, Available: true, CandidateRequests: policy.MinimumRequests,
		StableQueueP95MS: 1, CandidateQueueP95MS: 100,
	}
	decision := (Machine{Policy: policy}).Evaluate(
		State{Stage: StageShadow, Since: now}, true, metrics, now.Add(policy.ShadowDuration),
	)
	if decision.Rollback || decision.Paused || decision.Stage != StageCanary5 {
		t.Fatalf("shadow decision=%#v", decision)
	}

	decision = (Machine{Policy: policy}).Evaluate(
		State{Stage: StageCanary5, Since: now, RegressionWindows: 1, LastRegressionAt: now.Add(-time.Minute)},
		true, metrics, now.Add(policy.StageDuration),
	)
	if !decision.Rollback || decision.Reason != "candidate queue-latency regression exceeded" {
		t.Fatalf("canary decision=%#v", decision)
	}
}

func TestRolloutPausesWhenMetricsUnavailable(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	policy := DefaultPolicy()
	decision := (Machine{Policy: policy}).Evaluate(
		State{Stage: StageCanary25, Since: now}, true, Metrics{}, now.Add(policy.StageDuration),
	)
	if !decision.Paused || decision.Rollback || decision.Stage != StageCanary25 {
		t.Fatalf("decision=%#v", decision)
	}
}

func TestRolloutRollsBackOnOOM(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	metrics := Metrics{Available: true, CandidateRequests: 100, CandidateOOMs: 1}
	decision := (Machine{Policy: DefaultPolicy()}).Evaluate(
		State{Stage: StageCanary5, Since: now}, true, metrics, now,
	)
	if !decision.Rollback || decision.Stage != StageFailed || decision.StableWeight != 100 {
		t.Fatalf("decision=%#v", decision)
	}
}

func TestDefaultPolicyMatchesV1ReleaseGates(t *testing.T) {
	t.Parallel()
	policy := DefaultPolicy()
	if policy.ShadowDuration != 10*time.Minute || policy.StageDuration != 10*time.Minute {
		t.Fatalf("durations = shadow %s, stage %s", policy.ShadowDuration, policy.StageDuration)
	}
	if policy.MinimumRequests != 200 {
		t.Fatalf("minimum requests = %d, want 200", policy.MinimumRequests)
	}
	if policy.MaxCandidateErrorRate != 0.02 || policy.MaxErrorRateIncrease != 0.01 {
		t.Fatalf("error gates = ceiling %g, increase %g", policy.MaxCandidateErrorRate, policy.MaxErrorRateIncrease)
	}
	if policy.MaxTTFTRatio != 1.15 || policy.MaxTPOTRatio != 1.15 || policy.MaxQueueRatio != 1.25 {
		t.Fatalf("latency gates = TTFT %g, TPOT %g, queue %g", policy.MaxTTFTRatio, policy.MaxTPOTRatio, policy.MaxQueueRatio)
	}
}

func TestRolloutUsesAbsoluteErrorRegressionAndGPUXIDGate(t *testing.T) {
	t.Parallel()
	policy := DefaultPolicy()
	metrics := Metrics{
		Available: true, CandidateRequests: policy.MinimumRequests,
		StableErrorRate: 0.001, CandidateErrorRate: 0.012,
	}
	decision := (Machine{Policy: policy}).Evaluate(
		State{Stage: StageCanary5, Since: time.Unix(1000, 0)}, true, metrics, time.Unix(1000, 0),
	)
	if decision.Rollback || !decision.Paused || decision.RegressionWindows != 1 {
		t.Fatalf("first absolute error regression decision=%#v", decision)
	}
	decision = (Machine{Policy: policy}).Evaluate(
		State{
			Stage: StageCanary5, Since: time.Unix(1000, 0),
			RegressionWindows: decision.RegressionWindows, LastRegressionAt: decision.LastRegressionAt,
		}, true, metrics, time.Unix(1060, 0),
	)
	if !decision.Rollback || decision.Reason != "candidate error-rate regression exceeded" || decision.RegressionWindows != 2 {
		t.Fatalf("second absolute error regression decision=%#v", decision)
	}
	metrics.CandidateErrorRate = 0
	metrics.CandidateXIDErrors = 1
	decision = (Machine{Policy: policy}).Evaluate(
		State{Stage: StageCanary5, Since: time.Unix(1000, 0)}, true, metrics, time.Unix(1000, 0),
	)
	if !decision.Rollback || decision.Reason != "candidate reported a GPU XID error" {
		t.Fatalf("GPU XID decision=%#v", decision)
	}
}

func TestRolloutRequiresDistinctOneMinuteRegressionWindows(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	metrics := Metrics{Available: true, CandidateErrorRate: 0.5}
	machine := Machine{Policy: DefaultPolicy()}
	first := machine.Evaluate(State{Stage: StageCanary25, Since: now}, true, metrics, now)
	tooSoon := machine.Evaluate(State{
		Stage: StageCanary25, Since: now,
		RegressionWindows: first.RegressionWindows, LastRegressionAt: first.LastRegressionAt,
	}, true, metrics, now.Add(30*time.Second))
	if tooSoon.Rollback || tooSoon.RegressionWindows != 1 || tooSoon.RequeueAfter != 30*time.Second {
		t.Fatalf("too-soon decision=%#v", tooSoon)
	}
	second := machine.Evaluate(State{
		Stage: StageCanary25, Since: now,
		RegressionWindows: first.RegressionWindows, LastRegressionAt: first.LastRegressionAt,
	}, true, metrics, now.Add(time.Minute))
	if !second.Rollback || second.RegressionWindows != 2 {
		t.Fatalf("second distinct window decision=%#v", second)
	}
}

func TestRolloutSLOViolationUsesConsecutiveWindows(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	machine := Machine{Policy: DefaultPolicy()}
	metrics := Metrics{Available: true, CandidateSLOViolation: true}
	first := machine.Evaluate(State{Stage: StageCanary5, Since: now}, true, metrics, now)
	if first.Rollback || first.RegressionWindows != 1 {
		t.Fatalf("first SLO window decision=%#v", first)
	}
	second := machine.Evaluate(State{
		Stage: StageCanary5, Since: now,
		RegressionWindows: 1, LastRegressionAt: now,
	}, true, metrics, now.Add(time.Minute))
	if !second.Rollback || second.Reason != "candidate deployment SLO was violated" {
		t.Fatalf("second SLO window decision=%#v", second)
	}
}

func TestRolloutDirectSafetyDoesNotDependOnPrometheusAvailability(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	machine := Machine{Policy: DefaultPolicy()}
	for name, metrics := range map[string]Metrics{
		"oom":     {Available: false, UnavailableReason: "Prometheus unavailable", CandidateOOMs: 1},
		"xid":     {Available: false, UnavailableReason: "request telemetry stale", CandidateXIDErrors: 1},
		"restart": {Available: false, UnavailableReason: "request telemetry stale", CandidateRestarts: 1},
	} {
		t.Run(name, func(t *testing.T) {
			decision := machine.Evaluate(State{Stage: StageCanary5, Since: now}, true, metrics, now)
			if !decision.Rollback || decision.Stage != StageFailed {
				t.Fatalf("safety decision = %#v", decision)
			}
		})
	}
}

func TestRolloutUnavailableReasonIsReportedAtPromotionGate(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	decision := (Machine{Policy: DefaultPolicy()}).Evaluate(
		State{Stage: StageCanary5, Since: now.Add(-11 * time.Minute)},
		true,
		Metrics{Available: false, UnavailableReason: "candidate telemetry is stale"},
		now,
	)
	if !decision.Paused || decision.Rollback || !strings.Contains(decision.Reason, "candidate telemetry is stale") {
		t.Fatalf("unavailable decision = %#v", decision)
	}
}
