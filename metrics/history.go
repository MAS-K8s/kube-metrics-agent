package metrics

import (
	"sync"
)

// History maintains a rolling window of historical metrics
type History struct {
	metrics []Metrics
	maxSize int
	mu      sync.RWMutex
}

// NewHistory creates a new metrics history buffer
func NewHistory(maxSize int) *History {
	return &History{
		metrics: make([]Metrics, 0, maxSize),
		maxSize: maxSize,
	}
}

// Add appends a new metrics snapshot to history
func (h *History) Add(m Metrics) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.metrics = append(h.metrics, m)
	if len(h.metrics) > h.maxSize {
		h.metrics = h.metrics[1:]
	}
}

// GetRecent retrieves the n most recent metrics
func (h *History) GetRecent(n int) []Metrics {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.metrics) < n {
		result := make([]Metrics, len(h.metrics))
		copy(result, h.metrics)
		return result
	}

	result := make([]Metrics, n)
	copy(result, h.metrics[len(h.metrics)-n:])
	return result
}

// GetAll returns all metrics in history
func (h *History) GetAll() []Metrics {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make([]Metrics, len(h.metrics))
	copy(result, h.metrics)
	return result
}

// Size returns the current number of metrics in history
func (h *History) Size() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.metrics)
}

// Clear removes all metrics from history
func (h *History) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.metrics = make([]Metrics, 0, h.maxSize)
}

// CalculateTrends computes trend metrics from history
func (h *History) CalculateTrends() (cpu1m, cpu5m, requestTrend float64) {
	recent := h.GetRecent(10) // Last 10 samples for 5 minutes at 30s intervals

	if len(recent) < 2 {
		return 0, 0, 0
	}

	// CPU trend over last 1 minute (2 samples)
	if len(recent) >= 2 {
		cpu1m = recent[len(recent)-1].CPUUsage - recent[len(recent)-2].CPUUsage
	}

	// CPU trend over last 5 minutes (10 samples)
	if len(recent) >= 10 {
		cpu5m = (recent[len(recent)-1].CPUUsage - recent[0].CPUUsage) / 10.0
	}

	// Request rate trend
	if len(recent) >= 2 {
		requestTrend = recent[len(recent)-1].RequestRate - recent[len(recent)-2].RequestRate
	}

	return
}

// GetAverages calculates average metrics over the history
func (h *History) GetAverages() Metrics {
	all := h.GetAll()
	if len(all) == 0 {
		return Metrics{}
	}

	var sum Metrics
	for _, m := range all {
		sum.CPUUsage += m.CPUUsage
		sum.MemoryUsage += m.MemoryUsage
		sum.RequestRate += m.RequestRate
		sum.LatencyP50 += m.LatencyP50
		sum.LatencyP95 += m.LatencyP95
		sum.LatencyP99 += m.LatencyP99
		sum.ErrorRate += m.ErrorRate
		sum.Replicas += m.Replicas
	}

	count := float64(len(all))
	return Metrics{
		CPUUsage:    sum.CPUUsage / count,
		MemoryUsage: sum.MemoryUsage / count,
		RequestRate: sum.RequestRate / count,
		LatencyP50:  sum.LatencyP50 / count,
		LatencyP95:  sum.LatencyP95 / count,
		LatencyP99:  sum.LatencyP99 / count,
		ErrorRate:   sum.ErrorRate / count,
		Replicas:    sum.Replicas / int32(count),
	}
}

// GetStdDev calculates standard deviation of CPU usage
func (h *History) GetStdDev() float64 {
	all := h.GetAll()
	if len(all) < 2 {
		return 0
	}

	// Calculate mean
	var sum float64
	for _, m := range all {
		sum += m.CPUUsage
	}
	mean := sum / float64(len(all))

	// Calculate variance
	var variance float64
	for _, m := range all {
		diff := m.CPUUsage - mean
		variance += diff * diff
	}
	variance /= float64(len(all))

	// Return standard deviation
	return variance
}

// GetPeakValues returns the peak values from history
func (h *History) GetPeakValues() Metrics {
	all := h.GetAll()
	if len(all) == 0 {
		return Metrics{}
	}

	peak := all[0]
	for _, m := range all[1:] {
		if m.CPUUsage > peak.CPUUsage {
			peak.CPUUsage = m.CPUUsage
		}
		if m.MemoryUsage > peak.MemoryUsage {
			peak.MemoryUsage = m.MemoryUsage
		}
		if m.RequestRate > peak.RequestRate {
			peak.RequestRate = m.RequestRate
		}
		if m.LatencyP95 > peak.LatencyP95 {
			peak.LatencyP95 = m.LatencyP95
		}
		if m.ErrorRate > peak.ErrorRate {
			peak.ErrorRate = m.ErrorRate
		}
		if m.Replicas > peak.Replicas {
			peak.Replicas = m.Replicas
		}
	}

	return peak
}

// IsStable checks if metrics have been stable over recent history
func (h *History) IsStable(cpuThreshold, latencyThreshold float64) bool {
	recent := h.GetRecent(5)
	if len(recent) < 5 {
		return false
	}

	// Check CPU stability
	var cpuSum float64
	for _, m := range recent {
		cpuSum += m.CPUUsage
	}
	cpuAvg := cpuSum / float64(len(recent))

	for _, m := range recent {
		if abs(m.CPUUsage-cpuAvg) > cpuThreshold {
			return false
		}
	}

	// Check latency stability
	var latencySum float64
	for _, m := range recent {
		latencySum += m.LatencyP95
	}
	latencyAvg := latencySum / float64(len(recent))

	for _, m := range recent {
		if abs(m.LatencyP95-latencyAvg) > latencyThreshold {
			return false
		}
	}

	return true
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}