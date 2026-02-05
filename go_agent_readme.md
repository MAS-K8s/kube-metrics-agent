# 🔧 Go Agent Controller

> Kubernetes controller that integrates with RL Agent for intelligent autoscaling decisions.

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8.svg)](https://golang.org/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.28+-326CE5.svg)](https://kubernetes.io/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

---

## 📋 Table of Contents

- [Overview](#overview)
- [Architecture](#architecture)
- [Features](#features)
- [Prerequisites](#prerequisites)
- [Installation](#installation)
- [Configuration](#configuration)
- [Usage](#usage)
- [Monitoring](#monitoring)
- [Troubleshooting](#troubleshooting)

---

## 🎯 Overview

The Go Agent Controller is the "executor" of the autoscaling system. It acts as a bridge between Kubernetes and the Flask RL Agent Service by:

- **Collecting** metrics from Prometheus
- **Querying** the RL agent for scaling decisions
- **Executing** scaling actions on Kubernetes deployments
- **Monitoring** deployment health and status

### What This Service Does

```
┌─────────────────────────────────────────────────────────┐
│                  Go Agent Controller                     │
│                                                          │
│  ┌────────────────────────────────────────────────┐    │
│  │  Main Control Loop (Every 30s)                 │    │
│  │  1. Get current deployment state               │    │
│  │  2. Query Prometheus for metrics               │    │
│  │  3. Send to RL Agent for decision              │    │
│  │  4. Execute scaling action                     │    │
│  └────────────────────────────────────────────────┘    │
│                                                          │
└─────────────────────────────────────────────────────────┘
```

---

## 🏗️ Architecture

### System Integration

```
┌──────────────────────────────────────────────────────────────┐
│                    Complete System Flow                       │
└──────────────────────────────────────────────────────────────┘

┌─────────────┐         ┌──────────────┐         ┌────────────┐
│ Kubernetes  │         │ Prometheus   │         │  Flask RL  │
│   Cluster   │         │   (Metrics)  │         │   Agent    │
└──────┬──────┘         └──────┬───────┘         └─────┬──────┘
       │                       │                        │
       │                       │                        │
       └───────────────────────┴────────────────────────┘
                               │
                               │
                    ┌──────────▼──────────┐
                    │  Go Agent Controller│
                    │                     │
                    │  ┌───────────────┐ │
                    │  │ 1. Collect    │ │
                    │  │    Metrics    │ │
                    │  └───────┬───────┘ │
                    │          │         │
                    │  ┌───────▼───────┐ │
                    │  │ 2. Query RL   │ │
                    │  │    Agent      │ │
                    │  └───────┬───────┘ │
                    │          │         │
                    │  ┌───────▼───────┐ │
                    │  │ 3. Execute    │ │
                    │  │    Scaling    │ │
                    │  └───────────────┘ │
                    └─────────────────────┘
```

### Control Flow (Every Interval)

```
Start
  │
  ├─► 1. Get Current Deployment State
  │       └─► kubectl get deployment <name>
  │            └─► Current replicas: 3
  │
  ├─► 2. Query Prometheus for Metrics
  │       ├─► CPU usage: 75%
  │       ├─► Memory usage: 1.2GB
  │       ├─► Request rate: 120 req/s
  │       ├─► Latency P95: 450ms
  │       └─► Error rate: 1%
  │
  ├─► 3. Package Metrics into JSON
  │       └─► {
  │             "deployment_name": "myapp",
  │             "namespace": "default",
  │             "metrics": {
  │               "cpu_usage": 0.75,
  │               "memory_usage": 1.2,
  │               ...
  │             }
  │           }
  │
  ├─► 4. HTTP POST to Flask RL Agent
  │       └─► http://localhost:5000/predict
  │            └─► Response: {
  │                  "action": 2,
  │                  "action_name": "scale_up"
  │                }
  │
  ├─► 5. Execute Scaling Decision
  │       └─► If action == 2 (scale_up):
  │            └─► kubectl scale deployment myapp --replicas=4
  │       └─► If action == 0 (scale_down):
  │            └─► kubectl scale deployment myapp --replicas=2
  │       └─► If action == 1 (no_action):
  │            └─► Do nothing
  │
  ├─► 6. Log Results
  │       └─► ✅ Updated deployment replicas from 3 to 4
  │
  └─► 7. Sleep for Interval (30s)
        └─► Repeat
```

### Fallback Mechanism

```
┌─────────────────────────────────────┐
│  Try: Query RL Agent                │
└─────────────┬───────────────────────┘
              │
              ├─► Success?
              │   └─► Yes → Use RL decision
              │
              └─► Failure?
                  └─► Yes → Use rule-based fallback
                      ├─► If CPU > 70% → scale_up
                      ├─► If CPU < 30% → scale_down
                      └─► Else → no_action
```

---

## ✨ Features

- 🔄 **Continuous Monitoring** - Polls metrics at configurable intervals
- 🧠 **RL Integration** - Queries Flask RL Agent for intelligent decisions
- 🛡️ **Fallback Safety** - Rule-based scaling if RL agent unavailable
- 📊 **Prometheus Integration** - Collects comprehensive metrics
- 🎯 **Multi-Metric Support** - CPU, memory, latency, errors, etc.
- ⚙️ **Configurable Parameters** - Min/max replicas, intervals, thresholds
- 📝 **Structured Logging** - JSON logs for easy parsing
- 🔐 **RBAC Support** - Works with Kubernetes permissions

---

## 📦 Prerequisites

### Software Requirements

- **Go** 1.21 or higher
- **kubectl** configured with cluster access
- **Kubernetes cluster** (GKE, EKS, AKS, or minikube)
- **Prometheus** installed in cluster
- **Flask RL Agent** running

### Kubernetes Requirements

- Cluster access with deployment update permissions
- Prometheus installed (typically in `monitoring` namespace)
- At least one deployment to manage

### System Requirements

- **Memory**: 256MB RAM minimum
- **CPU**: 0.1 core minimum
- **Network**: Access to Kubernetes API and Prometheus

---

## 🚀 Installation

### Step 1: Clone or Create Project Structure

```bash
mkdir -p go-agent
cd go-agent
```

### Step 2: Initialize Go Module

```bash
# Initialize module
go mod init rl-controller

# Create requirements file for documentation
cat > requirements.txt << 'EOF'
github.com/go-logr/logr@v1.2.4
github.com/go-logr/zapr@v1.2.4
go.uber.org/zap@v1.24.0
k8s.io/client-go@v0.28.0
k8s.io/apimachinery@v0.28.0
k8s.io/api@v0.28.0
EOF
```

### Step 3: Install Dependencies

```bash
# Install all dependencies
go get github.com/go-logr/logr@v1.2.4
go get github.com/go-logr/zapr@v1.2.4
go get go.uber.org/zap@v1.24.0
go get k8s.io/client-go@v0.28.0
go get k8s.io/apimachinery@v0.28.0
go get k8s.io/api@v0.28.0

# Download all dependencies
go mod download

# Verify
go mod verify
```

### Step 4: Copy Source Files

Copy `main.go` to `go-agent/` directory.

### Step 5: Build the Application

```bash
# Build binary
go build -o rl-controller main.go

# Verify build
./rl-controller --help
```

---

## ⚙️ Configuration

### Command-Line Flags

| Flag | Description | Default | Example |
|------|-------------|---------|---------|
| `--namespace` | Kubernetes namespace | `default` | `--namespace=production` |
| `--deployment` | Deployment name to manage | `myapp` | `--deployment=nginx-app` |
| `--prometheus` | Prometheus base URL | `http://136.117.195.136:30980` | `--prometheus=http://localhost:9090` |
| `--rl-agent` | Flask RL Agent URL | `http://localhost:5000` | `--rl-agent=http://rl-agent:5000` |
| `--interval` | Polling interval | `30s` | `--interval=15s