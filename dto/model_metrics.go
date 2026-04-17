package dto

type Metrics struct {
	CPUUsage    float64 `json:"cpu_usage"`
	MemoryUsage float64 `json:"memory_usage"`

	RequestRate  float64 `json:"request_rate"`
	RequestTrend float64 `json:"request_trend"`

	LatencyP50 float64 `json:"latency_p50"`
	LatencyP95 float64 `json:"latency_p95"`
	LatencyP99 float64 `json:"latency_p99"`

	ErrorRate float64 `json:"error_rate"`

	Replicas   int32 `json:"replicas"`
	PodReady   int32 `json:"pod_ready"`
	PodPending int32 `json:"pod_pending"`

	CPUTrend1m float64 `json:"cpu_trend_1m"`
	CPUTrend5m float64 `json:"cpu_trend_5m"`

	Hour       int  `json:"hour"`
	DayOfWeek  int  `json:"day_of_week"`
	IsWeekend  bool `json:"is_weekend"`
	IsPeakHour bool `json:"is_peak_hour"`
	Timestamp int64 `json:"timestamp"`
}




