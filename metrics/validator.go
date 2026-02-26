package metrics

import (
	"fmt"
	"math"

	"go-controller-agent/dto"
)

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

func NewValidator() *Validator {
	return &Validator{
		minCPU:       0.0,
		maxCPU:       1.0,
		minMemory:    0.0,
		maxMemory:    128.0,
		minLatency:   0.0,
		maxLatency:   10.0,
		minErrorRate: 0.0,
		maxErrorRate: 1.0,
	}
}

type ValidationResult struct {
	Valid    bool
	Warnings []string
	Errors   []string
}

func (v *Validator) Validate(m dto.Metrics) ValidationResult {
	res := ValidationResult{Valid: true, Warnings: []string{}, Errors: []string{}}

	if m.CPUUsage < v.minCPU || m.CPUUsage > v.maxCPU {
		res.Errors = append(res.Errors, fmt.Sprintf("CPU out of range: %.2f", m.CPUUsage))
		res.Valid = false
	}
	if m.CPUUsage > 0.9 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("CPU critically high: %.2f", m.CPUUsage))
	}

	if m.MemoryUsage < v.minMemory || m.MemoryUsage > v.maxMemory {
		res.Errors = append(res.Errors, fmt.Sprintf("Memory out of range: %.2f GB", m.MemoryUsage))
		res.Valid = false
	}

	if m.LatencyP95 < v.minLatency || m.LatencyP95 > v.maxLatency {
		res.Errors = append(res.Errors, fmt.Sprintf("LatencyP95 out of range: %.3f", m.LatencyP95))
		res.Valid = false
	}
	if m.LatencyP95 > 0.5 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("LatencyP95 high: %.3f", m.LatencyP95))
	}

	if m.ErrorRate < v.minErrorRate || m.ErrorRate > v.maxErrorRate {
		res.Errors = append(res.Errors, fmt.Sprintf("ErrorRate out of range: %.4f", m.ErrorRate))
		res.Valid = false
	}
	if m.ErrorRate > 0.01 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("ErrorRate elevated: %.4f", m.ErrorRate))
	}

	if hasInvalidFloats(m) {
		res.Errors = append(res.Errors, "contains NaN/Inf")
		res.Valid = false
	}
	if m.Replicas > 0 && m.PodReady == 0 && m.PodPending == 0 {
		res.Warnings = append(res.Warnings, "replicas > 0 but no pods ready/pending")
	}
	if allZero(m) {
		res.Warnings = append(res.Warnings, "all metrics are zero (collection failure?)")
	}
	return res
}

func (v *Validator) SanitizeMetrics(m dto.Metrics) dto.Metrics {
	s := m
	s.CPUUsage = math.Max(v.minCPU, math.Min(v.maxCPU, s.CPUUsage))
	s.MemoryUsage = math.Max(v.minMemory, math.Min(v.maxMemory, s.MemoryUsage))
	s.LatencyP50 = math.Max(v.minLatency, math.Min(v.maxLatency, s.LatencyP50))
	s.LatencyP95 = math.Max(v.minLatency, math.Min(v.maxLatency, s.LatencyP95))
	s.LatencyP99 = math.Max(v.minLatency, math.Min(v.maxLatency, s.LatencyP99))
	s.ErrorRate = math.Max(v.minErrorRate, math.Min(v.maxErrorRate, s.ErrorRate))

	if math.IsNaN(s.CPUUsage) || math.IsInf(s.CPUUsage, 0) {
		s.CPUUsage = 0
	}
	if math.IsNaN(s.MemoryUsage) || math.IsInf(s.MemoryUsage, 0) {
		s.MemoryUsage = 0
	}
	if math.IsNaN(s.RequestRate) || math.IsInf(s.RequestRate, 0) {
		s.RequestRate = 0
	}
	return s
}

func hasInvalidFloats(m dto.Metrics) bool {
	vals := []float64{
		m.CPUUsage, m.MemoryUsage, m.RequestRate,
		m.LatencyP50, m.LatencyP95, m.LatencyP99,
		m.ErrorRate, m.CPUTrend1m, m.CPUTrend5m, m.RequestTrend,
	}
	for _, f := range vals {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return true
		}
	}
	return false
}

func allZero(m dto.Metrics) bool {
	return m.CPUUsage == 0 && m.MemoryUsage == 0 && m.RequestRate == 0 && m.LatencyP95 == 0 && m.ErrorRate == 0
}