package main

import (
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
	Namespace    string
	Deployment   string
	PromURL      string
	Interval     time.Duration
	CPUScaleUp   float64
	CPUScaleDown float64
	MinReplicas  int32
	MaxReplicas  int32
}

type PrometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Value []interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func main() {
	// -------------------------------
	// 🧩 CLI Flags
	// -------------------------------
	var cfg Config
	var minRepl, maxRepl int

	flag.StringVar(&cfg.Namespace, "namespace", "default", "Kubernetes namespace")
	flag.StringVar(&cfg.Deployment, "deployment", "myapp", "Deployment name to manage")
	flag.StringVar(&cfg.PromURL, "prometheus", "http://136.117.195.136:30980", "Prometheus base URL")
	flag.DurationVar(&cfg.Interval, "interval", 15*time.Second, "Metrics polling interval")
	flag.Float64Var(&cfg.CPUScaleUp, "cpu-up", 0.7, "CPU threshold to scale up")
	flag.Float64Var(&cfg.CPUScaleDown, "cpu-down", 0.3, "CPU threshold to scale down")
	flag.IntVar(&minRepl, "min-replicas", 1, "Minimum number of replicas")
	flag.IntVar(&maxRepl, "max-replicas", 5, "Maximum number of replicas")
	flag.Parse()

	cfg.MinReplicas = int32(minRepl)
	cfg.MaxReplicas = int32(maxRepl)

	// -------------------------------
	// ⚙️ Setup logger
	// -------------------------------
	zapLog, _ := zap.NewDevelopment()
	defer zapLog.Sync()
	logger := zapr.NewLogger(zapLog)

	logger.Info("🚀 Starting Go Controller Agent", "namespace", cfg.Namespace, "deployment", cfg.Deployment)

	// -------------------------------
	// 🔗 Build Kubernetes Client
	// -------------------------------
	clientset, err := buildK8sClient(logger)
	if err != nil {
		logger.Error(err, "❌ Failed to create Kubernetes client")
		os.Exit(1)
	}

	// -------------------------------
	// 🔄 Main loop
	// -------------------------------
	for {
		err := reconcile(clientset, logger, cfg)
		if err != nil {
			logger.Error(err, "⚠️ Error during reconcile loop")
		}
		time.Sleep(cfg.Interval)
	}
}

// -------------------------------------------------------------------
// 🔧 Build Kubernetes Client
// -------------------------------------------------------------------
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

// -------------------------------------------------------------------
// 🔁 Reconcile Logic
// -------------------------------------------------------------------
func reconcile(clientset *kubernetes.Clientset, log logr.Logger, cfg Config) error {
	ctx := context.Background()

	deploy, err := clientset.AppsV1().Deployments(cfg.Namespace).Get(ctx, cfg.Deployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment %s: %w", cfg.Deployment, err)
	}

	cpuUsage, err := getCPUUsage(cfg.PromURL, cfg.Namespace, cfg.Deployment)
	if err != nil {
		return fmt.Errorf("failed to query Prometheus: %w", err)
	}

	log.Info("📊 Current CPU usage", "value", cpuUsage)

	replicas := *deploy.Spec.Replicas
	newReplicas := replicas

	if cpuUsage > cfg.CPUScaleUp && replicas < cfg.MaxReplicas {
		newReplicas++
		log.Info("⚡ Scaling UP", "from", replicas, "to", newReplicas)
	} else if cpuUsage < cfg.CPUScaleDown && replicas > cfg.MinReplicas {
		newReplicas--
		log.Info("🧊 Scaling DOWN", "from", replicas, "to", newReplicas)
	}

	if newReplicas != replicas {
		deploy.Spec.Replicas = &newReplicas
		_, err := clientset.AppsV1().Deployments(cfg.Namespace).Update(ctx, deploy, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update replicas: %w", err)
		}
		log.Info("✅ Updated deployment replicas successfully", "replicas", newReplicas)
	}

	return nil
}

// -------------------------------------------------------------------
// 📈 Get CPU Usage from Prometheus
// -------------------------------------------------------------------
func getCPUUsage(promURL, namespace, deployment string) (float64, error) {
	query := fmt.Sprintf(`avg(rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s-.*"}[2m]))`, namespace, deployment)
	fullURL := fmt.Sprintf("%s/api/v1/query?query=%s", promURL, query)

	resp, err := http.Get(fullURL)
	if err != nil {
		return 0, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("non-200 response from Prometheus: %d - %s", resp.StatusCode, string(body))
	}

	var promResp PrometheusResponse
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read response body: %w", err)
	}

	if err := json.Unmarshal(body, &promResp); err != nil {
		return 0, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if len(promResp.Data.Result) == 0 {
		return 0, fmt.Errorf("no data returned for query")
	}

	valueStr, ok := promResp.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("invalid data format from Prometheus")
	}

	var value float64
	fmt.Sscanf(valueStr, "%f", &value)
	return value, nil
}

// -------------------------------------------------------------------
// 🏠 Helper: Get Home Dir
// -------------------------------------------------------------------
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if runtime.GOOS == "windows" {
		return os.Getenv("USERPROFILE")
	}
	return ""
}
