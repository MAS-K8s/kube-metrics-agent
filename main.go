package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go-controller-agent/dto"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)



type MetricsHistory struct {
	mu      sync.RWMutex
	History []dto.Metrics
	MaxSize int
}

func NewMetricsHistory(maxSize int) *MetricsHistory {
	return &MetricsHistory{
		History: make([]dto.Metrics, 0, maxSize),
		MaxSize: maxSize,
	}
}

func (mh *MetricsHistory) Add(m dto.Metrics) {
	mh.mu.Lock()
	defer mh.mu.Unlock()

	mh.History = append(mh.History, m)
	if len(mh.History) > mh.MaxSize {
		mh.History = mh.History[1:]
	}
}

func (mh *MetricsHistory) GetRecent(n int) []dto.Metrics {
	mh.mu.RLock()
	defer mh.mu.RUnlock()

	if n <= 0 || len(mh.History) == 0 {
		return nil
	}
	if len(mh.History) < n {
		out := make([]dto.Metrics, len(mh.History))
		copy(out, mh.History)
		return out
	}
	out := make([]dto.Metrics, n)
	copy(out, mh.History[len(mh.History)-n:])
	return out
}

func (mh *MetricsHistory) CalculateTrends() (cpu1m, cpu5m, requestTrend float64) {
	recent := mh.GetRecent(10)
	if len(recent) < 2 {
		return 0, 0, 0
	}

	cpu1m = recent[len(recent)-1].CPUUsage - recent[len(recent)-2].CPUUsage
	requestTrend = recent[len(recent)-1].RequestRate - recent[len(recent)-2].RequestRate

	if len(recent) >= 10 {
		// average delta per sample across last 10 samples
		cpu5m = (recent[len(recent)-1].CPUUsage - recent[0].CPUUsage) / float64(len(recent))
	}
	return
}

type SafetyController struct {
	mu            sync.Mutex
	scalingEvents []dto.ScalingEvent
	maxPerMinute  int32
}

func NewSafetyController(maxPerMinute int32) *SafetyController {
	return &SafetyController{
		scalingEvents: make([]dto.ScalingEvent, 0, 32),
		maxPerMinute:  maxPerMinute,
	}
}

func (sc *SafetyController) CanScale(current, desired int32) (bool, string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	// Rate limit window
	cutoff := time.Now().Add(-1 * time.Minute)
	valid := sc.scalingEvents[:0]
	for _, e := range sc.scalingEvents {
		if e.Timestamp.After(cutoff) {
			valid = append(valid, e)
		}
	}
	sc.scalingEvents = valid

	if int32(len(sc.scalingEvents)) >= sc.maxPerMinute {
		return false, fmt.Sprintf("rate limit: %d scalings in last minute", len(sc.scalingEvents))
	}

	// Oscillation detection: look at direction of last 3 actions
	if len(sc.scalingEvents) >= 3 {
		last3 := sc.scalingEvents[len(sc.scalingEvents)-3:]
		if isOscillating(last3) {
			return false, "oscillation detected, blocking scaling"
		}
	}

	// Clamp change size
	delta := int32(math.Abs(float64(desired - current)))
	if delta > 3 {
		return false, fmt.Sprintf("change too large: %d replicas", delta)
	}

	return true, ""
}

func (sc *SafetyController) RecordScaling(from, to int32, action, reason string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	sc.scalingEvents = append(sc.scalingEvents, dto.ScalingEvent{
		Timestamp:    time.Now(),
		FromReplicas: from,
		ToReplicas:   to,
		Action:       action,
		Reason:       reason,
	})
}

func isOscillating(events []dto.ScalingEvent) bool {
	if len(events) < 3 {
		return false
	}
	// Direction is based on each event's own from->to
	dir := func(e dto.ScalingEvent) int32 { return e.ToReplicas - e.FromReplicas }
	d1, d2, d3 := dir(events[0]), dir(events[1]), dir(events[2])

	// Up then down OR down then up in consecutive steps (ignore zeros)
	flip := func(a, b int32) bool {
		if a == 0 || b == 0 {
			return false
		}
		return (a > 0 && b < 0) || (a < 0 && b > 0)
	}
	return flip(d1, d2) || flip(d2, d3)
}

// ======================== CONTROLLER STRUCT ========================

type Controller struct {
	cfg       dto.Config
	log       logr.Logger
	clientset *kubernetes.Clientset
	http      *http.Client

	history *MetricsHistory
	safety  *SafetyController
}

func main() {
	cfg, err := parseAndValidateConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}

	zapLog, _ := zap.NewDevelopment()
	defer func() { _ = zapLog.Sync() }()
	log := zapr.NewLogger(zapLog)

	log.Info("🚀 Starting Advanced RL Controller",
		"namespace", cfg.Namespace,
		"deployment", cfg.Deployment,
		"training_mode", cfg.TrainingMode,
		"mock_mode", cfg.MockMode,
		"prometheus", cfg.PromURL,
		"rl_agent", cfg.RLAgentURL,
	)

	clientset, err := buildK8sClient(log)
	if err != nil {
		log.Error(err, "❌ Failed to create Kubernetes client")
		os.Exit(1)
	}

	c := &Controller{
		cfg:       cfg,
		log:       log,
		clientset: clientset,
		http: &http.Client{
			Timeout: 6 * time.Second, // global safety net
		},
		history: NewMetricsHistory(100),
		safety:  NewSafetyController(cfg.MaxScalePerMin),
	}

	// Don’t hard-exit if Prometheus/RL is down; keep running and fallback.
	if !c.cfg.MockMode {
		if err := c.testPrometheus(); err != nil {
			c.log.Error(err, "⚠️ Prometheus unavailable; switching to mock metrics")
			c.cfg.MockMode = true
		}
	}
	if err := c.testRLAgent(); err != nil {
		c.log.Error(err, "⚠️ RL Agent unavailable; will use fallback until it becomes reachable")
	}

	ctx, cancel := signalContext()
	defer cancel()

	c.run(ctx)
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

// ======================== CONFIG ========================

func parseAndValidateConfig() (dto.Config, error) {
	var cfg dto.Config
	var minRepl, maxRepl int

	flag.StringVar(&cfg.Namespace, "namespace", "default", "Kubernetes namespace")
	flag.StringVar(&cfg.Deployment, "deployment", "myapp", "Deployment name")
	flag.StringVar(&cfg.PromURL, "prometheus", "http://34.19.119.245:30457", "Prometheus base URL (no trailing /api)")
	flag.StringVar(&cfg.RLAgentURL, "rl-agent", "http://localhost:5000", "RL Agent base URL")
	flag.DurationVar(&cfg.Interval, "interval", 30*time.Second, "Polling interval")
	flag.IntVar(&minRepl, "min-replicas", 1, "Minimum replicas")
	flag.IntVar(&maxRepl, "max-replicas", 10, "Maximum replicas")
	flag.BoolVar(&cfg.TrainingMode, "training", true, "Enable training mode")
	flag.BoolVar(&cfg.MockMode, "mock", false, "Use mock metrics")
	flag.BoolVar(&cfg.SafetyEnabled, "safety", true, "Enable safety controls")
	flag.StringVar(&cfg.ModelPath, "model-path", "./models/rl_model.pt", "Model path")
	flag.Parse()

	cfg.MinReplicas = int32(minRepl)
	cfg.MaxReplicas = int32(maxRepl)
	cfg.MaxScalePerMin = 3

	if cfg.Namespace == "" || cfg.Deployment == "" {
		return cfg, errors.New("namespace and deployment must not be empty")
	}
	if cfg.MinReplicas < 1 || cfg.MaxReplicas < cfg.MinReplicas {
		return cfg, fmt.Errorf("invalid replicas range: min=%d max=%d", cfg.MinReplicas, cfg.MaxReplicas)
	}
	if cfg.Interval < 5*time.Second {
		return cfg, fmt.Errorf("interval too small (%s). use >= 5s", cfg.Interval)
	}

	// ✅ FIX: validate URLs (catches htttp://)
	if err := validateBaseURL(cfg.PromURL); err != nil {
		return cfg, fmt.Errorf("invalid prometheus url: %w", err)
	}
	if err := validateBaseURL(cfg.RLAgentURL); err != nil {
		return cfg, fmt.Errorf("invalid rl-agent url: %w", err)
	}

	return cfg, nil
}

func validateBaseURL(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http/https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}

// ======================== RUN LOOP ========================

func (c *Controller) run(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()

	c.log.Info("✅ Controller started", "interval", c.cfg.Interval.String())

	// run immediately once
	_ = c.reconcileOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			c.log.Info("🛑 Shutting down controller")
			return
		case <-ticker.C:
			if err := c.reconcileOnce(ctx); err != nil {
				c.log.Error(err, "⚠️ Reconcile failed")
			}
		}
	}
}

func (c *Controller) reconcileOnce(ctx context.Context) error {
	// per-iteration timeout (prevents hanging)
	iterCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	deploy, err := c.clientset.AppsV1().Deployments(c.cfg.Namespace).Get(iterCtx, c.cfg.Deployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment: %w", err)
	}

	metrics, err := c.collectMetrics(iterCtx, deploy)
	if err != nil {
		return fmt.Errorf("collect metrics: %w", err)
	}

	c.history.Add(metrics)

	c.log.Info("📊 Metrics Collected",
		"cpu", fmt.Sprintf("%.3f", metrics.CPUUsage),
		"memory", fmt.Sprintf("%.2f GB", metrics.MemoryUsage),
		"requests/s", fmt.Sprintf("%.1f", metrics.RequestRate),
		"p95_latency", fmt.Sprintf("%.3fs", metrics.LatencyP95),
		"replicas", metrics.Replicas,
		"ready", metrics.PodReady,
		"pending", metrics.PodPending,
		"error_rate", fmt.Sprintf("%.4f", metrics.ErrorRate),
	)

	rlResp, err := c.queryRLAgent(iterCtx, metrics)
	if err != nil {
		c.log.Error(err, "⚠️ RL Agent call failed; using fallback")
		return c.fallbackScaling(iterCtx, metrics, deploy)
	}
	if !rlResp.Success {
		c.log.Info("⚠️ RL response not successful", "message", rlResp.Message)
		return nil
	}

	c.log.Info("🤖 RL Decision",
		"action", rlResp.ActionName,
		"confidence", fmt.Sprintf("%.2f%%", rlResp.Confidence*100),
		"value", fmt.Sprintf("%.2f", rlResp.ValueEstimate),
		"reward", fmt.Sprintf("%.2f", rlResp.Reward),
		"buffer", rlResp.BufferSize,
		"steps", rlResp.TrainingSteps,
	)

	newReplicas := calculateNewReplicas(metrics.Replicas, rlResp.Action, c.cfg)
	if newReplicas == metrics.Replicas {
		c.log.Info("⏸️ No scaling needed", "action", rlResp.ActionName)
		return nil
	}

	// safety
	if c.cfg.SafetyEnabled {
		allowed, reason := c.safety.CanScale(metrics.Replicas, newReplicas)
		if !allowed {
			c.log.Info("🛡️ Scaling blocked by safety controller", "reason", reason)
			return nil
		}
	}

	if err := c.executeScaling(iterCtx, deploy, newReplicas); err != nil {
		return fmt.Errorf("execute scaling: %w", err)
	}

	c.safety.RecordScaling(metrics.Replicas, newReplicas, rlResp.ActionName, "rl_decision")

	c.log.Info("✅ Scaling Executed",
		"from", metrics.Replicas,
		"to", newReplicas,
		"action", rlResp.ActionName,
		"confidence", fmt.Sprintf("%.0f%%", rlResp.Confidence*100),
	)
	return nil
}

// ======================== K8S CLIENT ========================

func buildK8sClient(log logr.Logger) (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := filepath.Join(homeDir(), ".kube", "config")
		log.Info("⚙️ Using local kubeconfig", "path", kubeconfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig: %w", err)
		}
	} else {
		log.Info("🔗 Using in-cluster configuration")
	}
	return kubernetes.NewForConfig(config)
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if runtime.GOOS == "windows" {
		return os.Getenv("USERPROFILE")
	}
	return ""
}

// ======================== METRICS ========================

func (c *Controller) collectMetrics(ctx context.Context, deploy *appsv1.Deployment) (dto.Metrics, error) {
	now := time.Now()

	replicas := int32(1)
	if deploy.Spec.Replicas != nil {
		replicas = *deploy.Spec.Replicas
	}

	var metrics dto.Metrics

	if c.cfg.MockMode {
		metrics = generateMockMetrics(replicas)
		c.log.Info("🎭 Using mock metrics")
	} else {
		// ✅ Don’t ignore errors; if Prom fails, return error and let reconcile fallback.
		cpuUsage, err := c.queryPrometheus(ctx, fmt.Sprintf(
			`avg(rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s-.*"}[2m]))`,
			c.cfg.Namespace, c.cfg.Deployment))
		if err != nil {
			return dto.Metrics{}, fmt.Errorf("prom cpu: %w", err)
		}

		memUsage, err := c.queryPrometheus(ctx, fmt.Sprintf(
			`avg(container_memory_working_set_bytes{namespace="%s",pod=~"%s-.*"}) / (1024*1024*1024)`,
			c.cfg.Namespace, c.cfg.Deployment))
		if err != nil {
			return dto.Metrics{}, fmt.Errorf("prom mem: %w", err)
		}

		requestRate, err := c.queryPrometheus(ctx, fmt.Sprintf(
			`sum(rate(http_requests_total{namespace="%s",deployment="%s"}[2m]))`,
			c.cfg.Namespace, c.cfg.Deployment))
		if err != nil {
			return dto.Metrics{}, fmt.Errorf("prom rps: %w", err)
		}

		latencyP95, err := c.queryPrometheus(ctx, fmt.Sprintf(
			`histogram_quantile(0.95, rate(http_request_duration_seconds_bucket{namespace="%s"}[2m]))`,
			c.cfg.Namespace))
		if err != nil {
			return dto.Metrics{}, fmt.Errorf("prom p95: %w", err)
		}

		errorRate, err := c.queryPrometheus(ctx, fmt.Sprintf(
			`sum(rate(http_requests_total{namespace="%s",status=~"5.."}[2m])) / sum(rate(http_requests_total{namespace="%s"}[2m]))`,
			c.cfg.Namespace, c.cfg.Namespace))
		if err != nil {
			return dto.Metrics{}, fmt.Errorf("prom error_rate: %w", err)
		}

		metrics = dto.Metrics{
			CPUUsage:    cpuUsage,
			MemoryUsage: memUsage,
			RequestRate: requestRate,
			LatencyP95:  latencyP95,
			ErrorRate:   errorRate,
		}
	}

	// pod readiness/pending (best-effort)
	pods, err := c.clientset.CoreV1().Pods(c.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", c.cfg.Deployment),
	})
	pendingCount := int32(0)
	readyCount := int32(0)
	if err == nil {
		for _, pod := range pods.Items {
			switch pod.Status.Phase {
			case "Pending":
				pendingCount++
			case "Running":
				readyCount++
			}
		}
	}

	cpu1m, cpu5m, reqTrend := c.history.CalculateTrends()

	hour := now.Hour()
	dayOfWeek := int(now.Weekday())
	isWeekend := dayOfWeek == 0 || dayOfWeek == 6
	isPeakHour := hour >= 9 && hour <= 17 && !isWeekend

	metrics.Replicas = replicas
	metrics.PodPending = pendingCount
	metrics.PodReady = readyCount
	metrics.Timestamp = now.Unix()
	metrics.CPUTrend1m = cpu1m
	metrics.CPUTrend5m = cpu5m
	metrics.RequestTrend = reqTrend
	metrics.Hour = hour
	metrics.DayOfWeek = dayOfWeek
	metrics.IsWeekend = isWeekend
	metrics.IsPeakHour = isPeakHour

	if err := validateMetrics(metrics); err != nil {
		c.log.Info("⚠️ Metrics validation warning", "error", err.Error())
	}

	return metrics, nil
}

func generateMockMetrics(replicas int32) dto.Metrics {
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

	readyCount := replicas
	pendingCount := int32(0)
	if rand.Float64() < 0.1 {
		pendingCount = 1
		if replicas > 0 {
			readyCount = replicas - 1
		}
	}

	return dto.Metrics{
		CPUUsage:    cpuUsage,
		MemoryUsage: 2.0 + rand.Float64()*0.5,
		RequestRate: requestRate,
		LatencyP95:  latency,
		ErrorRate:   errorRate,
		PodReady:    readyCount,
		PodPending:  pendingCount,
	}
}

func validateMetrics(m dto.Metrics) error {
	if m.CPUUsage < 0 || m.CPUUsage > 1 {
		return fmt.Errorf("invalid CPU: %.2f", m.CPUUsage)
	}
	if m.ErrorRate < 0 || m.ErrorRate > 1 {
		return fmt.Errorf("invalid error rate: %.2f", m.ErrorRate)
	}
	return nil
}

// ======================== RL AGENT ========================

func (c *Controller) queryRLAgent(ctx context.Context, metrics dto.Metrics) (*dto.RLResponse, error) {
	reqData := dto.RLRequest{
		DeploymentName: c.cfg.Deployment,
		Namespace:      c.cfg.Namespace,
		Metrics:        metrics,
		TrainingMode:   c.cfg.TrainingMode,
		Timestamp:      time.Now().Unix(),
	}

	body, err := json.Marshal(reqData)
	if err != nil {
		return nil, fmt.Errorf("marshal rl request: %w", err)
	}

	u := c.cfg.RLAgentURL + "/predict"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("rl http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("rl non-200 status=%d body=%s", resp.StatusCode, string(b))
	}

	var out dto.RLResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode rl response: %w", err)
	}
	return &out, nil
}

// ======================== SCALING ========================

func (c *Controller) executeScaling(ctx context.Context, deploy *appsv1.Deployment, newReplicas int32) error {
	deploy.Spec.Replicas = &newReplicas

	updated, err := c.clientset.AppsV1().Deployments(c.cfg.Namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("deployment update: %w", err)
	}
	if updated.Spec.Replicas == nil || *updated.Spec.Replicas != newReplicas {
		return fmt.Errorf("verification failed: expected %d got %v", newReplicas, updated.Spec.Replicas)
	}
	return nil
}

func calculateNewReplicas(current int32, action int, cfg dto.Config) int32 {
	switch action {
	case 0: // scale_down
		if current > cfg.MinReplicas {
			return current - 1
		}
	case 2: // scale_up
		if current < cfg.MaxReplicas {
			return current + 1
		}
	default: // 1 = no_action
	}
	return current
}

func (c *Controller) fallbackScaling(ctx context.Context, metrics dto.Metrics, deploy *appsv1.Deployment) error {
	newReplicas := metrics.Replicas
	reason := ""

	if metrics.CPUUsage > 0.75 || metrics.LatencyP95 > 0.5 {
		if metrics.Replicas < c.cfg.MaxReplicas {
			newReplicas++
			reason = "fallback_high_load"
		}
	} else if metrics.CPUUsage < 0.30 && metrics.LatencyP95 < 0.2 {
		if metrics.Replicas > c.cfg.MinReplicas {
			newReplicas--
			reason = "fallback_low_load"
		}
	}

	if newReplicas == metrics.Replicas {
		return nil
	}

	if c.cfg.SafetyEnabled {
		allowed, msg := c.safety.CanScale(metrics.Replicas, newReplicas)
		if !allowed {
			c.log.Info("🛡️ Fallback scaling blocked", "reason", msg)
			return nil
		}
	}

	if err := c.executeScaling(ctx, deploy, newReplicas); err != nil {
		return err
	}
	c.safety.RecordScaling(metrics.Replicas, newReplicas, "fallback", reason)
	c.log.Info("✅ Fallback scaling executed", "to", newReplicas, "reason", reason)
	return nil
}

// ======================== PROMETHEUS ========================

func (c *Controller) queryPrometheus(ctx context.Context, promQuery string) (float64, error) {
	escaped := url.QueryEscape(promQuery)
	fullURL := fmt.Sprintf("%s/api/v1/query?query=%s", c.cfg.PromURL, escaped)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return 0, fmt.Errorf("new prom request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("prom http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("prom status=%d body=%s", resp.StatusCode, string(b))
	}

	var promResp dto.PrometheusResponse
	if err := json.NewDecoder(resp.Body).Decode(&promResp); err != nil {
		return 0, fmt.Errorf("decode prom response: %w", err)
	}

	if len(promResp.Data.Result) == 0 {
		return 0, nil
	}

	valueStr, ok := promResp.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("unexpected prom value format")
	}

	var v float64
	if _, err := fmt.Sscanf(valueStr, "%f", &v); err != nil {
		return 0, fmt.Errorf("parse prom float: %w", err)
	}
	return v, nil
}

func (c *Controller) testPrometheus() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.queryPrometheus(ctx, "up")
	if err != nil {
		return err
	}
	c.log.Info("✅ Prometheus connection successful")
	return nil
}

func (c *Controller) testRLAgent() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	u := c.cfg.RLAgentURL + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	c.log.Info("✅ RL Agent connection successful")
	return nil
}