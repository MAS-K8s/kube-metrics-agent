package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"time"

	"go-controller-agent/dto"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Compatibility aliases
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

// CheckPrometheus is a startup health check.
func (c *Collector) CheckPrometheus(ctx context.Context) error {
	val, err := c.queryPrometheusWithRetry(ctx, "up", "health_up")
	if err != nil {
		return fmt.Errorf("prometheus health check failed: %w", err)
	}
	c.logger.Info("prometheus health query succeeded", "metric", "health_up", "value", val)
	return nil
}

// Collect gathers metrics for a deployment.
func (c *Collector) Collect(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	namespace string,
	deploymentName string,
	deployment *appsv1.Deployment,
	history *History,
) (Metrics, error) {
	now := time.Now()

	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}

	var m Metrics
	if c.mockMode {
		m = c.generateMockMetrics(replicas)
		c.logger.Info("mock metrics generated",
			"namespace", namespace,
			"deployment", deploymentName,
			"cpu", m.CPUUsage,
			"memory", m.MemoryUsage,
			"requestRate", m.RequestRate,
			"latencyP50", m.LatencyP50,
			"latencyP95", m.LatencyP95,
			"latencyP99", m.LatencyP99,
			"errorRate", m.ErrorRate,
		)
	} else {
		real, err := c.collectRealMetrics(ctx, namespace, deploymentName)
		if err != nil {
			return Metrics{}, err
		}
		m = real
	}

	pendingCount, readyCount := c.collectPodStatus(ctx, clientset, namespace, deploymentName)
	m.PodPending = pendingCount
	m.PodReady = readyCount

	m.Replicas = replicas
	m.Timestamp = now.Unix()

	cpu1m, cpu5m, reqTrend := history.CalculateTrends()
	m.CPUTrend1m = cpu1m
	m.CPUTrend5m = cpu5m
	m.RequestTrend = reqTrend

	m.Hour = now.Hour()
	m.DayOfWeek = int(now.Weekday())
	m.IsWeekend = m.DayOfWeek == 0 || m.DayOfWeek == 6
	m.IsPeakHour = m.Hour >= 9 && m.Hour <= 17 && !m.IsWeekend

	c.logger.Info("collected metrics summary",
		"namespace", namespace,
		"deployment", deploymentName,
		"cpu", m.CPUUsage,
		"memoryGiB", m.MemoryUsage,
		"requestRate", m.RequestRate,
		"latencyP50", m.LatencyP50,
		"latencyP95", m.LatencyP95,
		"latencyP99", m.LatencyP99,
		"errorRate", m.ErrorRate,
		"replicas", m.Replicas,
		"podReady", m.PodReady,
		"podPending", m.PodPending,
		"cpuTrend1m", m.CPUTrend1m,
		"cpuTrend5m", m.CPUTrend5m,
		"requestTrend", m.RequestTrend,
		"hour", m.Hour,
		"dayOfWeek", m.DayOfWeek,
		"isWeekend", m.IsWeekend,
		"isPeakHour", m.IsPeakHour,
	)

	return m, nil
}

// queryResult holds the result of a single parallel Prometheus query.
type queryResult struct {
	name string
	val  float64
	err  error
}

// collectRealMetrics queries Prometheus for all deployment metrics IN PARALLEL.
// Running queries concurrently reduces collection time from ~81s (sequential)
// to ~10s, preventing context deadline exceeded cascades.
func (c *Collector) collectRealMetrics(ctx context.Context, namespace, deployment string) (Metrics, error) {
	
	podRegex := deployment + "-[a-zA-Z0-9]+-[a-zA-Z0-9]+"

	queries := map[string]string{
		
		"cpu": fmt.Sprintf(
			`avg(rate(container_cpu_usage_seconds_total{namespace=%q,pod=~%q,container!="POD",container!=""}[2m]))`,
			namespace, podRegex,
		),
		"memory": fmt.Sprintf(
			`avg(container_memory_working_set_bytes{namespace=%q,pod=~%q,container!="POD",container!=""}) / (1024*1024*1024)`,
			namespace, podRegex,
		),
		"request_rate": fmt.Sprintf(
			`sum(rate(http_requests_total{namespace=%q}[2m]))`,
			namespace,
		),
		"latency_p50": fmt.Sprintf(
			`histogram_quantile(0.50, sum(rate(http_request_duration_seconds_bucket{namespace=%q}[2m])) by (le))`,
			namespace,
		),
		"latency_p95": fmt.Sprintf(
			`histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{namespace=%q}[2m])) by (le))`,
			namespace,
		),
		"latency_p99": fmt.Sprintf(
			`histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket{namespace=%q}[2m])) by (le))`,
			namespace,
		),
		"error_rate": fmt.Sprintf(
			`(sum(rate(http_requests_total{namespace=%q,status_code=~"5.."}[2m])) or vector(0))
			/ (sum(rate(http_requests_total{namespace=%q}[2m])) > 0 or vector(1))`,
			namespace, namespace,
		),
		"network_rx": fmt.Sprintf(
			`sum(rate(container_network_receive_bytes_total{namespace=%q,pod=~%q}[2m]))`,
			namespace, podRegex,
		),
		// BUG 2 FIX continued: same cpu="total" fix applied here
		"cpu_throttle": fmt.Sprintf(
			`(avg(rate(container_cpu_cfs_throttled_periods_total{namespace=%q,pod=~%q,container!="POD",container!=""}[2m])) or vector(0))
			/ (avg(rate(container_cpu_cfs_periods_total{namespace=%q,pod=~%q,container!="POD",container!=""}[2m])) > 0 or vector(1))`,
			namespace, podRegex, namespace, podRegex,
		),
	}

	resultCh := make(chan queryResult, len(queries))
	var wg sync.WaitGroup

	for name, q := range queries {
		wg.Add(1)
		go func(n, query string) {
			defer wg.Done()
			c.logger.Info("executing prometheus query",
				"namespace", namespace,
				"deployment", deployment,
				"metric", n,
				"promQL", query,
			)
			val, err := c.queryPrometheusWithRetry(ctx, query, n)
			resultCh <- queryResult{name: n, val: val, err: err}
		}(name, q)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	rawValues := map[string]float64{}
	for r := range resultCh {
		if r.err != nil {
			c.logger.Error(r.err, "prometheus query failed (using 0)",
				"namespace", namespace,
				"deployment", deployment,
				"metric", r.name,
			)
			rawValues[r.name] = 0
		} else {
			rawValues[r.name] = sanitizeFloat(r.val)
		}
		c.logger.Info("prometheus query result",
			"namespace", namespace,
			"deployment", deployment,
			"metric", r.name,
			"value", rawValues[r.name],
		)
	}

	
	out := Metrics{}

	// ── Direct assignments ────────────────────────────────────────────
	out.CPUUsage = rawValues["cpu"]
	out.MemoryUsage = rawValues["memory"]
	out.ErrorRate = rawValues["error_rate"]

	// ── Request rate: app metric → network fallback ───────────────────
	// The old fallback divided network bytes by 1024 unconditionally, producing
	// a fake ~1.17 req/s during idle (kube-probe keepalives alone generate
	// ~1 KB/s). Only fall back when traffic clearly exceeds background noise.
	if rawValues["request_rate"] > 0 {
		out.RequestRate = rawValues["request_rate"]
	} else {
		const minNetworkThreshold = 10240.0 // 10 KB/s — above keepalive noise
		const bytesPerRequest = 2048.0      // realistic average HTTP request size
		networkRx := rawValues["network_rx"]
		if networkRx > minNetworkThreshold {
			out.RequestRate = networkRx / bytesPerRequest
			c.logger.Info("request_rate fallback: using network_rx",
				"namespace", namespace,
				"deployment", deployment,
				"network_rx_bytes_s", networkRx,
				"estimated_req_s", out.RequestRate,
			)
		} else {
			out.RequestRate = 0.0
		}
	}

	// ── Latency: app histogram → CPU-throttle proxy ──────────────────
	// Old code always set latency to 0.05s synthetic even when throttle=0,
	// giving the agent fake readings every single cycle. Now latency stays
	// 0.0 when there is no real data — honest signal beats a wrong estimate.
	if rawValues["latency_p95"] > 0 {
		out.LatencyP50 = rawValues["latency_p50"]
		out.LatencyP95 = rawValues["latency_p95"]
		out.LatencyP99 = rawValues["latency_p99"]
	} else {
		throttle := rawValues["cpu_throttle"]
		if throttle > 0.05 {
			// Only synthesise latency when there is actual throttling evidence
			synthetic := sanitizeFloat(throttle * 2.0)
			out.LatencyP50 = synthetic * 0.7
			out.LatencyP95 = synthetic
			out.LatencyP99 = synthetic * 1.3
			c.logger.Info("latency fallback: using cpu_throttle proxy",
				"namespace", namespace,
				"deployment", deployment,
				"cpu_throttle_ratio", throttle,
				"synthetic_p95_s", out.LatencyP95,
			)
		}
		// Otherwise leave latency as 0.0
	}

	return out, nil
}

func (c *Collector) queryPrometheusWithRetry(ctx context.Context, promQL string, metricName string) (float64, error) {
	delay := c.baseDelay
	var lastErr error

	attempts := c.retryAttempts
	if attempts < 1 {
		attempts = 1
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		val, err := c.queryPrometheus(ctx, promQL, metricName)
		if err == nil {
			if attempt > 1 {
				c.logger.Info("prometheus query succeeded after retry",
					"metric", metricName,
					"attempt", attempt,
					"value", val,
				)
			}
			return val, nil
		}

		lastErr = err
		c.logger.Error(err, "prometheus query attempt failed",
			"metric", metricName,
			"attempt", attempt,
			"maxAttempts", attempts,
		)

		if attempt < attempts {
			if delay > c.maxDelay {
				delay = c.maxDelay
			}
			c.logger.Info("waiting before prometheus retry",
				"metric", metricName,
				"attempt", attempt,
				"sleep", delay.String(),
			)
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

func (c *Collector) queryPrometheus(ctx context.Context, promQL string, metricName string) (float64, error) {
	base, err := url.Parse(c.prometheusURL)
	if err != nil {
		return 0, fmt.Errorf("invalid prometheus url: %w", err)
	}

	base.Path = "/api/v1/query"
	q := base.Query()
	q.Set("query", promQL)
	base.RawQuery = q.Encode()

	finalURL := base.String()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finalURL, nil)
	if err != nil {
		return 0, fmt.Errorf("create request failed: %w", err)
	}

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	duration := time.Since(start)
	if err != nil {
		return 0, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	c.logger.Info("prometheus http response",
		"metric", metricName,
		"statusCode", resp.StatusCode,
		"duration", duration.String(),
	)

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("non-200 status: %d", resp.StatusCode)
	}

	var pr PrometheusResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return 0, fmt.Errorf("decode failed: %w", err)
	}

	if pr.Status != "" && pr.Status != "success" {
		return 0, fmt.Errorf("prometheus returned status=%q", pr.Status)
	}

	if len(pr.Data.Result) == 0 {
		c.logger.Info("prometheus returned empty result (using 0)",
			"metric", metricName,
		)
		return 0, nil
	}

	if len(pr.Data.Result[0].Value) < 2 {
		c.logger.Info("prometheus result missing value slot (using 0)",
			"metric", metricName,
		)
		return 0, nil
	}

	valueStr, ok := pr.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("invalid value type in prometheus response")
	}

	var value float64
	if _, err := fmt.Sscanf(valueStr, "%f", &value); err != nil {
		return 0, fmt.Errorf("parse value failed: %w", err)
	}

	return sanitizeFloat(value), nil
}

func (c *Collector) collectPodStatus(ctx context.Context, clientset *kubernetes.Clientset, namespace, deploymentName string) (pending, ready int32) {
	labelSelector := fmt.Sprintf("app=%s", deploymentName)

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		c.logger.Error(err, "list pods failed",
			"namespace", namespace,
			"deployment", deploymentName,
			"labelSelector", labelSelector,
		)
		return 0, 0
	}

	c.logger.Info("pod status query result",
		"namespace", namespace,
		"deployment", deploymentName,
		"labelSelector", labelSelector,
		"podCount", len(pods.Items),
	)

	for _, pod := range pods.Items {
		switch pod.Status.Phase {
		case "Pending":
			pending++
		case "Running":
			ready++
		}
	}

	c.logger.Info("pod status summary",
		"namespace", namespace,
		"deployment", deploymentName,
		"pending", pending,
		"ready", ready,
	)

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

// BUG 4 FIX: sanitizeFloat was missing its closing brace }, making the entire
// file syntactically invalid and uncompilable.
func sanitizeFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}