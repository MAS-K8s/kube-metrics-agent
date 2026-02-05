package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
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

// ======================== CONFIGURATION ========================

type Config struct {
	Namespace      string
	Deployment     string
	PromURL        string
	RLAgentURL     string
	Interval       time.Duration
	MinReplicas    int32
	MaxReplicas    int32
	TrainingMode   bool
	MockMode       bool
	SafetyEnabled  bool
	MaxScalePerMin int32
	ModelPath      string
}

// ======================== DATA STRUCTURES ========================

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

type MetricsHistory struct {
	History []Metrics
	MaxSize int
	mu      sync.RWMutex
}

type RLRequest struct {
	DeploymentName string  `json:"deployment_name"`
	Namespace      string  `json:"namespace"`
	Metrics        Metrics `json:"metrics"`
	TrainingMode   bool    `json:"training_mode"`
	Timestamp      int64   `json:"timestamp"`
}

type RLResponse struct {
	Action              int       `json:"action"`
	ActionName          string    `json:"action_name"`
	Confidence          float64   `json:"confidence"`
	Reward              float64   `json:"reward,omitempty"`
	Epsilon             float64   `json:"epsilon,omitempty"`
	QValues             []float64 `json:"q_values,omitempty"`
	ValueEstimate       float64   `json:"value_estimate,omitempty"`
	ActionProbabilities []float64 `json:"action_probabilities,omitempty"`
	Success             bool      `json:"success"`
	Message             string    `json:"message,omitempty"`
	BufferSize          int       `json:"buffer_size,omitempty"`
	TrainingSteps       int       `json:"training_steps,omitempty"`
}

type PrometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Value []interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

type ScalingEvent struct {
	Timestamp    time.Time
	FromReplicas int32
	ToReplicas   int32
	Action       string
	Reason       string
}

type SafetyController struct {
	scalingEvents []ScalingEvent
	mu            sync.Mutex
	maxPerMinute  int32
}

// ======================== METRICS HISTORY ========================

func NewMetricsHistory(maxSize int) *MetricsHistory {
	return &MetricsHistory{
		History: make([]Metrics, 0, maxSize),
		MaxSize: maxSize,
	}
}

func (mh *MetricsHistory) Add(m Metrics) {
	mh.mu.Lock()
	defer mh.mu.Unlock()

	mh.History = append(mh.History, m)
	if len(mh.History) > mh.MaxSize {
		mh.History = mh.History[1:]
	}
}

func (mh *MetricsHistory) GetRecent(n int) []Metrics {
	mh.mu.RLock()
	defer mh.mu.RUnlock()

	if len(mh.History) < n {
		return mh.History
	}
	return mh.History[len(mh.History)-n:]
}

func (mh *MetricsHistory) CalculateTrends() (cpu1m, cpu5m, requestTrend float64) {
	recent := mh.GetRecent(10)
	if len(recent) < 2 {
		return 0, 0, 0
	}

	if len(recent) >= 2 {
		cpu1m = recent[len(recent)-1].CPUUsage - recent[len(recent)-2].CPUUsage
	}

	if len(recent) >= 10 {
		cpu5m = (recent[len(recent)-1].CPUUsage - recent[0].CPUUsage) / 10.0
	}

	if len(recent) >= 2 {
		requestTrend = recent[len(recent)-1].RequestRate - recent[len(recent)-2].RequestRate
	}

	return
}

// ======================== SAFETY CONTROLLER ========================

func NewSafetyController(maxPerMinute int32) *SafetyController {
	return &SafetyController{
		scalingEvents: make([]ScalingEvent, 0),
		maxPerMinute:  maxPerMinute,
	}
}

func (sc *SafetyController) CanScale(current, desired int32, reason string) (bool, string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	cutoff := time.Now().Add(-1 * time.Minute)
	validEvents := make([]ScalingEvent, 0)
	for _, e := range sc.scalingEvents {
		if e.Timestamp.After(cutoff) {
			validEvents = append(validEvents, e)
		}
	}
	sc.scalingEvents = validEvents

	if int32(len(sc.scalingEvents)) >= sc.maxPerMinute {
		return false, fmt.Sprintf("Rate limit: %d scalings in last minute", len(sc.scalingEvents))
	}

	if len(sc.scalingEvents) >= 3 {
		last3 := sc.scalingEvents[len(sc.scalingEvents)-3:]
		if isOscillating(last3) {
			return false, "Detected oscillating behavior, blocking to stabilize"
		}
	}

	delta := int32(math.Abs(float64(desired - current)))
	if delta > 3 {
		return false, fmt.Sprintf("Change too large: %d replicas", delta)
	}

	return true, ""
}

func (sc *SafetyController) RecordScaling(from, to int32, action, reason string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	sc.scalingEvents = append(sc.scalingEvents, ScalingEvent{
		Timestamp:    time.Now(),
		FromReplicas: from,
		ToReplicas:   to,
		Action:       action,
		Reason:       reason,
	})
}

func isOscillating(events []ScalingEvent) bool {
	if len(events) < 3 {
		return false
	}

	dir1 := events[1].ToReplicas - events[0].FromReplicas
	dir2 := events[2].ToReplicas - events[1].FromReplicas

	return (dir1 > 0 && dir2 < 0) || (dir1 < 0 && dir2 > 0)
}

// ======================== MAIN ========================

func main() {
	var cfg Config
	var minRepl, maxRepl int

	flag.StringVar(&cfg.Namespace, "namespace", "default", "Kubernetes namespace")
	flag.StringVar(&cfg.Deployment, "deployment", "myapp", "Deployment name")
	// flag.StringVar(&cfg.PromURL, "prometheus", "http://prometheus:9090", "Prometheus URL")
	flag.StringVar(&cfg.PromURL, "prometheus", "http://34.145.46.139:30457", "Prometheus URL")
	flag.StringVar(&cfg.RLAgentURL, "rl-agent", "http://localhost:5000", "RL Agent URL")
	flag.DurationVar(&cfg.Interval, "interval", 30*time.Second, "Polling interval")
	flag.IntVar(&minRepl, "min-replicas", 1, "Minimum replicas")
	flag.IntVar(&maxRepl, "max-replicas", 10, "Maximum replicas")
	flag.BoolVar(&cfg.TrainingMode, "training", true, "Enable training mode")
	flag.BoolVar(&cfg.MockMode, "mock", false, "Use mock metrics for testing")
	flag.BoolVar(&cfg.SafetyEnabled, "safety", true, "Enable safety controls")
	flag.StringVar(&cfg.ModelPath, "model-path", "./models/rl_model.pt", "Model path")
	flag.Parse()

	cfg.MinReplicas = int32(minRepl)
	cfg.MaxReplicas = int32(maxRepl)
	cfg.MaxScalePerMin = 3

	zapLog, _ := zap.NewDevelopment()
	defer zapLog.Sync()
	logger := zapr.NewLogger(zapLog)

	logger.Info("🚀 Starting Advanced RL Controller",
		"namespace", cfg.Namespace,
		"deployment", cfg.Deployment,
		"training_mode", cfg.TrainingMode,
		"mock_mode", cfg.MockMode)

	clientset, err := buildK8sClient(logger)
	if err != nil {
		logger.Error(err, "❌ Failed to create Kubernetes client")
		os.Exit(1)
	}

	if !cfg.MockMode {
		if err := testPrometheus(cfg.PromURL, logger); err != nil {
			logger.Error(err, "⚠️ Prometheus unavailable, enabling mock mode")
			cfg.MockMode = true
		}
	}

	history := NewMetricsHistory(100)
	safety := NewSafetyController(cfg.MaxScalePerMin)

	if err := testRLAgent(cfg.RLAgentURL, logger); err != nil {
		logger.Error(err, "❌ RL Agent unavailable")
		os.Exit(1)
	}

	logger.Info("✅ All systems initialized successfully")

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for range ticker.C {
		if err := reconcileRL(clientset, logger, cfg, history, safety); err != nil {
			logger.Error(err, "⚠️ Error in reconcile loop")
		}
	}
}

// ======================== K8S CLIENT ========================

func buildK8sClient(log logr.Logger) (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	config, err = rest.InClusterConfig()
	if err != nil {
		kubeconfig := filepath.Join(homeDir(), ".kube", "config")
		log.Info("⚙️ Using local kubeconfig", "path", kubeconfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
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

// ======================== RECONCILE LOOP ========================

func reconcileRL(clientset *kubernetes.Clientset, log logr.Logger, cfg Config,
	history *MetricsHistory, safety *SafetyController) error {

	ctx := context.Background()

	deploy, err := clientset.AppsV1().Deployments(cfg.Namespace).Get(ctx, cfg.Deployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment: %w", err)
	}

	metrics, err := collectMetrics(clientset, cfg, deploy, history, log)
	if err != nil {
		log.Error(err, "⚠️ Failed to collect metrics")
		return err
	}

	history.Add(metrics)

	log.Info("📊 Metrics Collected",
		"cpu", fmt.Sprintf("%.3f", metrics.CPUUsage),
		"memory", fmt.Sprintf("%.2f GB", metrics.MemoryUsage),
		"requests/s", fmt.Sprintf("%.1f", metrics.RequestRate),
		"p95_latency", fmt.Sprintf("%.3fs", metrics.LatencyP95),
		"replicas", metrics.Replicas,
		"ready", metrics.PodReady,
		"pending", metrics.PodPending,
		"error_rate", fmt.Sprintf("%.4f", metrics.ErrorRate))

	rlResp, err := queryRLAgent(cfg, metrics, log)
	if err != nil {
		log.Error(err, "⚠️ RL Agent failed, using fallback")
		return fallbackScaling(clientset, log, cfg, metrics, deploy, safety)
	}

	if !rlResp.Success {
		log.Info("⚠️ RL Agent returned unsuccessful response", "message", rlResp.Message)
		return nil
	}

	// FIXED: Enhanced logging with more details
	log.Info("🤖 RL Decision",
		"action", rlResp.ActionName,
		"confidence", fmt.Sprintf("%.2f%%", rlResp.Confidence*100),
		"value", fmt.Sprintf("%.2f", rlResp.ValueEstimate),
		"reward", fmt.Sprintf("%.2f", rlResp.Reward),
		"buffer", fmt.Sprintf("%d", rlResp.BufferSize),
		"steps", fmt.Sprintf("%d", rlResp.TrainingSteps))

	newReplicas := calculateNewReplicas(metrics.Replicas, rlResp.Action, cfg)

	if newReplicas != metrics.Replicas {
		if cfg.SafetyEnabled {
			allowed, reason := safety.CanScale(metrics.Replicas, newReplicas, rlResp.ActionName)
			if !allowed {
				log.Info("🛡️ Scaling blocked by safety controller", "reason", reason)
				return nil
			}
		}

		if err := executeScaling(clientset, ctx, cfg, deploy, newReplicas, log); err != nil {
			return fmt.Errorf("scaling failed: %w", err)
		}

		safety.RecordScaling(metrics.Replicas, newReplicas, rlResp.ActionName, "RL decision")

		log.Info("✅ Scaling Executed",
			"from", metrics.Replicas,
			"to", newReplicas,
			"action", rlResp.ActionName,
			"confidence", fmt.Sprintf("%.0f%%", rlResp.Confidence*100))
	} else {
		log.Info("⏸️ No scaling needed", "action", rlResp.ActionName)
	}

	return nil
}

// ======================== METRICS COLLECTION ========================

func collectMetrics(clientset *kubernetes.Clientset, cfg Config, deploy *appsv1.Deployment,
	history *MetricsHistory, log logr.Logger) (Metrics, error) {

	ctx := context.Background()
	now := time.Now()

	replicas := int32(1)
	if deploy.Spec.Replicas != nil {
		replicas = *deploy.Spec.Replicas
	}

	var metrics Metrics

	if cfg.MockMode {
		metrics = generateMockMetrics(replicas)
		log.Info("🎭 Using mock metrics")
	} else {
		// Real Prometheus queries...
		cpuUsage, _ := queryPrometheus(cfg.PromURL, fmt.Sprintf(
			`avg(rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s-.*"}[2m]))`,
			cfg.Namespace, cfg.Deployment))

		memUsage, _ := queryPrometheus(cfg.PromURL, fmt.Sprintf(
			`avg(container_memory_working_set_bytes{namespace="%s",pod=~"%s-.*"}) / (1024*1024*1024)`,
			cfg.Namespace, cfg.Deployment))

		requestRate, _ := queryPrometheus(cfg.PromURL, fmt.Sprintf(
			`sum(rate(http_requests_total{namespace="%s",deployment="%s"}[2m]))`,
			cfg.Namespace, cfg.Deployment))

		latencyP50, _ := queryPrometheus(cfg.PromURL, fmt.Sprintf(
			`histogram_quantile(0.50, rate(http_request_duration_seconds_bucket{namespace="%s"}[2m]))`,
			cfg.Namespace))

		latencyP95, _ := queryPrometheus(cfg.PromURL, fmt.Sprintf(
			`histogram_quantile(0.95, rate(http_request_duration_seconds_bucket{namespace="%s"}[2m]))`,
			cfg.Namespace))

		latencyP99, _ := queryPrometheus(cfg.PromURL, fmt.Sprintf(
			`histogram_quantile(0.99, rate(http_request_duration_seconds_bucket{namespace="%s"}[2m]))`,
			cfg.Namespace))

		errorRate, _ := queryPrometheus(cfg.PromURL, fmt.Sprintf(
			`sum(rate(http_requests_total{namespace="%s",status=~"5.."}[2m])) / sum(rate(http_requests_total{namespace="%s"}[2m]))`,
			cfg.Namespace, cfg.Namespace))

		metrics = Metrics{
			CPUUsage:    cpuUsage,
			MemoryUsage: memUsage,
			RequestRate: requestRate,
			LatencyP50:  latencyP50,
			LatencyP95:  latencyP95,
			LatencyP99:  latencyP99,
			ErrorRate:   errorRate,
		}
	}

	pods, err := clientset.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", cfg.Deployment),
	})

	pendingCount := int32(0)
	readyCount := int32(0)
	if err == nil {
		for _, pod := range pods.Items {
			if pod.Status.Phase == "Pending" {
				pendingCount++
			} else if pod.Status.Phase == "Running" {
				readyCount++
			}
		}
	}

	cpu1m, cpu5m, reqTrend := history.CalculateTrends()

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
		log.Info("⚠️ Metrics validation warning", "error", err.Error())
	}

	return metrics, nil
}

// FIXED: Better mock metrics generation
func generateMockMetrics(replicas int32) Metrics {
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

	// FIXED: More realistic pod status
	readyCount := replicas
	pendingCount := int32(0)

	// Only show pending pods occasionally (10% chance, max 1 pod)
	if rand.Float64() < 0.1 {
		pendingCount = 1
		if replicas > 0 {
			readyCount = replicas - 1
		}
	}

	return Metrics{
		CPUUsage:    cpuUsage,
		MemoryUsage: 2.0 + rand.Float64()*0.5,
		RequestRate: requestRate,
		LatencyP50:  latency * 0.7,
		LatencyP95:  latency,
		LatencyP99:  latency * 1.2,
		ErrorRate:   errorRate,
		PodReady:    readyCount,    // FIXED
		PodPending:  pendingCount,  // FIXED
	}
}

func validateMetrics(m Metrics) error {
	if m.CPUUsage < 0 || m.CPUUsage > 1 {
		return fmt.Errorf("invalid CPU: %.2f", m.CPUUsage)
	}
	if m.ErrorRate < 0 || m.ErrorRate > 1 {
		return fmt.Errorf("invalid error rate: %.2f", m.ErrorRate)
	}
	return nil
}

// ======================== RL AGENT COMMUNICATION ========================

func queryRLAgent(cfg Config, metrics Metrics, log logr.Logger) (*RLResponse, error) {
	reqData := RLRequest{
		DeploymentName: cfg.Deployment,
		Namespace:      cfg.Namespace,
		Metrics:        metrics,
		TrainingMode:   cfg.TrainingMode,
		Timestamp:      time.Now().Unix(),
	}

	jsonData, err := json.Marshal(reqData)
	if err != nil {
		return nil, fmt.Errorf("marshal failed: %w", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(
		cfg.RLAgentURL+"/predict",
		"application/json",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("non-200 status %d: %s", resp.StatusCode, string(body))
	}

	var rlResp RLResponse
	if err := json.NewDecoder(resp.Body).Decode(&rlResp); err != nil {
		return nil, fmt.Errorf("decode failed: %w", err)
	}

	return &rlResp, nil
}

// ======================== SCALING EXECUTION ========================

func executeScaling(clientset *kubernetes.Clientset, ctx context.Context, cfg Config,
	deploy *appsv1.Deployment, newReplicas int32, log logr.Logger) error {

	deploy.Spec.Replicas = &newReplicas

	updated, err := clientset.AppsV1().Deployments(cfg.Namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update failed: %w", err)
	}

	if updated.Spec.Replicas == nil || *updated.Spec.Replicas != newReplicas {
		return fmt.Errorf("verification failed: expected %d, got %v",
			newReplicas, updated.Spec.Replicas)
	}

	return nil
}

func calculateNewReplicas(current int32, action int, cfg Config) int32 {
	switch action {
	case 0:
		if current > cfg.MinReplicas {
			return current - 1
		}
	case 2:
		if current < cfg.MaxReplicas {
			return current + 1
		}
	}
	return current
}

// ======================== FALLBACK SCALING ========================

func fallbackScaling(clientset *kubernetes.Clientset, log logr.Logger, cfg Config,
	metrics Metrics, deploy *appsv1.Deployment, safety *SafetyController) error {

	newReplicas := metrics.Replicas
	reason := ""

	if metrics.CPUUsage > 0.75 || metrics.LatencyP95 > 0.5 {
		if metrics.Replicas < cfg.MaxReplicas {
			newReplicas++
			reason = "high load"
		}
	} else if metrics.CPUUsage < 0.3 && metrics.LatencyP95 < 0.2 {
		if metrics.Replicas > cfg.MinReplicas {
			newReplicas--
			reason = "low load"
		}
	}

	if newReplicas != metrics.Replicas {
		allowed, msg := safety.CanScale(metrics.Replicas, newReplicas, reason)
		if !allowed {
			log.Info("🛡️ Fallback scaling blocked", "reason", msg)
			return nil
		}

		ctx := context.Background()
		if err := executeScaling(clientset, ctx, cfg, deploy, newReplicas, log); err != nil {
			return err
		}

		safety.RecordScaling(metrics.Replicas, newReplicas, "fallback", reason)
		log.Info("✅ Fallback scaling executed", "to", newReplicas, "reason", reason)
	}

	return nil
}

// ======================== PROMETHEUS QUERIES ========================

func queryPrometheus(promURL, query string) (float64, error) {
	fullURL := fmt.Sprintf("%s/api/v1/query?query=%s", promURL, query)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fullURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}

	var promResp PrometheusResponse
	if err := json.NewDecoder(resp.Body).Decode(&promResp); err != nil {
		return 0, err
	}

	if len(promResp.Data.Result) == 0 {
		return 0, nil
	}

	valueStr, ok := promResp.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("invalid format")
	}

	var value float64
	fmt.Sscanf(valueStr, "%f", &value)
	return value, nil
}

func testPrometheus(promURL string, log logr.Logger) error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(promURL + "/api/v1/query?query=up")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	log.Info("✅ Prometheus connection successful")
	return nil
}

func testRLAgent(agentURL string, log logr.Logger) error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(agentURL + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	log.Info("✅ RL Agent connection successful")
	return nil
}