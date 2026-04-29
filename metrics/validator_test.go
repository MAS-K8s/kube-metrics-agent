package metrics_test

import (
	"testing"

	"go-controller-agent/dto"
	"go-controller-agent/metrics"
)

func validMetrics() dto.Metrics {
	return dto.Metrics{
		CPUUsage:    0.4,
		MemoryUsage: 2.0,
		RequestRate: 50.0,
		LatencyP50:  0.1,
		LatencyP95:  0.2,
		LatencyP99:  0.3,
		ErrorRate:   0.001,
		Replicas:    2,
		PodReady:    2,
		PodPending:  0,
	}
}

// TC-UT-13: Valid metrics pass validation
func TestValidatePassesForValidMetrics(t *testing.T) {
	v := metrics.NewValidator()
	result := v.Validate(validMetrics())
	if !result.Valid {
		t.Errorf("expected valid, got errors: %v", result.Errors)
	}
}

// TC-UT-14: CPU above 1.0 fails validation
func TestValidateCPUOutOfRange(t *testing.T) {
	v := metrics.NewValidator()
	m := validMetrics()
	m.CPUUsage = 1.5
	result := v.Validate(m)
	if result.Valid {
		t.Error("expected invalid when CPU > 1.0")
	}
}

// TC-UT-15: Negative error rate fails validation
func TestValidateNegativeErrorRate(t *testing.T) {
	v := metrics.NewValidator()
	m := validMetrics()
	m.ErrorRate = -0.1
	result := v.Validate(m)
	if result.Valid {
		t.Error("expected invalid when ErrorRate < 0")
	}
}

// TC-UT-16: All-zero metrics trigger allZero warning
func TestValidateAllZeroMetrics(t *testing.T) {
	v := metrics.NewValidator()
	m := dto.Metrics{}
	result := v.Validate(m)
	if len(result.Warnings) == 0 {
		t.Error("expected warning for all-zero metrics")
	}
}

// TC-UT-17: LatencyP95 above SLA target triggers warning
func TestValidateHighLatencyWarning(t *testing.T) {
	v := metrics.NewValidator()
	m := validMetrics()
	m.LatencyP95 = 0.8
	result := v.Validate(m)
	if len(result.Warnings) == 0 {
		t.Error("expected warning when LatencyP95 > 0.5s SLA target")
	}
}

// TC-UT-18: SanitizeMetrics clamps CPU above maximum to 1.0
func TestSanitizeClampsHighCPU(t *testing.T) {
	v := metrics.NewValidator()
	m := validMetrics()
	m.CPUUsage = 2.5
	sanitised := v.SanitizeMetrics(m)
	if sanitised.CPUUsage != 1.0 {
		t.Errorf("expected CPUUsage clamped to 1.0, got %.2f", sanitised.CPUUsage)
	}
}

// TC-UT-19: SanitizeMetrics replaces negative LatencyP95 with 0
func TestSanitizeClampsNegativeLatency(t *testing.T) {
	v := metrics.NewValidator()
	m := validMetrics()
	m.LatencyP95 = -0.5
	sanitised := v.SanitizeMetrics(m)
	if sanitised.LatencyP95 != 0.0 {
		t.Errorf("expected LatencyP95 clamped to 0.0, got %.2f", sanitised.LatencyP95)
	}
}

// TC-UT-20: CPU critically high triggers warning not error
func TestValidateCriticalCPUWarning(t *testing.T) {
	v := metrics.NewValidator()
	m := validMetrics()
	m.CPUUsage = 0.95
	result := v.Validate(m)
	if !result.Valid {
		t.Error("CPU=0.95 is within range, should not be an error")
	}
	if len(result.Warnings) == 0 {
		t.Error("expected warning for critically high CPU")
	}
}
