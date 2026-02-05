package config

import (
	"fmt"
	"os"
	"time"
)

// Config holds all configuration for the controller
type Config struct {
	// Kubernetes settings
	Namespace  string
	Deployment string

	// External services
	PrometheusURL string
	RLAgentURL    string

	// Controller settings
	Interval       time.Duration
	MinReplicas    int32
	MaxReplicas    int32
	TrainingMode   bool
	MockMode       bool
	SafetyEnabled  bool
	MaxScalePerMin int32

	// Model settings
	ModelPath string
}

// Validate checks if configuration is valid
func (c *Config) Validate() error {
	if c.Namespace == "" {
		return fmt.Errorf("namespace cannot be empty")
	}

	if c.Deployment == "" {
		return fmt.Errorf("deployment cannot be empty")
	}

	if c.RLAgentURL == "" {
		return fmt.Errorf("RL agent URL cannot be empty")
	}

	if c.MinReplicas < 1 {
		return fmt.Errorf("min replicas must be >= 1, got %d", c.MinReplicas)
	}

	if c.MaxReplicas < c.MinReplicas {
		return fmt.Errorf("max replicas (%d) must be >= min replicas (%d)",
			c.MaxReplicas, c.MinReplicas)
	}

	if c.Interval < 10*time.Second {
		return fmt.Errorf("interval must be >= 10s, got %s", c.Interval)
	}

	if c.MaxScalePerMin < 1 {
		return fmt.Errorf("max scale per minute must be >= 1, got %d", c.MaxScalePerMin)
	}

	return nil
}

// LoadFromEnv loads configuration from environment variables
func LoadFromEnv() *Config {
	return &Config{
		Namespace:      getEnv("NAMESPACE", "default"),
		Deployment:     getEnv("DEPLOYMENT", "myapp"),
		PrometheusURL:  getEnv("PROMETHEUS_URL", "http://prometheus:9090"),
		RLAgentURL:     getEnv("RL_AGENT_URL", "http://rl-agent-service:5000"),
		Interval:       getDurationEnv("INTERVAL", 30*time.Second),
		MinReplicas:    int32(getIntEnv("MIN_REPLICAS", 1)),
		MaxReplicas:    int32(getIntEnv("MAX_REPLICAS", 10)),
		TrainingMode:   getBoolEnv("TRAINING_MODE", true),
		MockMode:       getBoolEnv("MOCK_MODE", false),
		SafetyEnabled:  getBoolEnv("SAFETY_ENABLED", true),
		MaxScalePerMin: int32(getIntEnv("MAX_SCALE_PER_MIN", 3)),
		ModelPath:      getEnv("MODEL_PATH", "./models/rl_model.pt"),
	}
}

// getEnv gets an environment variable with a default value
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getIntEnv gets an integer environment variable with a default
func getIntEnv(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		var intValue int
		if _, err := fmt.Sscanf(value, "%d", &intValue); err == nil {
			return intValue
		}
	}
	return defaultValue
}

// getBoolEnv gets a boolean environment variable with a default
func getBoolEnv(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		return value == "true" || value == "1" || value == "yes"
	}
	return defaultValue
}

// getDurationEnv gets a duration environment variable with a default
func getDurationEnv(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return defaultValue
}

// String returns a string representation of the config
func (c *Config) String() string {
	return fmt.Sprintf(
		"Config{Namespace:%s, Deployment:%s, Interval:%s, MinReplicas:%d, MaxReplicas:%d, TrainingMode:%t, MockMode:%t}",
		c.Namespace,
		c.Deployment,
		c.Interval,
		c.MinReplicas,
		c.MaxReplicas,
		c.TrainingMode,
		c.MockMode,
	)
}