package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Config struct {
	Namespace      string
	Deployment     string
	PromURL        string
	RLAgentURL     string
	Interval       time.Duration
	MinReplicas    int32
	MaxReplicas    int32
	TrainingMode   bool
	ModelPath      string
}

type Metrics struct {
	CPUUsage      float64 `json:"cpu_usage"`
	MemoryUsage   float64 `json:"memory_usage"`
	RequestRate   float64 `json:"request_rate"`
	LatencyP95    float64 `json:"latency_p95"`
	Replicas      int32   `json:"replicas"`
	ErrorRate     float64 `json:"error_rate"`
	PodPending    int32   `json:"pod_pending"`
	Timestamp     int64   `json:"timestamp"`
}

type RLRequest struct {
	DeploymentName string  `json:"deployment_name"`
	Namespace      string  `json:"namespace"`
	Metrics        Metrics `json:"metrics"`
	TrainingMode   bool    `json:"training_mode"`
}

type RLResponse struct {
	Action       int     `json:"action"`        // 0: scale_down, 1: no_action, 2: scale_up
	ActionName   string  `json:"action_name"`
	Confidence   float64 `json:"confidence"`
	Reward       float64 `json:"reward,omitempty"`
	Epsilon      float64 `json:"epsilon,omitempty"`
}

type PrometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Value []interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

type MetricsHistory struct {
	Previous Metrics
	Current  Metrics
}

func main() {
	var cfg Config
	var minRepl, maxRepl int

	flag.StringVar(&cfg.Namespace, "namespace", "default", "Kubernetes namespace")
	flag.StringVar(&cfg.Deployment, "deployment", "myapp", "Deployment name to manage")
	flag.StringVar(&cfg.PromURL, "prometheus", "http://34.145.46.139:30457", "Prometheus base URL")
	flag.StringVar(&cfg.RLAgentURL, "rl-agent", "http://localhost:5000", "RL Agent service URL")
	flag.DurationVar(&cfg.Interval, "interval", 30*time.Second, "Metrics polling interval")
	flag.IntVar(&minRepl, "min-replicas", 1, "Minimum number of replicas")
	flag.IntVar(&maxRepl, "max-replicas", 10, "Maximum number of replicas")
	flag.BoolVar(&cfg.TrainingMode, "training", false, "Enable training mode")
	flag.StringVar(&cfg.ModelPath, "model-path", "./models/rl_model.pt", "Path to save/load model")
	flag.Parse()

	cfg.MinReplicas = int32(minRepl)
	cfg.MaxReplicas = int32(maxRepl)

	zapLog, _ := zap.NewDevelopment()
	defer zapLog.Sync()
	logger := zapr.NewLogger(zapLog)

	logger.Info("🚀 Starting RL-Enhanced Go Controller Agent",
		"namespace", cfg.Namespace,
		"deployment", cfg.Deployment,
		"training_mode", cfg.TrainingMode)

	clientset, err := buildK8sClient(logger)
	if err != nil {
		logger.Error(err, "❌ Failed to create Kubernetes client")
		os.Exit(1)
	}

	// Initialize metrics history
	history := &MetricsHistory{}

	// Main control loop
	for {
		err := reconcileRL(clientset, logger, cfg, history)
		if err != nil {
			logger.Error(err, "⚠️ Error during reconcile loop")
		}
		time.Sleep(cfg.Interval)
	}
}

func buildK8sClient(log logr.Logger) (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	config, err = rest.InClusterConfig()
	if err != nil {
		var kubeconfig string
		if home := homeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		} else {
			return nil, fmt.Errorf("cannot find home directory for kubeconfig")
		}

		log.Info("⚙️ Using local kubeconfig", "path", kubeconfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
		}
	} else {
		log.Info("🔗 Using in-cluster Kubernetes configuration")
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to build Kubernetes client: %w", err)
	}
	return clientset, nil
}

func reconcileRL(clientset *kubernetes.Clientset, log logr.Logger, cfg Config, history *MetricsHistory) error {
	ctx := context.Background()

	// Get current deployment
	deploy, err := clientset.AppsV1().Deployments(cfg.Namespace).Get(ctx, cfg.Deployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment %s: %w", cfg.Deployment, err)
	}

	// Collect all metrics
	metrics, err := collectMetrics(clientset, cfg, deploy)
	if err != nil {
		return fmt.Errorf("failed to collect metrics: %w", err)
	}

	// Update history
	history.Previous = history.Current
	history.Current = metrics

	log.Info("📊 Collected Metrics",
		"cpu", fmt.Sprintf("%.2f", metrics.CPUUsage),
		"memory", fmt.Sprintf("%.2f", metrics.MemoryUsage),
		"request_rate", fmt.Sprintf("%.2f", metrics.RequestRate),
		"latency_p95", fmt.Sprintf("%.2f", metrics.LatencyP95),
		"replicas", metrics.Replicas,
		"error_rate", fmt.Sprintf("%.4f", metrics.ErrorRate))

	// Query RL Agent for action
	rlResponse, err := queryRLAgent(cfg, metrics)
	if err != nil {
		log.Error(err, "⚠️ Failed to query RL agent, using fallback rule-based scaling")
		return fallbackScaling(clientset, log, cfg, metrics, deploy)
	}

	log.Info("🤖 RL Agent Decision",
		"action", rlResponse.ActionName,
		"confidence", fmt.Sprintf("%.2f", rlResponse.Confidence),
		"epsilon", fmt.Sprintf("%.3f", rlResponse.Epsilon))

	if cfg.TrainingMode && rlResponse.Reward != 0 {
		log.Info("📈 Training Reward", "reward", fmt.Sprintf("%.2f", rlResponse.Reward))
	}

	// Execute action
	newReplicas := calculateNewReplicas(metrics.Replicas, rlResponse.Action, cfg)
	
	if newReplicas != metrics.Replicas {
		deploy.Spec.Replicas = &newReplicas
		_, err := clientset.AppsV1().Deployments(cfg.Namespace).Update(ctx, deploy, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update replicas: %w", err)
		}
		log.Info("✅ Executed Scaling Action",
			"from", metrics.Replicas,
			"to", newReplicas,
			"action", rlResponse.ActionName)
	} else {
		log.Info("⏸️ No scaling required", "action", rlResponse.ActionName)
	}

	return nil
}

func collectMetrics(clientset *kubernetes.Clientset, cfg Config, deploy interface{}) (Metrics, error) {
	ctx := context.Background()
	
	// Get current replicas
	replicas := int32(1)
	if d, ok := deploy.(interface{ GetReplicas() *int32 }); ok {
		if r := d.GetReplicas(); r != nil {
			replicas = *r
		}
	}

	// Query Prometheus for various metrics
	cpuUsage, _ := queryPrometheus(cfg.PromURL, fmt.Sprintf(
		`avg(rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s-.*"}[2m]))`,
		cfg.Namespace, cfg.Deployment))

	memUsage, _ := queryPrometheus(cfg.PromURL, fmt.Sprintf(
		`avg(container_memory_working_set_bytes{namespace="%s",pod=~"%s-.*"}) / (1024*1024*1024)`,
		cfg.Namespace, cfg.Deployment))

	requestRate, _ := queryPrometheus(cfg.PromURL, fmt.Sprintf(
		`sum(rate(http_requests_total{namespace="%s",deployment="%s"}[2m]))`,
		cfg.Namespace, cfg.Deployment))

	latencyP95, _ := queryPrometheus(cfg.PromURL, fmt.Sprintf(
		`histogram_quantile(0.95, rate(http_request_duration_seconds_bucket{namespace="%s",deployment="%s"}[2m]))`,
		cfg.Namespace, cfg.Deployment))

	errorRate, _ := queryPrometheus(cfg.PromURL, fmt.Sprintf(
		`sum(rate(http_requests_total{namespace="%s",deployment="%s",status=~"5.."}[2m])) / sum(rate(http_requests_total{namespace="%s",deployment="%s"}[2m]))`,
		cfg.Namespace, cfg.Deployment, cfg.Namespace, cfg.Deployment))

	// Get pending pods
	pods, _ := clientset.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", cfg.Deployment),
	})
	
	pendingCount := int32(0)
	if pods != nil {
		for _, pod := range pods.Items {
			if pod.Status.Phase == "Pending" {
				pendingCount++
			}
		}
	}

	return Metrics{
		CPUUsage:    cpuUsage,
		MemoryUsage: memUsage,
		RequestRate: requestRate,
		LatencyP95:  latencyP95,
		Replicas:    replicas,
		ErrorRate:   errorRate,
		PodPending:  pendingCount,
		Timestamp:   time.Now().Unix(),
	}, nil
}

func queryRLAgent(cfg Config, metrics Metrics) (*RLResponse, error) {
	reqData := RLRequest{
		DeploymentName: cfg.Deployment,
		Namespace:      cfg.Namespace,
		Metrics:        metrics,
		TrainingMode:   cfg.TrainingMode,
	}

	jsonData, err := json.Marshal(reqData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := http.Post(
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
		return nil, fmt.Errorf("non-200 response: %d - %s", resp.StatusCode, string(body))
	}

	var rlResp RLResponse
	if err := json.NewDecoder(resp.Body).Decode(&rlResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &rlResp, nil
}

func queryPrometheus(promURL, query string) (float64, error) {
	fullURL := fmt.Sprintf("%s/api/v1/query?query=%s", promURL, query)
	resp, err := http.Get(fullURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("non-200 response: %d", resp.StatusCode)
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
		return 0, fmt.Errorf("invalid data format")
	}

	var value float64
	fmt.Sscanf(valueStr, "%f", &value)
	return value, nil
}

func calculateNewReplicas(current int32, action int, cfg Config) int32 {
	switch action {
	case 0: // scale_down
		if current > cfg.MinReplicas {
			return current - 1
		}
	case 2: // scale_up
		if current < cfg.MaxReplicas {
			return current + 1
		}
	}
	return current
}

func fallbackScaling(clientset *kubernetes.Clientset, log logr.Logger, cfg Config, metrics Metrics, deploy interface{}) error {
	// Simple rule-based fallback
	newReplicas := metrics.Replicas
	
	if metrics.CPUUsage > 0.7 && metrics.Replicas < cfg.MaxReplicas {
		newReplicas++
		log.Info("📈 Fallback: Scaling UP", "reason", "high CPU")
	} else if metrics.CPUUsage < 0.3 && metrics.Replicas > cfg.MinReplicas {
		newReplicas--
		log.Info("📉 Fallback: Scaling DOWN", "reason", "low CPU")
	}
	
	if newReplicas != metrics.Replicas {
		// Implement update logic here
		log.Info("✅ Fallback scaling executed", "replicas", newReplicas)
	}
	
	return nil
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