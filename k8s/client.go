package k8s

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type ClientBuilder struct {
	logger logr.Logger
}

func NewClientBuilder(logger logr.Logger) *ClientBuilder {
	return &ClientBuilder{logger: logger}
}

func (cb *ClientBuilder) Build() (*kubernetes.Clientset, error) {
	cfg, err := cb.buildConfig()
	if err != nil {
		return nil, fmt.Errorf("build kube config: %w", err)
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}

	cb.logger.Info("✅ Kubernetes client created")
	return cs, nil
}

func (cb *ClientBuilder) buildConfig() (*rest.Config, error) {
	// Prefer in-cluster
	if cfg, err := rest.InClusterConfig(); err == nil {
		cb.logger.Info("🔗 Using in-cluster Kubernetes config")
		return cfg, nil
	}

	// Fallback: kubeconfig
	kubeconfig := cb.getKubeconfigPath()
	if kubeconfig == "" {
		return nil, fmt.Errorf("not in cluster and kubeconfig not found")
	}

	cb.logger.Info("⚙️ Using local kubeconfig", "path", kubeconfig)
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return cfg, nil
}

func (cb *ClientBuilder) getKubeconfigPath() string {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return v
	}
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
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

// HealthChecker checks Kubernetes API health
type HealthChecker struct {
	clientset *kubernetes.Clientset
	logger    logr.Logger
}

func NewHealthChecker(clientset *kubernetes.Clientset, logger logr.Logger) *HealthChecker {
	return &HealthChecker{clientset: clientset, logger: logger}
}

func (hc *HealthChecker) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	version, err := hc.clientset.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("k8s api unreachable: %w", err)
	}
	hc.logger.Info("✅ Kubernetes API healthy", "version", version.GitVersion, "platform", version.Platform)
	return nil
}
