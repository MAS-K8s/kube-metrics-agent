package k8s

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ClientBuilder helps build Kubernetes clients
type ClientBuilder struct {
	logger logr.Logger
}

// NewClientBuilder creates a new client builder
func NewClientBuilder(logger logr.Logger) *ClientBuilder {
	return &ClientBuilder{
		logger: logger,
	}
}

// Build creates a Kubernetes clientset
func (cb *ClientBuilder) Build() (*kubernetes.Clientset, error) {
	config, err := cb.buildConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	cb.logger.Info("✅ Kubernetes client created successfully")
	return clientset, nil
}

// buildConfig creates a Kubernetes client configuration
func (cb *ClientBuilder) buildConfig() (*rest.Config, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err == nil {
		cb.logger.Info("🔗 Using in-cluster Kubernetes configuration")
		return config, nil
	}

	// Fall back to kubeconfig
	kubeconfig := cb.getKubeconfigPath()
	if kubeconfig == "" {
		return nil, fmt.Errorf("could not find kubeconfig and not running in-cluster")
	}

	cb.logger.Info("⚙️ Using local kubeconfig", "path", kubeconfig)
	config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	return config, nil
}

// getKubeconfigPath finds the kubeconfig file path
func (cb *ClientBuilder) getKubeconfigPath() string {
	// Check KUBECONFIG environment variable
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		return kubeconfig
	}

	// Check default location
	home := homeDir()
	if home == "" {
		return ""
	}

	return filepath.Join(home, ".kube", "config")
}

// homeDir gets the user's home directory
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

// NewHealthChecker creates a new health checker
func NewHealthChecker(clientset *kubernetes.Clientset, logger logr.Logger) *HealthChecker {
	return &HealthChecker{
		clientset: clientset,
		logger:    logger,
	}
}

// Check verifies Kubernetes API is reachable
func (hc *HealthChecker) Check() error {
	// Try to get server version
	version, err := hc.clientset.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("cannot reach Kubernetes API: %w", err)
	}

	hc.logger.Info("✅ Kubernetes API healthy",
		"version", version.GitVersion,
		"platform", version.Platform)

	return nil
}