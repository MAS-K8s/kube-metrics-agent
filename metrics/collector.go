package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"time"

	"go-controller-agent/dto"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ✅ Compatibility aliases (so other files that use metrics.Metrics still compile)
type Metrics = dto.Metrics
type PrometheusResponse = dto.PrometheusResponse

type Collector struct {
	prometheusURL string
	mockMode      bool
	logger        logr.Logger
	httpClient    *http.Client

	retryAttempts int
	baseDelay     time.Duration
	maxDelay      time.Duration
}

func NewCollector(
	prometheusURL string,
	mockMode bool,
	timeout time.Duration,
	retryAttempts int,
	baseDelay time.Duration,
	maxDelay time.Duration,
	logger logr.Logger,
) *Collector {
	return &Collector{
		prometheusURL: prometheusURL,
		mockMode:      mockMode,
		logger:        logger,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		retryAttempts: retryAttempts,
		baseDelay:     baseDelay,
		maxDelay:      maxDelay,
	}
}

// ✅ Startup health check for Prometheus
func (c *Collector) CheckPrometheus(ctx context.Context) error {
	// Quick sanity query
	_, err := c.queryPrometheusWithRetry(ctx, "up")
	if err != nil {
		return fmt.Errorf("prometheus health check failed: %w", err)
	}
	return nil
}

// Collect gathers metrics for a deployment (CTX PROPAGATED)
func (c *Collector) Collect(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	namespace string,
	deploymentName string,
	deployment *appsv1.Deployment,
	history *History,
) (Metrics, error) {
	now := time.Now()

	// replicas
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}

	var m Metrics
	if c.mockMode {
		m = c.generateMockMetrics(replicas)
	} else {
		real, err := c.collectRealMetrics(ctx, namespace, deploymentName)
		if err != nil {
			return Metrics{}, err
		}
		m = real
	}

	// Pod status (best-effort)
	pendingCount, readyCount := c.collectPodStatus(ctx, clientset, namespace, deploymentName)
	m.PodPending = pendingCount
	m.PodReady = readyCount

	m.Replicas = replicas
	m.Timestamp = now.Unix()

	// trends
	cpu1m, cpu5m, reqTrend := history.CalculateTrends()
	m.CPUTrend1m = cpu1m
	m.CPUTrend5m = cpu5m
	m.RequestTrend = reqTrend

	// time features
	m.Hour = now.Hour()
	m.DayOfWeek = int(now.Weekday())
	m.IsWeekend = m.DayOfWeek == 0 || m.DayOfWeek == 6
	m.IsPeakHour = m.Hour >= 9 && m.Hour <= 17 && !m.IsWeekend

	return m, nil
}

func (c *Collector) collectRealMetrics(ctx context.Context, namespace, deployment string) (Metrics, error) {
	queries := map[string]string{
		"cpu": fmt.Sprintf(
			`avg(rate(container_cpu_usage_seconds_total{namespace=%q,pod=~%q}[2m]))`,
			namespace, deployment+"-.*",
		),
		"memory": fmt.Sprintf(
			`avg(container_memory_working_set_bytes{namespace=%q,pod=~%q}) / (1024*1024*1024)`,
			namespace, deployment+"-.*",
		),
		"request_rate": fmt.Sprintf(
			`sum(rate(http_requests_total{namespace=%q,deployment=%q}[2m]))`,
			namespace, deployment,
		),
		"latency_p50": fmt.Sprintf(
			`histogram_quantile(0.50, rate(http_request_duration_seconds_bucket{namespace=%q}[2m]))`,
			namespace,
		),
		"latency_p95": fmt.Sprintf(
			`histogram_quantile(0.95, rate(http_request_duration_seconds_bucket{namespace=%q}[2m]))`,
			namespace,
		),
		"latency_p99": fmt.Sprintf(
			`histogram_quantile(0.99, rate(http_request_duration_seconds_bucket{namespace=%q}[2m]))`,
			namespace,
		),
		"error_rate": fmt.Sprintf(
			`sum(rate(http_requests_total{namespace=%q,status=~"5.."}[2m])) / sum(rate(http_requests_total{namespace=%q}[2m]))`,
			namespace, namespace,
		),
	}

	out := Metrics{}
	for name, q := range queries {
		val, err := c.queryPrometheusWithRetry(ctx, q)
		if err != nil {
			return Metrics{}, fmt.Errorf("prometheus query failed metric=%s: %w", name, err)
		}
		switch name {
		case "cpu":
			out.CPUUsage = val
		case "memory":
			out.MemoryUsage = val
		case "request_rate":
			out.RequestRate = val
		case "latency_p50":
			out.LatencyP50 = val
		case "latency_p95":
			out.LatencyP95 = val
		case "latency_p99":
			out.LatencyP99 = val
		case "error_rate":
			out.ErrorRate = val
		}
	}
	return out, nil
}

// ✅ retry + exponential backoff
func (c *Collector) queryPrometheusWithRetry(ctx context.Context, promQL string) (float64, error) {
	delay := c.baseDelay
	var lastErr error

	for attempt := 1; attempt <= c.retryAttempts; attempt++ {
		val, err := c.queryPrometheus(ctx, promQL)
		if err == nil {
			return val, nil
		}
		lastErr = err

		if attempt < c.retryAttempts {
			if delay > c.maxDelay {
				delay = c.maxDelay
			}
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(delay):
			}
			delay = time.Duration(float64(delay) * 1.7)
		}
	}
	return 0, lastErr
}

// ✅ Proper URL escaping (FIX for your current bug)
func (c *Collector) queryPrometheus(ctx context.Context, promQL string) (float64, error) {
	base, err := url.Parse(c.prometheusURL)
	if err != nil {
		return 0, fmt.Errorf("invalid prometheus url: %w", err)
	}
	base.Path = "/api/v1/query"
	q := base.Query()
	q.Set("query", promQL)
	base.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("create request failed: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("non-200 status: %d", resp.StatusCode)
	}

	var pr PrometheusResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return 0, fmt.Errorf("decode failed: %w", err)
	}

	if len(pr.Data.Result) == 0 || len(pr.Data.Result[0].Value) < 2 {
		return 0, nil
	}

	valueStr, ok := pr.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("invalid value type")
	}

	var value float64
	if _, err := fmt.Sscanf(valueStr, "%f", &value); err != nil {
		return 0, fmt.Errorf("parse value failed: %w", err)
	}
	return value, nil
}

func (c *Collector) collectPodStatus(ctx context.Context, clientset *kubernetes.Clientset, namespace, deploymentName string) (pending, ready int32) {
	// NOTE: your old logic used app=<deployment>. That can be wrong in real clusters.
	// We keep it for compatibility, but if your pods don't have app=<deployment>, update it later.
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", deploymentName),
	})
	if err != nil {
		c.logger.V(1).Info("list pods failed", "namespace", namespace, "deployment", deploymentName, "err", err)
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

func (c *Collector) generateMockMetrics(replicas int32) Metrics {
	baseLoad := 0.5 + 0.3*math.Sin(float64(time.Now().Unix())/3600.0)

	cpu := baseLoad + (rand.Float64()-0.5)*0.1
	cpu = math.Max(0.1, math.Min(0.95, cpu))

	req := 100.0 + 50.0*math.Sin(float64(time.Now().Unix())/1800.0)
	req = math.Max(10, req)

	lat := 0.2 + 0.3*cpu
	errRate := 0.001
	if cpu > 0.8 {
		errRate = 0.01 * (cpu - 0.8) / 0.2
	}

	return Metrics{
		CPUUsage:    cpu,
		MemoryUsage: 2.0 + rand.Float64()*0.5,
		RequestRate: req,
		LatencyP50:  lat * 0.7,
		LatencyP95:  lat,
		LatencyP99:  lat * 1.2,
		ErrorRate:   errRate,
		Replicas:    replicas,
	}
}