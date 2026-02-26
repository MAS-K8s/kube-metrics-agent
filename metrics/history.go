package metrics

import (
	"math"
	"sync"

	"go-controller-agent/dto"
)

type History struct {
	metrics []dto.Metrics
	maxSize int
	mu      sync.RWMutex
}

func NewHistory(maxSize int) *History {
	return &History{
		metrics: make([]dto.Metrics, 0, maxSize),
		maxSize: maxSize,
	}
}

func (h *History) Add(m dto.Metrics) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.metrics = append(h.metrics, m)
	if len(h.metrics) > h.maxSize {
		h.metrics = h.metrics[1:]
	}
}

func (h *History) GetRecent(n int) []dto.Metrics {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.metrics) < n {
		out := make([]dto.Metrics, len(h.metrics))
		copy(out, h.metrics)
		return out
	}

	out := make([]dto.Metrics, n)
	copy(out, h.metrics[len(h.metrics)-n:])
	return out
}

func (h *History) CalculateTrends() (cpu1m, cpu5m, requestTrend float64) {
	recent := h.GetRecent(10)
	if len(recent) < 2 {
		return 0, 0, 0
	}

	cpu1m = recent[len(recent)-1].CPUUsage - recent[len(recent)-2].CPUUsage

	if len(recent) >= 10 {
		cpu5m = (recent[len(recent)-1].CPUUsage - recent[0].CPUUsage) / float64(len(recent))
	}

	requestTrend = recent[len(recent)-1].RequestRate - recent[len(recent)-2].RequestRate
	return
}

// GetStdDevCPU returns standard deviation (FIXED: sqrt variance)
func (h *History) GetStdDevCPU() float64 {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.metrics) < 2 {
		return 0
	}
	var sum float64
	for _, m := range h.metrics {
		sum += m.CPUUsage
	}
	mean := sum / float64(len(h.metrics))

	var variance float64
	for _, m := range h.metrics {
		d := m.CPUUsage - mean
		variance += d * d
	}
	variance /= float64(len(h.metrics))
	return math.Sqrt(variance) // ✅ FIX
}

// GetAverages returns average metrics (FIXED replicas averaging)
func (h *History) GetAverages() dto.Metrics {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.metrics) == 0 {
		return dto.Metrics{}
	}

	var sum dto.Metrics
	var sumReplicas int64

	for _, m := range h.metrics {
		sum.CPUUsage += m.CPUUsage
		sum.MemoryUsage += m.MemoryUsage
		sum.RequestRate += m.RequestRate
		sum.LatencyP50 += m.LatencyP50
		sum.LatencyP95 += m.LatencyP95
		sum.LatencyP99 += m.LatencyP99
		sum.ErrorRate += m.ErrorRate
		sumReplicas += int64(m.Replicas)
	}

	count := float64(len(h.metrics))
	avgReplicas := int32(sumReplicas / int64(len(h.metrics)))

	return dto.Metrics{
		CPUUsage:    sum.CPUUsage / count,
		MemoryUsage: sum.MemoryUsage / count,
		RequestRate: sum.RequestRate / count,
		LatencyP50:  sum.LatencyP50 / count,
		LatencyP95:  sum.LatencyP95 / count,
		LatencyP99:  sum.LatencyP99 / count,
		ErrorRate:   sum.ErrorRate / count,
		Replicas:    avgReplicas,
	}
}