package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Metrics represents collected system metrics
type Metrics struct {
	CPUUsage       float64 `json:"cpu_usage"`
	MemoryUsage    float64 `json:"memory_usage"`
	RequestRate    float64 `json:"request_rate"`
	LatencyP50     float64 `json:"latency_p50"`
	LatencyP95     float64 `json:"latency_p95"`
	LatencyP99     float64 `json:"latency_p99"`
	Replicas       int32   `json:"replicas"`
	ErrorRate      float64 `json:"error_rate"`
	PodPending     int32   `json:"pod_pending"`
	PodReady       int32   `json:"pod_ready"`
	Timestamp      int64   `json:"timestamp"`
	CPUTrend1m     float64 `json:"cpu_trend_1m"`
	CPUTrend5m     float64 `json:"cpu_trend_5m"`
	RequestTrend   float64 `json:"request_trend"`
	Hour           int     `json:"hour"`
	DayOfWeek      int     `json:"day_of_week"`
	IsWeekend      bool    `json:"is_weekend"`
	IsPeakHour     bool    `json:"is_peak_hour"`
}

// Collector handles metrics collection from various sources
type Collector struct {
	prometheusURL string
	mockMode      bool
	logger        logr.Logger
	httpClient    *http.Client
}

// PrometheusResponse represents Prometheus API response
type PrometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Value []interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// NewCollector creates a new metrics collector
func NewCollector(prometheusURL string, mockMode bool, logger logr.Logger) *Collector {
	return &Collector{
		prometheusURL: prometheusURL,
		mockMode:      mockMode,
		logger:        logger,
		httpClient: &http.Client{
			Timeout: 3 * time.Second,
		},
	}
}

// Collect gathers all metrics for a deployment
func (c *Collector) Collect(
	clientset *kubernetes.Clientset,
	namespace string,
	deploymentName string,
	deployment *appsv1.Deployment,
	history *History,
) (Metrics, error) {
	ctx := context.Background()
	now := time.Now()

	// Get current replicas
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}

	var metrics Metrics

	if c.mockMode {
		metrics = c.generateMockMetrics(replicas)
		c.logger.Info("🎭 Using mock metrics")
	} else {
		metrics = c.collectRealMetrics(namespace, deploymentName)
	}

	// Collect pod status
	pendingCount, readyCount := c.collectPodStatus(clientset, ctx, namespace, deploymentName)
	metrics.PodPending = pendingCount
	metrics.PodReady = readyCount
	metrics.Replicas = replicas
	metrics.Timestamp = now.Unix()

	// Calculate trends from history
	cpu1m, cpu5m, reqTrend := history.CalculateTrends()
	metrics.CPUTrend1m = cpu1m
	metrics.CPUTrend5m = cpu5m
	metrics.RequestTrend = reqTrend

	// Add time context
	metrics.Hour = now.Hour()
	metrics.DayOfWeek = int(now.Weekday())
	metrics.IsWeekend = metrics.DayOfWeek == 0 || metrics.DayOfWeek == 6
	metrics.IsPeakHour = metrics.Hour >= 9 && metrics.Hour <= 17 && !metrics.IsWeekend

	return metrics, nil
}

// collectRealMetrics queries Prometheus for real metrics
func (c *Collector) collectRealMetrics(namespace, deployment string) Metrics {
	queries := map[string]string{
		"cpu": fmt.Sprintf(
			`avg(rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s-.*"}[2m]))`,
			namespace, deployment,
		),
		"memory": fmt.Sprintf(
			`avg(container_memory_working_set_bytes{namespace="%s",pod=~"%s-.*"}) / (1024*1024*1024)`,
			namespace, deployment,
		),
		"request_rate": fmt.Sprintf(
			`sum(rate(http_requests_total{namespace="%s",deployment="%s"}[2m]))`,
			namespace, deployment,
		),
		"latency_p50": fmt.Sprintf(
			`histogram_quantile(0.50, rate(http_request_duration_seconds_bucket{namespace="%s"}[2m]))`,
			namespace,
		),
		"latency_p95": fmt.Sprintf(
			`histogram_quantile(0.95, rate(http_request_duration_seconds_bucket{namespace="%s"}[2m]))`,
			namespace,
		),
		"latency_p99": fmt.Sprintf(
			`histogram_quantile(0.99, rate(http_request_duration_seconds_bucket{namespace="%s"}[2m]))`,
			namespace,
		),
		"error_rate": fmt.Sprintf(
			`sum(rate(http_requests_total{namespace="%s",status=~"5.."}[2m])) / sum(rate(http_requests_total{namespace="%s"}[2m]))`,
			namespace, namespace,
		),
	}

	metrics := Metrics{}

	for name, query := range queries {
		value, err := c.queryPrometheus(query)
		if err != nil {
			c.logger.V(1).Info("Prometheus query failed", "metric", name, "error", err)
			value = 0
		}

		switch name {
		case "cpu":
			metrics.CPUUsage = value
		case "memory":
			metrics.MemoryUsage = value
		case "request_rate":
			metrics.RequestRate = value
		case "latency_p50":
			metrics.LatencyP50 = value
		case "latency_p95":
			metrics.LatencyP95 = value
		case "latency_p99":
			metrics.LatencyP99 = value
		case "error_rate":
			metrics.ErrorRate = value
		}
	}

	return metrics
}

// queryPrometheus executes a Prometheus query
func (c *Collector) queryPrometheus(query string) (float64, error) {
	url := fmt.Sprintf("%s/api/v1/query?query=%s", c.prometheusURL, query)

	resp, err := c.httpClient.Get(url)
	if err != nil {
		return 0, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("non-200 status: %d", resp.StatusCode)
	}

	var promResp PrometheusResponse
	if err := json.NewDecoder(resp.Body).Decode(&promResp); err != nil {
		return 0, fmt.Errorf("decode failed: %w", err)
	}

	if len(promResp.Data.Result) == 0 {
		return 0, nil
	}

	valueStr, ok := promResp.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("invalid value format")
	}

	var value float64
	if _, err := fmt.Sscanf(valueStr, "%f", &value); err != nil {
		return 0, fmt.Errorf("parse failed: %w", err)
	}

	return value, nil
}

// collectPodStatus gets pod counts by status
func (c *Collector) collectPodStatus(
	clientset *kubernetes.Clientset,
	ctx context.Context,
	namespace string,
	deploymentName string,
) (pending, ready int32) {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", deploymentName),
	})

	if err != nil {
		c.logger.Error(err, "Failed to list pods")
		return 0, 0
	}

	for _, pod := range pods.Items {
		switch pod.Status.Phase {
		case "Pending":
			pending++
		case "Running":
			ready++
		}
	}

	return pending, ready
}

// generateMockMetrics creates synthetic metrics for testing
func (c *Collector) generateMockMetrics(replicas int32) Metrics {
	// Simulate daily load pattern
	baseLoad := 0.5 + 0.3*math.Sin(float64(time.Now().Unix())/3600.0)

	cpuUsage := baseLoad + (rand.Float64()-0.5)*0.1
	cpuUsage = math.Max(0.1, math.Min(0.95, cpuUsage))

	requestRate := 100.0 + 50.0*math.Sin(float64(time.Now().Unix())/1800.0)
	requestRate = math.Max(10, requestRate)

	latency := 0.2 + 0.3*cpuUsage
	errorRate := 0.001
	if cpuUsage > 0.8 {
		errorRate = 0.01 * (cpuUsage - 0.8) / 0.2
	}

	return Metrics{
		CPUUsage:    cpuUsage,
		MemoryUsage: 2.0 + rand.Float64()*0.5,
		RequestRate: requestRate,
		LatencyP50:  latency * 0.7,
		LatencyP95:  latency,
		LatencyP99:  latency * 1.2,
		ErrorRate:   errorRate,
	}
}

// TestPrometheus checks if Prometheus is accessible
func (c *Collector) TestPrometheus() error {
	resp, err := c.httpClient.Get(c.prometheusURL + "/api/v1/query?query=up")
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("unhealthy status: %d", resp.StatusCode)
	}

	c.logger.Info("✅ Prometheus connection successful")
	return nil
}