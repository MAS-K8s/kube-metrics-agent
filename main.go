package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go-controller-agent/config"
	"go-controller-agent/dto"
	"go-controller-agent/k8s"
	"go-controller-agent/metrics"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
)

type CircuitBreaker struct {
	mu               sync.Mutex
	failures         int
	openUntil        time.Time
	failureThreshold int
	openDuration     time.Duration
}

func NewCircuitBreaker(threshold int, openDuration time.Duration) *CircuitBreaker {
	return &CircuitBreaker{failureThreshold: threshold, openDuration: openDuration}
}
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return !time.Now().Before(cb.openUntil)
}
func (cb *CircuitBreaker) Success() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.openUntil = time.Time{}
}
func (cb *CircuitBreaker) Fail() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures++
	if cb.failures >= cb.failureThreshold {
		cb.openUntil = time.Now().Add(cb.openDuration)
	}
}

type cooldownState struct {
	mu         sync.Mutex
	lastScaled map[string]time.Time
}

func newCooldownState() *cooldownState { return &cooldownState{lastScaled: map[string]time.Time{}} }
func (cs *cooldownState) CanScale(key string, cooldown time.Duration) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	last, ok := cs.lastScaled[key]
	if !ok {
		return true
	}
	return time.Since(last) >= cooldown
}
func (cs *cooldownState) MarkScaled(key string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.lastScaled[key] = time.Now()
}

type HistoryStore struct {
	mu   sync.Mutex
	data map[string]*metrics.History
}

func NewHistoryStore() *HistoryStore { return &HistoryStore{data: map[string]*metrics.History{}} }
func (hs *HistoryStore) Get(ns, name string) *metrics.History {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	key := ns + "/" + name
	if h, ok := hs.data[key]; ok {
		return h
	}
	h := metrics.NewHistory(120)
	hs.data[key] = h
	return h
}

func main() {
	zapLog, _ := zap.NewDevelopment()
	logger := zapr.NewLogger(zapLog)

	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		logger.Error(err, "config invalid")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	nsInfo := cfg.Namespace
	if nsInfo == "" {
		nsInfo = "<ALL>"
	}

	logger.Info("🚀 RL Controller starting",
		"namespace", nsInfo,
		"selector", cfg.LabelSelector,
		"interval", cfg.Interval,
		"training", cfg.TrainingMode,
		"mock", cfg.MockMode,
		"maxConcurrent", cfg.MaxConcurrent,
	)

	clientset, err := k8s.NewClientBuilder(logger).Build()
	if err != nil {
		logger.Error(err, "k8s client build failed")
		os.Exit(1)
	}
	if err := k8s.NewHealthChecker(clientset, logger).Check(ctx); err != nil {
		logger.Error(err, "k8s health check failed")
		os.Exit(1)
	}

	scaler := k8s.NewScaler(clientset, logger)
	validator := metrics.NewValidator()
	historyStore := NewHistoryStore()

	collector := metrics.NewCollector(
		cfg.PrometheusURL,
		cfg.MockMode,
		cfg.PrometheusTimeout,
		cfg.RetryMaxAttempts,
		cfg.RetryBaseDelay,
		cfg.RetryMaxDelay,
		logger,
	)

	httpClient := &http.Client{Timeout: cfg.RLAgentTimeout}
	rlCB := NewCircuitBreaker(cfg.CBFailureThreshold, cfg.CBOpenDuration)
	cd := newCooldownState()
	sem := make(chan struct{}, cfg.MaxConcurrent)

	checkStartup(ctx, cfg, logger, httpClient, collector)

	_ = runOnce(ctx, cfg, logger, scaler, collector, validator, historyStore, httpClient, rlCB, cd, sem)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("🛑 Shutdown requested")
			return
		case <-ticker.C:
			_ = runOnce(ctx, cfg, logger, scaler, collector, validator, historyStore, httpClient, rlCB, cd, sem)
		}
	}
}

func checkStartup(ctx context.Context, cfg config.Config, logger logr.Logger, httpClient *http.Client, collector *metrics.Collector) {
	if err := checkRLHealth(ctx, cfg, httpClient); err != nil {
		logger.Error(err, "⚠️ RL Agent health check failed")
	} else {
		logger.Info("✅ RL Agent health check OK")
	}

	if !cfg.MockMode {
		if err := collector.CheckPrometheus(ctx); err != nil {
			logger.Error(err, "⚠️ Prometheus health check failed (metrics may be unavailable)")
		} else {
			logger.Info("✅ Prometheus health check OK")
		}
	} else {
		logger.Info("ℹ️ MockMode enabled, skipping Prometheus health check")
	}
}

func checkRLHealth(ctx context.Context, cfg config.Config, httpClient *http.Client) error {
	hctx, cancel := context.WithTimeout(ctx, cfg.RLAgentTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(hctx, http.MethodGet, cfg.RLAgentURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("create health request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("rl agent not reachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("rl health status=%d body=%s", resp.StatusCode, string(body))
	}
	return nil
}

func runOnce(
	ctx context.Context,
	cfg config.Config,
	logger logr.Logger,
	scaler *k8s.Scaler,
	collector *metrics.Collector,
	validator *metrics.Validator,
	historyStore *HistoryStore,
	httpClient *http.Client,
	rlCB *CircuitBreaker,
	cd *cooldownState,
	sem chan struct{},
) error {
	k8sCtx, cancel := context.WithTimeout(ctx, cfg.K8sTimeout)
	defer cancel()

	deployments, err := scaler.ListDeploymentsBySelector(k8sCtx, cfg.Namespace, cfg.LabelSelector)
	if err != nil {
		logger.Error(err, "list deployments failed", "namespace", cfg.Namespace, "selector", cfg.LabelSelector)
		return nil
	}

	if len(deployments) == 0 {
		nsInfo := cfg.Namespace
		if nsInfo == "" {
			nsInfo = "<ALL>"
		}
		logger.V(1).Info("no deployments matched selector", "namespace", nsInfo, "selector", cfg.LabelSelector)
		return nil
	}

	var wg sync.WaitGroup
	for _, dep := range deployments {
		dep := dep
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			reconcileCtx, cancel := context.WithTimeout(ctx, cfg.Interval)
			defer cancel()

			if err := reconcileOne(reconcileCtx, cfg, logger, scaler, collector, validator, historyStore, httpClient, rlCB, cd, dep.Namespace, dep.Name); err != nil {
				logger.Error(err, "reconcile failed", "namespace", dep.Namespace, "deployment", dep.Name)
			}
		}()
	}
	wg.Wait()
	return nil
}

func reconcileOne(
	ctx context.Context,
	cfg config.Config,
	logger logr.Logger,
	scaler *k8s.Scaler,
	collector *metrics.Collector,
	validator *metrics.Validator,
	historyStore *HistoryStore,
	httpClient *http.Client,
	rlCB *CircuitBreaker,
	cd *cooldownState,
	namespace, name string,
) error {
	k8sCtx, cancel := context.WithTimeout(ctx, cfg.K8sTimeout)
	defer cancel()

	dep, err := scaler.GetDeployment(k8sCtx, namespace, name)
	if err != nil {
		return fmt.Errorf("get deployment: %w", err)
	}

	hist := historyStore.Get(namespace, name)

	// ✅ If Prometheus fails: LOG and skip scaling (controller continues)
	m, err := collector.Collect(ctx, scaler.Clientset(), namespace, name, dep, hist)
	if err != nil {
		logger.Error(err, "⚠️ Prometheus metrics failed (skip scaling)", "namespace", namespace, "deployment", name)
		return nil
	}

	vr := validator.Validate(m)
	if !vr.Valid {
		logger.Error(fmt.Errorf("%v", vr.Errors), "⚠️ Metrics invalid (skip scaling)", "namespace", namespace, "deployment", name)
		return nil
	}
	m = validator.SanitizeMetrics(m)
	hist.Add(m)

	action, conf, actionName, err := callRL(ctx, cfg, httpClient, rlCB, namespace, name, m)
	if err != nil {
		logger.Error(err, "⚠️ RL agent call failed (fallback no_action)", "namespace", namespace, "deployment", name)
		return nil
	}

	if conf < cfg.ConfidenceMin {
		logger.V(1).Info("low confidence -> no_action", "namespace", namespace, "deployment", name, "confidence", conf, "min", cfg.ConfidenceMin)
		return nil
	}

	current := m.Replicas
	desired := current
	switch action {
	case 0:
		desired = current - cfg.MaxScaleStep
	case 2:
		desired = current + cfg.MaxScaleStep
	default:
		desired = current
	}

	if desired < cfg.MinReplicas {
		desired = cfg.MinReplicas
	}
	if desired > cfg.MaxReplicas {
		desired = cfg.MaxReplicas
	}
	if desired == current {
		return nil
	}

	key := namespace + "/" + name
	if !cd.CanScale(key, cfg.Cooldown) {
		return nil
	}

	scaleCtx, cancel := context.WithTimeout(ctx, cfg.K8sTimeout)
	defer cancel()

	if err := scaler.ScaleDeployment(scaleCtx, namespace, name, desired); err != nil {
		return fmt.Errorf("scale failed: %w", err)
	}

	cd.MarkScaled(key)

	logger.Info("📈 Scaling applied",
		"namespace", namespace,
		"deployment", name,
		"action", actionName,
		"confidence", conf,
		"from", current,
		"to", desired,
	)
	return nil
}

func callRL(
	ctx context.Context,
	cfg config.Config,
	httpClient *http.Client,
	cb *CircuitBreaker,
	namespace, deployment string,
	m dto.Metrics,
) (action int, confidence float64, actionName string, err error) {
	if !cb.Allow() {
		return 1, 0, "no_action", fmt.Errorf("circuit breaker open")
	}

	reqBody := dto.RLRequest{
		DeploymentName: deployment,
		Namespace:      namespace,
		Metrics:        m,
		TrainingMode:   cfg.TrainingMode,
		Timestamp:      time.Now().Unix(),
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		cb.Fail()
		return 1, 0, "no_action", fmt.Errorf("marshal rl request: %w", err)
	}

	rlCtx, cancel := context.WithTimeout(ctx, cfg.RLAgentTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rlCtx, http.MethodPost, cfg.RLAgentURL+"/predict", bytes.NewReader(b))
	if err != nil {
		cb.Fail()
		return 1, 0, "no_action", fmt.Errorf("create rl request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		cb.Fail()
		return 1, 0, "no_action", fmt.Errorf("rl http error: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cb.Fail()
		return 1, 0, "no_action", fmt.Errorf("rl status=%d body=%s", resp.StatusCode, string(body))
	}

	var rr dto.RLResponse
	if err := json.Unmarshal(body, &rr); err != nil {
		cb.Fail()
		return 1, 0, "no_action", fmt.Errorf("decode rl response: %w", err)
	}
	if !rr.Success {
		cb.Fail()
		return 1, 0, "no_action", fmt.Errorf("rl success=false: %s", rr.Message)
	}

	cb.Success()
	return rr.Action, rr.Confidence, rr.ActionName, nil
}
