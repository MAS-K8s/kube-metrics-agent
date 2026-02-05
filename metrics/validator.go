package metrics

import (
	"fmt"
	"math"
)

// Validator validates collected metrics
type Validator struct {
	minCPU       float64
	maxCPU       float64
	minMemory    float64
	maxMemory    float64
	minLatency   float64
	maxLatency   float64
	minErrorRate float64
	maxErrorRate float64
}

// NewValidator creates a new metrics validator with default thresholds
func NewValidator() *Validator {
	return &Validator{
		minCPU:       0.0,
		maxCPU:       1.0,
		minMemory:    0.0,
		maxMemory:    128.0, // 128GB max
		minLatency:   0.0,
		maxLatency:   10.0, // 10 seconds max
		minErrorRate: 0.0,
		maxErrorRate: 1.0,
	}
}

// ValidationResult contains validation details
type ValidationResult struct {
	Valid    bool
	Warnings []string
	Errors   []string
}

// Validate checks if metrics are within acceptable ranges
func (v *Validator) Validate(m Metrics) ValidationResult {
	result := ValidationResult{
		Valid:    true,
		Warnings: make([]string, 0),
		Errors:   make([]string, 0),
	}

	// Validate CPU
	if m.CPUUsage < v.minCPU || m.CPUUsage > v.maxCPU {
		result.Errors = append(result.Errors,
			fmt.Sprintf("CPU usage out of range: %.2f (expected %.2f-%.2f)",
				m.CPUUsage, v.minCPU, v.maxCPU))
		result.Valid = false
	}

	// Warn if CPU is very high
	if m.CPUUsage > 0.9 {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("CPU usage critically high: %.2f", m.CPUUsage))
	}

	// Validate Memory
	if m.MemoryUsage < v.minMemory || m.MemoryUsage > v.maxMemory {
		result.Errors = append(result.Errors,
			fmt.Sprintf("Memory usage out of range: %.2f GB (expected %.2f-%.2f GB)",
				m.MemoryUsage, v.minMemory, v.maxMemory))
		result.Valid = false
	}

	// Validate Latency
	if m.LatencyP95 < v.minLatency || m.LatencyP95 > v.maxLatency {
		result.Errors = append(result.Errors,
			fmt.Sprintf("Latency P95 out of range: %.3f s (expected %.3f-%.3f s)",
				m.LatencyP95, v.minLatency, v.maxLatency))
		result.Valid = false
	}

	// Warn if latency is high
	if m.LatencyP95 > 0.5 {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("Latency P95 high: %.3f s (SLA: 0.5s)", m.LatencyP95))
	}

	// Validate Error Rate
	if m.ErrorRate < v.minErrorRate || m.ErrorRate > v.maxErrorRate {
		result.Errors = append(result.Errors,
			fmt.Sprintf("Error rate out of range: %.4f (expected %.4f-%.4f)",
				m.ErrorRate, v.minErrorRate, v.maxErrorRate))
		result.Valid = false
	}

	// Warn if error rate is elevated
	if m.ErrorRate > 0.01 {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("Error rate elevated: %.4f (>1%%)", m.ErrorRate))
	}

	// Check for NaN or Inf values
	if v.hasInvalidFloats(m) {
		result.Errors = append(result.Errors, "Metrics contain NaN or Inf values")
		result.Valid = false
	}

	// Validate pod counts
	if m.Replicas > 0 && m.PodReady == 0 && m.PodPending == 0 {
		result.Warnings = append(result.Warnings,
			"No pods are ready or pending despite replicas > 0")
	}

	// Check if all metrics are zero (likely collection failure)
	if v.allZero(m) {
		result.Warnings = append(result.Warnings,
			"All metrics are zero - possible collection failure")
	}

	return result
}

// hasInvalidFloats checks for NaN or Inf values
func (v *Validator) hasInvalidFloats(m Metrics) bool {
	floats := []float64{
		m.CPUUsage,
		m.MemoryUsage,
		m.RequestRate,
		m.LatencyP50,
		m.LatencyP95,
		m.LatencyP99,
		m.ErrorRate,
		m.CPUTrend1m,
		m.CPUTrend5m,
		m.RequestTrend,
	}

	for _, f := range floats {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return true
		}
	}

	return false
}

// allZero checks if all critical metrics are zero
func (v *Validator) allZero(m Metrics) bool {
	return m.CPUUsage == 0 &&
		m.MemoryUsage == 0 &&
		m.RequestRate == 0 &&
		m.LatencyP95 == 0 &&
		m.ErrorRate == 0
}

// SanitizeMetrics cleans and bounds metrics to acceptable ranges
func (v *Validator) SanitizeMetrics(m Metrics) Metrics {
	sanitized := m

	// Bound CPU
	sanitized.CPUUsage = math.Max(v.minCPU, math.Min(v.maxCPU, m.CPUUsage))

	// Bound Memory
	sanitized.MemoryUsage = math.Max(v.minMemory, math.Min(v.maxMemory, m.MemoryUsage))

	// Bound Latency
	sanitized.LatencyP50 = math.Max(v.minLatency, math.Min(v.maxLatency, m.LatencyP50))
	sanitized.LatencyP95 = math.Max(v.minLatency, math.Min(v.maxLatency, m.LatencyP95))
	sanitized.LatencyP99 = math.Max(v.minLatency, math.Min(v.maxLatency, m.LatencyP99))

	// Bound Error Rate
	sanitized.ErrorRate = math.Max(v.minErrorRate, math.Min(v.maxErrorRate, m.ErrorRate))

	// Replace NaN/Inf with 0
	if math.IsNaN(sanitized.CPUUsage) || math.IsInf(sanitized.CPUUsage, 0) {
		sanitized.CPUUsage = 0
	}
	if math.IsNaN(sanitized.MemoryUsage) || math.IsInf(sanitized.MemoryUsage, 0) {
		sanitized.MemoryUsage = 0
	}
	if math.IsNaN(sanitized.RequestRate) || math.IsInf(sanitized.RequestRate, 0) {
		sanitized.RequestRate = 0
	}

	return sanitized
}

// CheckSLA checks if metrics meet SLA requirements
type SLACheck struct {
	LatencyTarget float64
	ErrorTarget   float64
	UptimeTarget  float64
}

// CheckSLA validates metrics against SLA
func (s *SLACheck) Check(m Metrics) (bool, []string) {
	violations := make([]string, 0)

	// Check latency SLA
	if m.LatencyP95 > s.LatencyTarget {
		violations = append(violations,
			fmt.Sprintf("Latency SLA violated: %.3fs > %.3fs target",
				m.LatencyP95, s.LatencyTarget))
	}

	// Check error rate SLA
	if m.ErrorRate > s.ErrorTarget {
		violations = append(violations,
			fmt.Sprintf("Error rate SLA violated: %.4f > %.4f target",
				m.ErrorRate, s.ErrorTarget))
	}

	// Check uptime (based on ready pods)
	if m.Replicas > 0 {
		uptime := float64(m.PodReady) / float64(m.Replicas)
		if uptime < s.UptimeTarget {
			violations = append(violations,
				fmt.Sprintf("Uptime SLA violated: %.2f%% < %.2f%% target",
					uptime*100, s.UptimeTarget*100))
		}
	}

	return len(violations) == 0, violations
}

// NewSLACheck creates a default SLA checker
func NewSLACheck() *SLACheck {
	return &SLACheck{
		LatencyTarget: 0.5,   // 500ms
		ErrorTarget:   0.01,  // 1%
		UptimeTarget:  0.99,  // 99%
	}
}