package config_test

import (
	"os"
	"testing"

	"go-controller-agent/config"
)

func setEnv(t *testing.T, key, value string) {
	t.Helper()
	os.Setenv(key, value)
	t.Cleanup(func() { os.Unsetenv(key) })
}

func baseEnv(t *testing.T) {
	setEnv(t, "PROMETHEUS_URL", "http://34.169.103.220:30228")
	setEnv(t, "RL_AGENT_URL", "http://127.0.0.1:5000")
}

// TC-UT-06: Valid configuration loads without error
func TestLoadValidConfig(t *testing.T) {
	baseEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected no error for valid config, got: %v", err)
	}
	if cfg.PrometheusURL != "http://34.169.103.220:30228" {
		t.Errorf("unexpected PrometheusURL: %s", cfg.PrometheusURL)
	}
}

// TC-UT-07: Missing PROMETHEUS_URL causes validation error
func TestLoadMissingPrometheusURL(t *testing.T) {
	setEnv(t, "RL_AGENT_URL", "http://127.0.0.1:5000")
	_, err := config.Load()
	if err == nil {
		t.Error("expected error when PROMETHEUS_URL is missing")
	}
}

// TC-UT-08: Missing RL_AGENT_URL causes validation error
func TestLoadMissingRLAgentURL(t *testing.T) {
	setEnv(t, "PROMETHEUS_URL", "http://34.169.103.220:30228")
	_, err := config.Load()
	if err == nil {
		t.Error("expected error when RL_AGENT_URL is missing")
	}
}

// TC-UT-09: MAX_REPLICAS less than MIN_REPLICAS causes validation error
func TestLoadInvalidReplicaBounds(t *testing.T) {
	baseEnv(t)
	setEnv(t, "MIN_REPLICAS", "5")
	setEnv(t, "MAX_REPLICAS", "2")
	_, err := config.Load()
	if err == nil {
		t.Error("expected error when MAX_REPLICAS < MIN_REPLICAS")
	}
}

// TC-UT-10: CONFIDENCE_MIN outside [0,1] causes validation error
func TestLoadInvalidConfidenceMin(t *testing.T) {
	baseEnv(t)
	setEnv(t, "CONFIDENCE_MIN", "1.5")
	_, err := config.Load()
	if err == nil {
		t.Error("expected error when CONFIDENCE_MIN > 1")
	}
}

// TC-UT-11: INTERVAL below 5s causes validation error
func TestLoadIntervalTooShort(t *testing.T) {
	baseEnv(t)
	setEnv(t, "INTERVAL", "2s")
	_, err := config.Load()
	if err == nil {
		t.Error("expected error when INTERVAL < 5s")
	}
}

// TC-UT-12: Default values are applied when optional vars are absent
func TestLoadDefaults(t *testing.T) {
	baseEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MinReplicas != 1 {
		t.Errorf("expected default MinReplicas=1, got %d", cfg.MinReplicas)
	}
	if cfg.MaxReplicas != 10 {
		t.Errorf("expected default MaxReplicas=10, got %d", cfg.MaxReplicas)
	}
	if cfg.MaxConcurrent != 4 {
		t.Errorf("expected default MaxConcurrent=4, got %d", cfg.MaxConcurrent)
	}
}
