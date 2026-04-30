# 🤖 MARLS – Go Kubernetes Controller

> **Multi-Agent Reinforcement Learning Scaler** — a production-grade Kubernetes controller that drives intelligent autoscaling via a PPO RL agent, with a built-in rule-based fallback scaler for full resilience.

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8.svg)](https://golang.org/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.26+-326CE5.svg)](https://kubernetes.io/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

---

## 📋 Table of Contents

- [Overview](#overview)
- [Architecture](#architecture)
- [Project Structure](#project-structure)
- [How It Works](#how-it-works)
- [Prerequisites](#prerequisites)
- [Installation](#installation)
- [Configuration](#configuration)
- [Deployment](#deployment)
- [Local Development](#local-development)
- [Testing](#testing)
- [Observability](#observability)
- [Troubleshooting](#troubleshooting)

---

## 🎯 Overview

The MARLS Go controller is the **orchestration layer** of the autoscaling system. It runs a continuous reconciliation loop that:

1. **Discovers** all `Deployment` resources labelled `rl-autoscale=true` (cluster-wide or in a single namespace).
2. **Collects** an 18-dimensional state vector from Prometheus (CPU, memory, request rate, latencies, error rate, pod topology, temporal features, and trends).
3. **Forwards** the state to the Flask PPO RL agent service (`POST /predict`).
4. **Applies** the returned scaling action (`scale_up` / `no_action` / `scale_down`), subject to:
   - Confidence gating (reject low-confidence RL decisions)
   - Cooldown enforcement (prevent rapid replica flapping)
   - `MinReplicas` / `MaxReplicas` / `MaxScaleStep` bounds
5. **Falls back** to a deterministic rule-based scaler whenever the RL agent is unreachable or the circuit breaker is open — the controller never silently does nothing.

---

## 🏗️ Architecture

```
┌────────────────────────────────────────────────────────────────────┐
│                    MARLS Go Controller (main.go)                   │
│                                                                    │
│  ┌─────────────────────────────────────────────────────────────┐  │
│  │  Reconciliation Loop  (every INTERVAL seconds)              │  │
│  │                                                             │  │
│  │  1. ListDeployments (label: rl-autoscale=true)              │  │
│  │  2. For each deployment (up to MAX_CONCURRENT goroutines):  │  │
│  │     a. Collect metrics   ──► Prometheus + K8s pod status    │  │
│  │     b. Validate & sanitise metrics                          │  │
│  │     c. Call RL agent     ──► POST /predict                  │  │
│  │        │  success → use PPO action + confidence             │  │
│  │        └  failure → rule-based fallback (fallback.go)       │  │
│  │     d. Confidence gate, cooldown check, min/max bounds      │  │
│  │     e. ScaleDeployment   ──► K8s AppsV1 API                 │  │
│  └─────────────────────────────────────────────────────────────┘  │
│                                                                    │
│  ┌──────────────┐  ┌────────────────┐  ┌────────────────────────┐ │
│  │ Circuit      │  │ Cooldown       │  │ History Ring Buffers   │ │
│  │ Breaker      │  │ Tracker        │  │ (120 entries/deploy.)  │ │
│  │ (circuit/)   │  │ (per deploy.)  │  │ → CPU/request trends   │ │
│  └──────────────┘  └────────────────┘  └────────────────────────┘ │
└────────────────────────────────────────────────────────────────────┘
         │                    │                        │
         ▼                    ▼                        ▼
  Kubernetes API        Prometheus API          RL Agent (Flask)
  (scale deploy.)      (query metrics)         POST /predict
```

### Decision Flow

```
Metrics collected
       │
       ▼
 Validate metrics ──► invalid? → skip cycle
       │
       ▼
 Call RL Agent ──► success? ──────────────────────┐
       │                                          │
       └── failure / CB open                      │
               │                                  ▼
               ▼                        action, confidence
       FallbackEnabled?                           │
         yes → rule-based decision                │
         no  → no_action (return)                 │
               │                                  │
               └────────────────────┬─────────────┘
                                    ▼
                          Confidence ≥ CONFIDENCE_MIN?
                              no → skip cycle
                              yes ↓
                          current == MinReplicas && scale_down?
                              yes → skip cycle
                              no ↓
                          computeDesired (clamped by MaxScaleStep, min, max)
                              desired == current?
                              yes → skip cycle
                              no ↓
                          Cooldown elapsed?
                              no → skip cycle
                              yes ↓
                          ScaleDeployment ✅
```

---

## 📁 Project Structure

```
.
├── main.go                    # Entry point: reconcile loop, cooldown, history store
├── go.mod / go.sum
├── .env                       # Local dev env vars (not committed)
├── Dockerfile
├── README.md
│
├── config/
│   ├── config.go              # Config struct; env-var loading + flag overrides
│   └── config_test.go
│
├── circuit/
│   └── circuit_breaker.go     # Thread-safe circuit breaker (fail/success/allow)
│
├── dto/                       # Shared data-transfer types (no business logic)
│   ├── model_config.go        # Config DTO
│   ├── model_DeploymentStatus.go  # DeploymentStatus + IsHealthy/IsScaling helpers
│   ├── model_metrics.go       # 18-field Metrics struct
│   ├── model_prometheus.go    # PrometheusResponse, ScalingEvent, SafetyController
│   └── model_rl-agent.go      # RLRequest / RLResponse
│
├── k8s/
│   ├── client.go              # K8s client builder (in-cluster + kubeconfig fallback)
│   └── scaler.go              # ListDeployments, GetDeployment, ScaleDeployment
│
└── metrics/
    ├── collector.go           # Prometheus queries (parallel), pod status, mock mode
    ├── fallback.go            # Rule-based scaler (drop-in RL replacement)
    ├── history.go             # Ring-buffer metrics history + trend calculation
    ├── validator.go           # Metric range validation + sanitisation
    └── validator_test.go      # Unit tests (TC-UT-13 through TC-UT-20)
```

---

## ⚙️ How It Works

### Metric Collection (`metrics/collector.go`)

All Prometheus queries run **in parallel** via goroutines, reducing collection time from ~81 s (sequential) to ~10 s. Metrics collected:

| Field | Source |
|---|---|
| `cpu_usage` | `container_cpu_usage_seconds_total` (rate 2m) |
| `memory_usage` | `container_memory_working_set_bytes` (GiB) |
| `request_rate` | `http_requests_total` → network fallback if zero |
| `latency_p50/p95/p99` | `http_request_duration_seconds_bucket` → CPU throttle proxy if missing |
| `error_rate` | `http_requests_total{status_code=~"5.."}` / total |
| `pod_pending / pod_ready` | K8s `CoreV1().Pods().List()` |
| `cpu_trend_1m / cpu_trend_5m` | Calculated from History ring buffer |
| `request_trend` | Calculated from History ring buffer |
| `hour / day_of_week / is_weekend / is_peak_hour` | `time.Now()` |

### Circuit Breaker (`circuit/circuit_breaker.go`)

A mutex-protected, threshold-based circuit breaker guards every RL agent call:

- Opens after `CB_FAILURE_THRESHOLD` consecutive failures.
- Stays open for `CB_OPEN_DURATION`, then resets automatically.
- When open, the controller routes immediately to the rule-based fallback.

### Rule-Based Fallback Scaler (`metrics/fallback.go`)

Activated when the RL agent is unreachable or `FALLBACK_ENABLED=true` (force rule-based mode). Priority order:

1. **Scale up** if ANY of: `CPU > FALLBACK_SCALEUP_CPU`, `P95 > FALLBACK_SCALEUP_LATENCY`, `error% > FALLBACK_SCALEUP_ERROR`, `req/s > FALLBACK_SCALEUP_REQRATE`.
2. **Scale down** if ALL of the low-load thresholds are simultaneously satisfied.
3. **No action** — default.

Fallback decisions always have `confidence = 1.0` and bypass the confidence gate.

### Cooldown & History (`main.go`, `metrics/history.go`)

- Each deployment has its own `time.Time` cooldown entry; scaling is suppressed until `COOLDOWN` has elapsed since the last action.
- Each deployment has a 120-entry ring buffer of `dto.Metrics` snapshots used to derive `cpu_trend_1m`, `cpu_trend_5m`, and `request_trend`.

---

## 📦 Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | 1.21+ | `go build ./...` |
| Kubernetes | 1.26+ | RBAC: get/list/watch/update Deployments, list Pods |
| Prometheus | 2.x | Must expose standard `http_requests_total`, `http_request_duration_seconds_bucket`, `container_cpu_usage_seconds_total`, `container_memory_working_set_bytes` |
| RL Agent | — | Flask PPO service exposing `POST /predict` and `GET /health` |
| Docker | 20.x+ | For building the container image |

---

## 🚀 Installation

### 1. Clone the repository

```bash
git clone https://github.com/MAS-K8s/kube-metrics-agent.git
cd kube-metrics-agent
```

### 2. Install dependencies

```bash
go mod download
```

### 3. Run the app

```bash
go run main.go
```

---

## 🔧 Configuration

All configuration is driven by **environment variables**. CLI flags exist only to override values during local development (they use a fresh `flag.FlagSet` and never conflict with `go test`).

### Required Variables

| Variable | Description |
|---|---|
| `PROMETHEUS_URL` | Base URL of the Prometheus server (e.g. `http://prometheus:9090`) |
| `RL_AGENT_URL` | Base URL of the Flask RL agent (e.g. `http://rl-agent:5000`) |

### Scope & Discovery

| Variable | Default | Description |
|---|---|---|
| `NAMESPACE` | `""` (all) | Restrict to a single namespace; empty = cluster-wide |
| `LABEL_SELECTOR` | `rl-autoscale=true` | Label selector for managed Deployments |

### Reconciliation

| Variable | Default | Description |
|---|---|---|
| `INTERVAL` | `30s` | How often to run the reconciliation loop (min `5s`) |
| `MAX_CONCURRENT` | `4` | Max parallel deployment goroutines per cycle |

### Scaling Policy

| Variable | Default | Description |
|---|---|---|
| `MIN_REPLICAS` | `1` | Never scale below this |
| `MAX_REPLICAS` | `10` | Never scale above this |
| `MAX_SCALE_STEP` | `1` | Max replicas changed per action |
| `COOLDOWN` | `60s` | Minimum time between scaling actions per deployment |
| `CONFIDENCE_MIN` | `0.60` | Reject RL actions with confidence below this |

### Modes

| Variable | Default | Description |
|---|---|---|
| `TRAINING_MODE` | `true` | Passed to the RL agent to enable online learning |
| `MOCK_MODE` | `false` | Use synthetic metrics instead of querying Prometheus |

### Timeouts & Retries

| Variable | Default | Description |
|---|---|---|
| `PROM_TIMEOUT` | `3s` | Per-query Prometheus timeout |
| `RL_TIMEOUT` | `2s` | Per-request RL agent timeout |
| `K8S_TIMEOUT` | `5s` | Kubernetes API call timeout |
| `RETRY_MAX_ATTEMPTS` | `3` | Prometheus query retries |
| `RETRY_BASE_DELAY` | `250ms` | Initial retry back-off |
| `RETRY_MAX_DELAY` | `3s` | Maximum retry back-off |

### Circuit Breaker

| Variable | Default | Description |
|---|---|---|
| `CB_FAILURE_THRESHOLD` | `5` | Failures before the breaker opens |
| `CB_OPEN_DURATION` | `30s` | How long the breaker stays open |

### Fallback Rule-Based Scaler

| Variable | Default | Description |
|---|---|---|
| `FALLBACK_ENABLED` | `true` | Enable rule-based fallback when RL is unavailable |
| `FALLBACK_SCALEUP_CPU` | `0.75` | Scale up when CPU > threshold |
| `FALLBACK_SCALEUP_LATENCY` | `0.50` | Scale up when P95 latency > threshold (s) |
| `FALLBACK_SCALEUP_ERROR` | `0.05` | Scale up when error rate > threshold |
| `FALLBACK_SCALEUP_REQRATE` | `100.0` | Scale up when req/s > threshold (`0` = disabled) |
| `FALLBACK_SCALEDOWN_CPU` | `0.20` | Scale down threshold for CPU |
| `FALLBACK_SCALEDOWN_LATENCY` | `0.10` | Scale down threshold for P95 latency (s) |
| `FALLBACK_SCALEDOWN_ERROR` | `0.01` | Scale down threshold for error rate |

---

## ☸️ Deployment

### Label your Deployments

The controller only manages Deployments that carry the `rl-autoscale=true` label:

```bash
kubectl label deployment my-app rl-autoscale=true
```

Or add it directly in the Deployment manifest:

```yaml
metadata:
  labels:
    rl-autoscale: "true"
```

### RBAC

The controller needs read access to Deployments and Pods, and write access to scale Deployments:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: marls-controller
rules:
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "watch", "update"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: marls-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: marls-controller
subjects:
  - kind: ServiceAccount
    name: marls-controller
    namespace: marls-system
```

### Kubernetes Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: marls-controller
  namespace: marls-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app: marls-controller
  template:
    metadata:
      labels:
        app: marls-controller
    spec:
      serviceAccountName: marls-controller
      containers:
        - name: controller
          image: your-registry/marls-controller:latest
          env:
            - name: PROMETHEUS_URL
              value: "http://prometheus-operated.monitoring:9090"
            - name: RL_AGENT_URL
              value: "http://marls-rl-agent.marls-system:5000"
            - name: INTERVAL
              value: "30s"
            - name: TRAINING_MODE
              value: "true"
            - name: FALLBACK_ENABLED
              value: "true"
          resources:
            requests:
              cpu: 100m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 256Mi
```

### Docker Build

```bash
docker build -t your-registry/marls-controller:latest .
docker push your-registry/marls-controller:latest
```

---

## 💻 Local Development

Create a `.env` file (gitignored):

```bash
PROMETHEUS_URL=http://localhost:9090
RL_AGENT_URL=http://localhost:5000
INTERVAL=15s
MOCK_MODE=true
TRAINING_MODE=true
FALLBACK_ENABLED=true
MIN_REPLICAS=1
MAX_REPLICAS=5
```

Run locally:

```bash
go run ./... 
# or with kubeconfig pointing at a local cluster:
KUBECONFIG=~/.kube/config go run ./...
```

CLI flag overrides (local dev only):

```bash
go run ./... -mock -interval 10s -confidence-min 0.5
```

---

## 🧪 Testing

Run all tests:

```bash
go test ./...
```

Run only the validator unit tests with verbose output:

```bash
go test ./metrics/ -v -run TestValidate
```

### Validator Test Coverage

| Test ID | Name | What it covers |
|---|---|---|
| TC-UT-13 | `TestValidatePassesForValidMetrics` | Happy-path validation |
| TC-UT-14 | `TestValidateCPUOutOfRange` | CPU > 1.0 → error |
| TC-UT-15 | `TestValidateNegativeErrorRate` | ErrorRate < 0 → error |
| TC-UT-16 | `TestValidateAllZeroMetrics` | All-zero → warning |
| TC-UT-17 | `TestValidateHighLatencyWarning` | P95 > 0.5s → warning |
| TC-UT-18 | `TestSanitizeClampsHighCPU` | CPU 2.5 → clamped to 1.0 |
| TC-UT-19 | `TestSanitizeClampsNegativeLatency` | P95 -0.5 → clamped to 0.0 |
| TC-UT-20 | `TestValidateCriticalCPUWarning` | CPU 0.95 → in-range but warning |

---

## 📊 Observability

The controller emits **structured JSON logs** via `go.uber.org/zap` + `go-logr/zapr`. Every significant event is logged with consistent key-value fields, making logs easy to query in Loki, Datadog, or CloudWatch.

### Key Log Events

| Event | Fields |
|---|---|
| Reconciliation start/end | `namespace`, `deployment`, `action`, `from`, `to` |
| RL agent response | `action`, `actionID`, `confidence`, `elapsed` |
| Fallback decision | `action`, `reason` |
| Confidence rejection | `confidence`, `confidenceMin` |
| Cooldown suppression | `cooldown` |
| Scaling applied | `action`, `confidence`, `from`, `to`, `usingFallback` |
| Prometheus query result | `metric`, `value`, `duration` |
| Circuit breaker state | implicit via error messages |

### Recommended Prometheus Alerts

```yaml
# Controller not producing scaling decisions
- alert: MARLSControllerSilent
  expr: absent(marls_scaling_actions_total) > 0
  for: 5m

# RL agent circuit breaker repeatedly opening
- alert: MARLSRLAgentUnreachable
  expr: increase(marls_rl_failures_total[5m]) > 5
```

---

## 🐛 Troubleshooting

**`PROMETHEUS_URL is required` on startup**
Set `PROMETHEUS_URL` as an environment variable. The `.env` file is only loaded when present and is not a substitute in Kubernetes Pod specs.

**`not in cluster and kubeconfig not found`**
The controller is running outside a cluster and cannot find `~/.kube/config`. Set `KUBECONFIG` or run inside a Pod with a properly bound `ServiceAccount`.

**All scaling decisions use the fallback scaler**
The RL agent is likely unreachable. Check `RL_AGENT_URL`, verify the Flask service is running (`GET /health`), and inspect the circuit breaker log messages. If the circuit breaker is repeatedly opening, increase `CB_OPEN_DURATION` to reduce retry pressure while the agent recovers.

**Scaling never happens despite high CPU**
Check in order: (1) Is the Deployment labelled `rl-autoscale=true`? (2) Is `CONFIDENCE_MIN` too high for the agent's current training stage? (3) Is a cooldown window still active? All three are visible in structured logs.

**Latency metrics are always 0**
The application does not expose `http_request_duration_seconds_bucket`. The collector will fall back to a CPU-throttle proxy only when throttling exceeds 5%. Consider instrumenting your app with a Prometheus histogram.

**`flag redefined` panic in tests**
The `applyFlagOverrides` function uses a fresh `flag.FlagSet` per call — never `flag.CommandLine` — so this should not occur. If it does, ensure no other package registers flags on `flag.CommandLine` during test setup.

---

## 🔗 Integration with the RL Agent

The controller sends a `POST /predict` request to the Flask RL agent on every cycle per deployment:

```json
{
  "deployment_name": "my-app",
  "namespace": "marls-test",
  "metrics": { "cpu_usage": 0.72, "latency_p95": 0.41, "replicas": 2, "..." : "..." },
  "training_mode": true,
  "timestamp": 1714483200
}
```

Expected response:

```json
{
  "success": true,
  "action": 2,
  "action_name": "scale_up",
  "confidence": 0.83,
  "value_estimate": 1.92,
  "buffer_size": 31,
  "training_steps": 214
}
```

If `success` is `false`, or the HTTP status is non-2xx, or the request times out, the controller records a circuit breaker failure and falls back to the rule-based scaler for that cycle.

---

## 📚 Related

- [MARLS Flask RL Agent Service (PPO)](./go_agent_readme.md) — the Python counterpart this controller talks to.
- [Proximal Policy Optimization (Schulman et al., 2017)](https://arxiv.org/abs/1707.06347)
- [Kubernetes client-go](https://github.com/kubernetes/client-go)
- [go-logr/logr](https://github.com/go-logr/logr)