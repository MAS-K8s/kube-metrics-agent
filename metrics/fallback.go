package metrics

import (
	"fmt"

	"go-controller-agent/dto"
)

// FallbackAction mirrors the RL agent's three-way action space so the rest of
// reconcileOne can treat rule-based decisions identically to RL decisions.
//
//	0 = scale_down
//	1 = no_action
//	2 = scale_up
const (
	FallbackActionScaleDown = 0
	FallbackActionNoAction  = 1
	FallbackActionScaleUp   = 2
)

// FallbackDecision is the rule-based equivalent of dto.RLResponse.
// Confidence is always 1.0 — rule thresholds are deterministic.
type FallbackDecision struct {
	Action     int
	ActionName string
	Reason     string
	Confidence float64
}

// FallbackConfig holds the threshold knobs for the rule-based policy.
// All fields are populated from config.Config (see config.go).
type FallbackConfig struct {
	// Scale-up triggers — ANY one being true causes scale_up.
	ScaleUpCPUThreshold     float64 // e.g. 0.75  → scale up when CPU  > 75 %
	ScaleUpLatencyThreshold float64 // e.g. 0.5   → scale up when P95  > 500 ms
	ScaleUpErrorThreshold   float64 // e.g. 0.05  → scale up when err% > 5 %
	ScaleUpRequestThreshold float64 // e.g. 100.0 → scale up when req/s > threshold (0 = disabled)

	// Scale-down triggers — ALL must be true simultaneously for scale_down.
	ScaleDownCPUThreshold     float64 // e.g. 0.20 → scale down when CPU  < 20 %
	ScaleDownLatencyThreshold float64 // e.g. 0.1  → scale down when P95  < 100 ms
	ScaleDownErrorThreshold   float64 // e.g. 0.01 → scale down when err% < 1 %
}

// DefaultFallbackConfig returns safe, conservative thresholds.
// These deliberately bias toward scale-up to avoid underprovisioning during
// an RL agent outage, when the environment is already degraded.
func DefaultFallbackConfig() FallbackConfig {
	return FallbackConfig{
		ScaleUpCPUThreshold:     0.75,
		ScaleUpLatencyThreshold: 0.50,
		ScaleUpErrorThreshold:   0.05,
		ScaleUpRequestThreshold: 100.0,

		ScaleDownCPUThreshold:     0.20,
		ScaleDownLatencyThreshold: 0.10,
		ScaleDownErrorThreshold:   0.01,
	}
}

// RuleBasedScaler decides how to scale a deployment using only static
// thresholds, with no ML component. It is the drop-in fallback invoked when
// the RL agent is unreachable or the circuit breaker is open.
type RuleBasedScaler struct {
	cfg FallbackConfig
}

// NewRuleBasedScaler constructs a RuleBasedScaler with the provided config.
func NewRuleBasedScaler(cfg FallbackConfig) *RuleBasedScaler {
	return &RuleBasedScaler{cfg: cfg}
}

// Decide returns a FallbackDecision for the given metrics snapshot.
//
// Priority order (first matching rule wins):
//  1. Scale-up  — triggered when ANY single metric exceeds its high threshold.
//     Rationale: during an RL outage the system may already be under stress;
//     erring on the side of scale-up is safer than underprovisioning.
//  2. Scale-down — triggered only when ALL low-load metrics are simultaneously
//     below their thresholds, preventing premature scale-down.
//  3. No action — default when neither condition is met.
func (r *RuleBasedScaler) Decide(m dto.Metrics) FallbackDecision {
	// ── 1. Scale-up checks (any one suffices) ────────────────────────────────

	if m.CPUUsage > r.cfg.ScaleUpCPUThreshold {
		return FallbackDecision{
			Action:     FallbackActionScaleUp,
			ActionName: "scale_up",
			Reason: fmt.Sprintf(
				"fallback: CPU %.1f%% exceeds scale-up threshold %.1f%%",
				m.CPUUsage*100, r.cfg.ScaleUpCPUThreshold*100,
			),
			Confidence: 1.0,
		}
	}

	if m.LatencyP95 > r.cfg.ScaleUpLatencyThreshold {
		return FallbackDecision{
			Action:     FallbackActionScaleUp,
			ActionName: "scale_up",
			Reason: fmt.Sprintf(
				"fallback: P95 latency %.3fs exceeds scale-up threshold %.3fs",
				m.LatencyP95, r.cfg.ScaleUpLatencyThreshold,
			),
			Confidence: 1.0,
		}
	}

	if m.ErrorRate > r.cfg.ScaleUpErrorThreshold {
		return FallbackDecision{
			Action:     FallbackActionScaleUp,
			ActionName: "scale_up",
			Reason: fmt.Sprintf(
				"fallback: error rate %.2f%% exceeds scale-up threshold %.2f%%",
				m.ErrorRate*100, r.cfg.ScaleUpErrorThreshold*100,
			),
			Confidence: 1.0,
		}
	}

	if r.cfg.ScaleUpRequestThreshold > 0 && m.RequestRate > r.cfg.ScaleUpRequestThreshold {
		return FallbackDecision{
			Action:     FallbackActionScaleUp,
			ActionName: "scale_up",
			Reason: fmt.Sprintf(
				"fallback: request rate %.1f req/s exceeds scale-up threshold %.1f req/s",
				m.RequestRate, r.cfg.ScaleUpRequestThreshold,
			),
			Confidence: 1.0,
		}
	}

	// ── 2. Scale-down checks (all must be true simultaneously) ───────────────
	//
	// A zero latency reading means no HTTP histogram data was collected; we
	// treat it as "low latency" rather than blocking scale-down indefinitely.
	cpuLow     := m.CPUUsage   < r.cfg.ScaleDownCPUThreshold
	latencyLow := m.LatencyP95 == 0 || m.LatencyP95 < r.cfg.ScaleDownLatencyThreshold
	errorLow   := m.ErrorRate  < r.cfg.ScaleDownErrorThreshold

	if cpuLow && latencyLow && errorLow {
		return FallbackDecision{
			Action:     FallbackActionScaleDown,
			ActionName: "scale_down",
			Reason: fmt.Sprintf(
				"fallback: all metrics below scale-down thresholds "+
					"(CPU %.1f%% < %.1f%%, P95 %.3fs < %.3fs, err %.2f%% < %.2f%%)",
				m.CPUUsage*100, r.cfg.ScaleDownCPUThreshold*100,
				m.LatencyP95, r.cfg.ScaleDownLatencyThreshold,
				m.ErrorRate*100, r.cfg.ScaleDownErrorThreshold*100,
			),
			Confidence: 1.0,
		}
	}

	// ── 3. Default: hold current replica count ───────────────────────────────
	return FallbackDecision{
		Action:     FallbackActionNoAction,
		ActionName: "no_action",
		Reason:     "fallback: metrics within normal range — no scaling required",
		Confidence: 1.0,
	}
}