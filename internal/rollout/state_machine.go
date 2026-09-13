package rollout

import (
	"fmt"
	"time"
)

type Stage string

const (
	StagePending        Stage = "Pending"
	StageCandidateReady Stage = "CandidateReady"
	StageShadow         Stage = "Shadow"
	StageCanary5        Stage = "Canary5"
	StageCanary25       Stage = "Canary25"
	StageCanary50       Stage = "Canary50"
	StageCanary100      Stage = "Canary100"
	StageStable         Stage = "Stable"
	StageFailed         Stage = "Failed"
)

type State struct {
	Stage             Stage
	Since             time.Time
	RegressionWindows int32
	LastRegressionAt  time.Time
}

type Metrics struct {
	Source                MetricsSource
	Available             bool
	UnavailableReason     string
	CandidateRequests     int64
	StableErrorRate       float64
	CandidateErrorRate    float64
	StableTTFTP95MS       float64
	CandidateTTFTP95MS    float64
	StableTPOTP95MS       float64
	CandidateTPOTP95MS    float64
	StableQueueP95MS      float64
	CandidateQueueP95MS   float64
	CandidateOOMs         int64
	CandidateXIDErrors    int64
	CandidateRestarts     int64
	CandidateSLOViolation bool
}

// MetricsSource identifies the bounded telemetry contract used for one
// rollout evaluation. It is persisted with rollback evidence so an operator
// can distinguish direct runtime shadow measurements from EPP canary data.
type MetricsSource string

const (
	MetricsSourceEndpointPicker MetricsSource = "endpoint-picker"
	MetricsSourceShadowRuntime  MetricsSource = "shadow-runtime"
)

type Policy struct {
	ShadowDuration        time.Duration
	StageDuration         time.Duration
	MinimumRequests       int64
	MaxCandidateErrorRate float64
	MaxErrorRateIncrease  float64
	MaxTTFTRatio          float64
	MaxTPOTRatio          float64
	MaxQueueRatio         float64
	MaxRestarts           int64
}

func DefaultPolicy() Policy {
	return Policy{
		ShadowDuration: 10 * time.Minute, StageDuration: 10 * time.Minute, MinimumRequests: 200,
		MaxCandidateErrorRate: 0.02, MaxErrorRateIncrease: 0.01,
		MaxTTFTRatio: 1.15, MaxTPOTRatio: 1.15, MaxQueueRatio: 1.25,
		MaxRestarts: 0,
	}
}

type Decision struct {
	Stage             Stage
	StableWeight      int32
	CandidateWeight   int32
	Shadow            bool
	Promote           bool
	Rollback          bool
	Paused            bool
	Reason            string
	RequeueAfter      time.Duration
	RegressionWindows int32
	LastRegressionAt  time.Time
}

type Machine struct {
	Policy Policy
}

func (m Machine) Evaluate(state State, candidateReady bool, metrics Metrics, now time.Time) Decision {
	policy := m.Policy
	if err := validatePolicy(policy); err != nil {
		return Decision{Stage: state.Stage, Paused: true, Reason: err.Error()}
	}
	if state.Stage == "" {
		state.Stage = StagePending
	}
	if state.Since.IsZero() {
		state.Since = now
	}

	if state.Stage != StagePending && state.Stage != StageStable && state.Stage != StageFailed {
		if reason := safetyReason(metrics, policy); reason != "" {
			return weights(Decision{Stage: StageFailed, Rollback: true, Reason: reason})
		}
		if reason := regressionReason(state.Stage, metrics, policy); reason != "" {
			if state.RegressionWindows > 0 && now.Sub(state.LastRegressionAt) < time.Minute {
				return weights(Decision{
					Stage: state.Stage, Paused: true, Reason: "waiting for the next one-minute regression window",
					RequeueAfter:      time.Minute - now.Sub(state.LastRegressionAt),
					RegressionWindows: state.RegressionWindows, LastRegressionAt: state.LastRegressionAt,
				})
			}
			windows := state.RegressionWindows + 1
			if windows >= 2 {
				return weights(Decision{
					Stage: StageFailed, Rollback: true, Reason: reason,
					RegressionWindows: windows, LastRegressionAt: now,
				})
			}
			return weights(Decision{
				Stage: state.Stage, Paused: true,
				Reason:       "first failing one-minute window: " + reason,
				RequeueAfter: time.Minute, RegressionWindows: windows, LastRegressionAt: now,
			})
		}
	}

	switch state.Stage {
	case StagePending:
		if !candidateReady {
			return weights(Decision{Stage: StagePending, Paused: true, Reason: "candidate is not Ready", RequeueAfter: 10 * time.Second})
		}
		return weights(Decision{Stage: StageCandidateReady, Reason: "candidate became Ready"})
	case StageCandidateReady:
		return weights(Decision{Stage: StageShadow, Shadow: true, Reason: "begin shadow traffic"})
	case StageShadow:
		if !elapsed(state.Since, now, policy.ShadowDuration) {
			return weights(Decision{Stage: StageShadow, Shadow: true, RequeueAfter: remaining(state.Since, now, policy.ShadowDuration)})
		}
		if paused := promotionReadiness(metrics, policy); paused != "" {
			return weights(Decision{Stage: StageShadow, Shadow: true, Paused: true, Reason: paused, RequeueAfter: time.Minute})
		}
		return weights(Decision{Stage: StageCanary5, Reason: "shadow gates passed"})
	case StageCanary5:
		return advanceCanary(state, now, metrics, policy, StageCanary5, StageCanary25)
	case StageCanary25:
		return advanceCanary(state, now, metrics, policy, StageCanary25, StageCanary50)
	case StageCanary50:
		return advanceCanary(state, now, metrics, policy, StageCanary50, StageCanary100)
	case StageCanary100:
		if !elapsed(state.Since, now, policy.StageDuration) {
			return weights(Decision{Stage: StageCanary100, RequeueAfter: remaining(state.Since, now, policy.StageDuration)})
		}
		if paused := promotionReadiness(metrics, policy); paused != "" {
			return weights(Decision{Stage: StageCanary100, Paused: true, Reason: paused, RequeueAfter: time.Minute})
		}
		return weights(Decision{Stage: StageStable, Promote: true, Reason: "candidate passed all rollout gates"})
	case StageStable:
		return weights(Decision{Stage: StageStable})
	case StageFailed:
		return weights(Decision{Stage: StageFailed, Rollback: true, Reason: "candidate previously failed"})
	default:
		return weights(Decision{Stage: state.Stage, Paused: true, Reason: fmt.Sprintf("unknown rollout stage %q", state.Stage)})
	}
}

func advanceCanary(state State, now time.Time, metrics Metrics, policy Policy, current, next Stage) Decision {
	if !elapsed(state.Since, now, policy.StageDuration) {
		return weights(Decision{Stage: current, RequeueAfter: remaining(state.Since, now, policy.StageDuration)})
	}
	if paused := promotionReadiness(metrics, policy); paused != "" {
		return weights(Decision{Stage: current, Paused: true, Reason: paused, RequeueAfter: time.Minute})
	}
	return weights(Decision{Stage: next, Reason: fmt.Sprintf("%s gates passed", current)})
}

func weights(decision Decision) Decision {
	switch decision.Stage {
	case StagePending, StageCandidateReady:
		decision.StableWeight, decision.CandidateWeight = 100, 0
	case StageShadow:
		decision.StableWeight, decision.CandidateWeight, decision.Shadow = 100, 0, true
	case StageCanary5:
		decision.StableWeight, decision.CandidateWeight = 95, 5
	case StageCanary25:
		decision.StableWeight, decision.CandidateWeight = 75, 25
	case StageCanary50:
		decision.StableWeight, decision.CandidateWeight = 50, 50
	case StageCanary100, StageStable:
		decision.StableWeight, decision.CandidateWeight = 0, 100
	case StageFailed:
		decision.StableWeight, decision.CandidateWeight = 100, 0
	}
	return decision
}

func safetyReason(metrics Metrics, policy Policy) string {
	if metrics.CandidateOOMs > 0 {
		return "candidate reported an OOM"
	}
	if metrics.CandidateXIDErrors > 0 {
		return "candidate reported a GPU XID error"
	}
	if metrics.CandidateRestarts > policy.MaxRestarts {
		return "candidate restart threshold exceeded"
	}
	return ""
}

func regressionReason(stage Stage, metrics Metrics, policy Policy) string {
	if !metrics.Available {
		return ""
	}
	if metrics.CandidateSLOViolation {
		return "candidate deployment SLO was violated"
	}
	if metrics.CandidateErrorRate > policy.MaxCandidateErrorRate {
		return "candidate error-rate threshold exceeded"
	}
	if metrics.CandidateErrorRate > metrics.StableErrorRate+policy.MaxErrorRateIncrease {
		return "candidate error-rate regression exceeded"
	}
	if exceedsRatio(metrics.CandidateTTFTP95MS, metrics.StableTTFTP95MS, policy.MaxTTFTRatio) {
		return "candidate TTFT regression exceeded"
	}
	if exceedsRatio(metrics.CandidateTPOTP95MS, metrics.StableTPOTP95MS, policy.MaxTPOTRatio) {
		return "candidate TPOT regression exceeded"
	}
	// Shadow requests are mirrored directly to the runtime Service and never
	// traverse either revision's EPP. Queue latency is consequently neither
	// available nor comparable until the first weighted canary stage.
	if stage != StageShadow && exceedsRatio(metrics.CandidateQueueP95MS, metrics.StableQueueP95MS, policy.MaxQueueRatio) {
		return "candidate queue-latency regression exceeded"
	}
	return ""
}

func promotionReadiness(metrics Metrics, policy Policy) string {
	if !metrics.Available {
		if metrics.UnavailableReason != "" {
			return "rollout metrics are unavailable: " + metrics.UnavailableReason
		}
		return "rollout metrics are unavailable"
	}
	if metrics.CandidateRequests < policy.MinimumRequests {
		return "insufficient candidate samples"
	}
	return ""
}

func exceedsRatio(candidate, stable, maximum float64) bool {
	if candidate <= 0 || stable <= 0 {
		return false
	}
	return candidate/stable > maximum
}

func validatePolicy(policy Policy) error {
	if policy.ShadowDuration <= 0 || policy.StageDuration <= 0 || policy.MinimumRequests < 1 {
		return fmt.Errorf("rollout durations and minimum requests must be positive")
	}
	if policy.MaxCandidateErrorRate < 0 || policy.MaxErrorRateIncrease < 0 ||
		policy.MaxTTFTRatio < 1 || policy.MaxTPOTRatio < 1 || policy.MaxQueueRatio < 1 {
		return fmt.Errorf("rollout regression thresholds are invalid")
	}
	return nil
}

func elapsed(since, now time.Time, duration time.Duration) bool {
	return !now.Before(since.Add(duration))
}

func remaining(since, now time.Time, duration time.Duration) time.Duration {
	value := since.Add(duration).Sub(now)
	if value < 0 {
		return 0
	}
	return value
}
