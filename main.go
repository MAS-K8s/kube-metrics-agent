// Command kube-metrics-agent is the MARLS Kubernetes controller.
//
// It runs a continuous reconciliation loop that:
//  1. Discovers all Deployments labelled rl-autoscale=true in the cluster.
//  2. Collects real-time Prometheus metrics and pod topology for each.
//  3. Forwards an 18-dimensional state vector to the PPO RL agent service.
//  4. Applies the returned scaling action (scale_up / no_action / scale_down)
//     subject to confidence gating, cooldown enforcement, and min/max bounds.
//
// All runtime configuration is read from environment variables (see config.go).
// The controller performs a warm-up sequence on startup to ensure both the
// Kubernetes API server and the RL agent are healthy before the first cycle.
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

	"go-controller-agent/circuit"
	"go-controller-agent/config"
	"go-controller-agent/dto"
	"go-controller-agent/k8s"
	"go-controller-agent/metrics"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
)


type cooldownTracker struct {
	mu         sync.Mutex
	lastScaled map[string]time.Time
}

func newCooldownTracker() *cooldownTracker {
	return &cooldownTracker{
		lastScaled: make(map[string]time.Time),
	}
}


func (ct *cooldownTracker) CanScale(key string, cooldown time.Duration) bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	last, ok := ct.lastScaled[key]
	if !ok {
		return true
	}
	return time.Since(last) >= cooldown
}

// MarkScaled records the current time as the last-scaled timestamp for key.
// Call this immediately after a scaling action is successfully applied.
func (ct *cooldownTracker) MarkScaled(key string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.lastScaled[key] = time.Now()
}


// historyStore owns one metrics.History ring buffer per managed deployment.
// Buffers are created lazily on first access and are safe for concurrent use.
type historyStore struct {
	mu   sync.Mutex
	data map[string]*metrics.History
}

func newHistoryStore() *historyStore {
	return &historyStore{
		data: make(map[string]*metrics.History),
	}
}

// Get returns the History for the deployment identified by (namespace, name),
// creating a new 120-entry ring buffer if one does not yet exist.
func (hs *historyStore) Get(namespace, name string) *metrics.History {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	key := namespace + "/" + name
	if h, ok := hs.data[key]; ok {
		return h
	}
	h := metrics.NewHistory(120)
	hs.data[key] = h
	return h
}


func main() {
	// Structured logger — production JSON format written to stdout.
	zapLog, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialise zap logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = zapLog.Sync() }()
	logger := zapr.NewLogger(zapLog)

	// Load .env file when present (local development only).
	// In Kubernetes the environment is provided by the Pod spec; godotenv is a
	// no-op when no .env file exists, so this is always safe to call.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		logger.Error(err, "configuration is invalid — cannot start")
		os.Exit(1)
	}

	// Top-level context cancelled on SIGINT / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	nsDisplay := cfg.Namespace
	if nsDisplay == "" {
		nsDisplay = "<ALL>"
	}

	logger.Info("MARLS RL controller starting",
		"namespace", nsDisplay,
		"labelSelector", cfg.LabelSelector,
		"interval", cfg.Interval,
		"trainingMode", cfg.TrainingMode,
		"mockMode", cfg.MockMode,
		"maxConcurrent", cfg.MaxConcurrent,
		"prometheusURL", cfg.PrometheusURL,
		"rlAgentURL", cfg.RLAgentURL,
		"minReplicas", cfg.MinReplicas,
		"maxReplicas", cfg.MaxReplicas,
		"maxScaleStep", cfg.MaxScaleStep,
		"cooldown", cfg.Cooldown,
		"confidenceMin", cfg.ConfidenceMin,
	)

	// Build and verify Kubernetes client.
	clientset, err := k8s.NewClientBuilder(logger).Build()
	if err != nil {
		logger.Error(err, "failed to build Kubernetes client")
		os.Exit(1)
	}
	if err := k8s.NewHealthChecker(clientset, logger).Check(ctx); err != nil {
		logger.Error(err, "Kubernetes health check failed")
		os.Exit(1)
	}

	// Initialise shared infrastructure components.
	scaler      := k8s.NewScaler(clientset, logger)
	validator   := metrics.NewValidator()
	history     := newHistoryStore()
	cooldown    := newCooldownTracker()
	rlBreaker   := circuit.New(cfg.CBFailureThreshold, cfg.CBOpenDuration)
	sem         := make(chan struct{}, cfg.MaxConcurrent)

	// The HTTP client's hard timeout is intentionally set to 3× RLAgentTimeout.
	// Per-request deadlines are controlled by context cancellation inside
	// callRLAgent and checkRLHealth, so this value only prevents an unbounded
	// wait if a context is accidentally missed. It must exceed RLAgentTimeout
	// to avoid racing the context on Flask cold-start (~6–8 s for PyTorch init).
	httpClient := &http.Client{
		Timeout: cfg.RLAgentTimeout * 3,
	}

	collector := metrics.NewCollector(
		cfg.PrometheusURL,
		cfg.MockMode,
		cfg.PrometheusTimeout,
		cfg.RetryMaxAttempts,
		cfg.RetryBaseDelay,
		cfg.RetryMaxDelay,
		logger,
	)

	// Startup health checks and RL agent warm-up.
	runStartupChecks(ctx, cfg, logger, httpClient, collector)

	// Run an initial reconciliation cycle immediately so the first scaling
	// decision is not delayed by the full ticker interval.
	if err := reconcileAll(ctx, cfg, logger, scaler, collector, validator, history, httpClient, rlBreaker, cooldown, sem); err != nil {
		logger.Error(err, "initial reconciliation cycle failed")
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutdown signal received — controller stopping")
			return
		case <-ticker.C:
			if err := reconcileAll(ctx, cfg, logger, scaler, collector, validator, history, httpClient, rlBreaker, cooldown, sem); err != nil {
				logger.Error(err, "reconciliation cycle failed")
			}
		}
	}
}



// runStartupChecks verifies that the RL agent service and Prometheus are
// reachable before the first reconciliation cycle. A failure here is logged
// but does not prevent the controller from starting; the circuit breaker will
// handle subsequent failures gracefully.
func runStartupChecks(
	ctx context.Context,
	cfg config.Config,
	logger logr.Logger,
	httpClient *http.Client,
	collector *metrics.Collector,
) {
	if err := checkRLHealth(ctx, cfg, httpClient); err != nil {
		logger.Error(err, "RL agent health check failed — will retry on first cycle")
	} else {
		logger.Info("RL agent health check passed")
		warmupRLAgent(ctx, cfg, logger, httpClient)
	}

	if cfg.MockMode {
		logger.Info("mock mode enabled — skipping Prometheus health check")
		return
	}
	if err := collector.CheckPrometheus(ctx); err != nil {
		logger.Error(err, "Prometheus health check failed — metrics collection may fail")
	} else {
		logger.Info("Prometheus health check passed")
	}
}


// The warmup is retried up to maxWarmupAttempts times with a 2-second backoff.
func warmupRLAgent(ctx context.Context, cfg config.Config, logger logr.Logger, httpClient *http.Client) {
	const (
		maxWarmupAttempts = 3
		warmupTimeout     = 30 * time.Second
		retryBackoff      = 2 * time.Second
	)

	warmupPayload := []byte(`{"deployment_name":"__warmup__","namespace":"default","metrics":{},"training_mode":false,"timestamp":0}`)

	for attempt := 1; attempt <= maxWarmupAttempts; attempt++ {
		// A new bytes.Reader must be constructed for each attempt because
		// io.Reader is consumed after the first HTTP send.
		body := bytes.NewReader(append([]byte(nil), warmupPayload...))

		wCtx, cancel := context.WithTimeout(ctx, warmupTimeout)
		req, err := http.NewRequestWithContext(wCtx, http.MethodPost, cfg.RLAgentURL+"/predict", body)
		if err != nil {
			cancel()
			logger.Error(err, "RL agent warm-up: failed to construct request", "attempt", attempt)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		start := time.Now()
		resp, err := httpClient.Do(req)
		elapsed := time.Since(start)
		cancel()

		if err != nil {
			logger.Error(err, "RL agent warm-up attempt failed", "attempt", attempt, "elapsed", elapsed)
			if attempt < maxWarmupAttempts {
				time.Sleep(retryBackoff)
			}
			continue
		}
		_ = resp.Body.Close()
		logger.Info("RL agent warm-up complete",
			"attempt", attempt,
			"elapsed", elapsed,
			"httpStatus", resp.StatusCode,
		)
		return
	}

	logger.Info("RL agent warm-up did not complete within retry budget — first live /predict may be slow")
}

// checkRLHealth performs a lightweight GET /health against the RL agent
// service and returns an error if the service is unreachable or returns a
// non-2xx status code.
func checkRLHealth(ctx context.Context, cfg config.Config, httpClient *http.Client) error {
	hCtx, cancel := context.WithTimeout(ctx, cfg.RLAgentTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(hCtx, http.MethodGet, cfg.RLAgentURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("construct health request: %w", err)
	}

	start := time.Now()
	resp, err := httpClient.Do(req)
	elapsed := time.Since(start)

	if err != nil {
		return fmt.Errorf("RL agent unreachable after %s: %w", elapsed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("RL agent health check returned HTTP %d after %s: %s", resp.StatusCode, elapsed, b)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reconciliation loop
// ---------------------------------------------------------------------------

// reconcileAll discovers all labelled Deployments and reconciles each one
// concurrently, bounded by cfg.MaxConcurrent. Errors from individual
// deployments are collected and the first is returned; all are logged.
func reconcileAll(
	ctx context.Context,
	cfg config.Config,
	logger logr.Logger,
	scaler *k8s.Scaler,
	collector *metrics.Collector,
	validator *metrics.Validator,
	history *historyStore,
	httpClient *http.Client,
	rlBreaker *circuit.Breaker,
	cooldown *cooldownTracker,
	sem chan struct{},
) error {
	listCtx, cancel := context.WithTimeout(ctx, cfg.K8sTimeout)
	defer cancel()

	logger.Info("listing deployments",
		"namespace", cfg.Namespace,
		"labelSelector", cfg.LabelSelector,
	)

	deployments, err := scaler.ListDeploymentsBySelector(listCtx, cfg.Namespace, cfg.LabelSelector)
	if err != nil {
		return fmt.Errorf("list deployments: %w", err)
	}

	logger.Info("deployments discovered", "count", len(deployments))

	if len(deployments) == 0 {
		ns := cfg.Namespace
		if ns == "" {
			ns = "<ALL>"
		}
		logger.Info("no deployments matched label selector",
			"namespace", ns,
			"labelSelector", cfg.LabelSelector,
		)
		return nil
	}

	for _, dep := range deployments {
		logger.Info("matched deployment", "namespace", dep.Namespace, "name", dep.Name)
	}

	var (
		wg    sync.WaitGroup
		errCh = make(chan error, len(deployments))
	)

	for _, dep := range deployments {
		dep := dep // capture loop variable for goroutine
		wg.Add(1)
		sem <- struct{}{} // acquire concurrency slot

		go func() {
			defer wg.Done()
			defer func() { <-sem }() // release slot on return

			rCtx, cancel := context.WithTimeout(ctx, cfg.Interval)
			defer cancel()

			if err := reconcileOne(rCtx, cfg, logger, scaler, collector, validator, history, httpClient, rlBreaker, cooldown, dep.Namespace, dep.Name); err != nil {
				errCh <- fmt.Errorf("%s/%s: %w", dep.Namespace, dep.Name, err)
			}
		}()
	}

	wg.Wait()
	close(errCh)

	var firstErr error
	for err := range errCh {
		logger.Error(err, "deployment reconciliation failed")
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// reconcileOne executes a single reconciliation cycle for one deployment:
//  1. Fetch current deployment state from the Kubernetes API.
//  2. Collect and validate Prometheus metrics.
//  3. Forward metrics to the RL agent and receive a scaling decision.
//  4. Apply the decision subject to confidence gating, cooldown, and bounds.
func reconcileOne(
	ctx context.Context,
	cfg config.Config,
	logger logr.Logger,
	scaler *k8s.Scaler,
	collector *metrics.Collector,
	validator *metrics.Validator,
	history *historyStore,
	httpClient *http.Client,
	rlBreaker *circuit.Breaker,
	cooldown *cooldownTracker,
	namespace, name string,
) error {
	// Step 1: Fetch deployment from Kubernetes.
	k8sCtx, cancel := context.WithTimeout(ctx, cfg.K8sTimeout)
	defer cancel()

	dep, err := scaler.GetDeployment(k8sCtx, namespace, name)
	if err != nil {
		return fmt.Errorf("get deployment: %w", err)
	}

	// Step 2: Collect metrics and validate.
	hist := history.Get(namespace, name)
	m, err := collector.Collect(ctx, scaler.Clientset(), namespace, name, dep, hist)
	if err != nil {
		// A collection failure is non-fatal — skip scaling this cycle rather
		// than returning an error, so other deployments are not blocked.
		logger.Error(err, "metric collection failed — skipping scaling this cycle",
			"namespace", namespace, "deployment", name)
		return nil
	}

	vr := validator.Validate(m)
	if !vr.Valid {
		logger.Error(fmt.Errorf("%v", vr.Errors), "metric validation failed — skipping scaling this cycle",
			"namespace", namespace, "deployment", name)
		return nil
	}

	m = validator.SanitizeMetrics(m)
	hist.Add(m)

	logger.Info("metrics collected",
		"namespace", namespace, "deployment", name,
		"cpu", m.CPUUsage, "memoryGiB", m.MemoryUsage,
		"requestRate", m.RequestRate, "latencyP95", m.LatencyP95,
		"errorRate", m.ErrorRate, "replicas", m.Replicas,
		"podReady", m.PodReady, "podPending", m.PodPending,
		"cpuTrend1m", m.CPUTrend1m,
	)

	// Step 3: Call RL agent for a scaling decision.
	decision, err := callRLAgent(ctx, cfg, httpClient, rlBreaker, namespace, name, m, logger)
	if err != nil {
		logger.Error(err, "RL agent call failed — defaulting to no_action",
			"namespace", namespace, "deployment", name)
		return nil
	}

	// Step 4a: Confidence gate — reject decisions below the minimum threshold.
	if decision.Confidence < cfg.ConfidenceMin {
		logger.Info("action rejected: confidence below minimum threshold",
			"namespace", namespace, "deployment", name,
			"action", decision.ActionName,
			"confidence", decision.Confidence,
			"confidenceMin", cfg.ConfidenceMin,
		)
		return nil
	}

	current := m.Replicas
	action := decision.Action

	// Step 4b: Minimum-replica guard — never scale below cfg.MinReplicas.
	if action == 0 && current <= cfg.MinReplicas {
		logger.Info("action rejected: deployment already at minimum replica count",
			"namespace", namespace, "deployment", name,
			"current", current, "minReplicas", cfg.MinReplicas,
		)
		return nil
	}

	// Step 4c: Compute desired replica count with MaxScaleStep clamping.
	desired := computeDesired(current, action, cfg)

	if desired == current {
		logger.Info("no replica change required",
			"namespace", namespace, "deployment", name,
			"current", current, "action", decision.ActionName,
			"confidence", decision.Confidence,
		)
		return nil
	}

	// Step 4d: Cooldown check — suppress rapid consecutive scaling actions.
	key := namespace + "/" + name
	if !cooldown.CanScale(key, cfg.Cooldown) {
		logger.Info("action suppressed: cooldown period active",
			"namespace", namespace, "deployment", name,
			"cooldown", cfg.Cooldown,
		)
		return nil
	}

	// Step 4e: Apply the scaling action.
	scaleCtx, scaleCancel := context.WithTimeout(ctx, cfg.K8sTimeout)
	defer scaleCancel()

	if err := scaler.ScaleDeployment(scaleCtx, namespace, name, desired); err != nil {
		return fmt.Errorf("apply scaling action: %w", err)
	}

	cooldown.MarkScaled(key)

	logger.Info("scaling action applied",
		"namespace", namespace, "deployment", name,
		"action", decision.ActionName,
		"confidence", decision.Confidence,
		"from", current, "to", desired,
	)

	return nil
}

// computeDesired translates a discrete RL action into a target replica count,
// applying MaxScaleStep and the configured min/max bounds.
//
//	action 0 = scale_down → current - MaxScaleStep
//	action 1 = no_action  → current (unchanged)
//	action 2 = scale_up   → current + MaxScaleStep
func computeDesired(current int32, action int, cfg config.Config) int32 {
	var desired int32
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
	return desired
}

// ---------------------------------------------------------------------------
// RL agent HTTP client
// ---------------------------------------------------------------------------

// callRLAgent serialises the current state vector as a dto.RLRequest, sends it
// to the RL agent's POST /predict endpoint, and deserialises the dto.RLResponse.
//
// The circuit breaker is consulted before each request and updated on every
// outcome. When the circuit is open, callRLAgent returns immediately with an
// error so the controller falls back to no_action without blocking.
func callRLAgent(
	ctx context.Context,
	cfg config.Config,
	httpClient *http.Client,
	cb *circuit.Breaker,
	namespace, deployment string,
	m dto.Metrics,
	logger logr.Logger,
) (*dto.RLResponse, error) {
	if !cb.Allow() {
		return nil, fmt.Errorf("RL agent circuit breaker is open — skipping request")
	}

	reqBody := dto.RLRequest{
		DeploymentName: deployment,
		Namespace:      namespace,
		Metrics:        m,
		TrainingMode:   cfg.TrainingMode,
		Timestamp:      time.Now().Unix(),
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		cb.Fail()
		return nil, fmt.Errorf("serialise RL request: %w", err)
	}

	rlCtx, cancel := context.WithTimeout(ctx, cfg.RLAgentTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rlCtx, http.MethodPost, cfg.RLAgentURL+"/predict", bytes.NewReader(payload))
	if err != nil {
		cb.Fail()
		return nil, fmt.Errorf("construct RL request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := httpClient.Do(req)
	elapsed := time.Since(start)

	if err != nil {
		cb.Fail()
		return nil, fmt.Errorf("RL agent HTTP error after %s: %w", elapsed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	logger.Info("RL agent responded",
		"namespace", namespace, "deployment", deployment,
		"httpStatus", resp.StatusCode, "elapsed", elapsed,
	)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cb.Fail()
		return nil, fmt.Errorf("RL agent returned HTTP %d after %s: %s", resp.StatusCode, elapsed, body)
	}

	var rlResp dto.RLResponse
	if err := json.Unmarshal(body, &rlResp); err != nil {
		cb.Fail()
		return nil, fmt.Errorf("deserialise RL response: %w", err)
	}
	if !rlResp.Success {
		cb.Fail()
		return nil, fmt.Errorf("RL agent returned success=false: %s", rlResp.Message)
	}

	cb.Success()

	logger.Info("RL agent decision received",
		"namespace", namespace, "deployment", deployment,
		"action", rlResp.ActionName, "actionID", rlResp.Action,
		"confidence", rlResp.Confidence,
	)

	return &rlResp, nil
}