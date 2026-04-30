package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Watch scope ("" = all namespaces)
	Namespace     string
	LabelSelector string

	// External endpoints (use in-cluster DNS names in K8s)
	PrometheusURL string
	RLAgentURL    string

	// Loop
	Interval time.Duration

	// Scaling policy
	MinReplicas   int32
	MaxReplicas   int32
	MaxScaleStep  int32
	Cooldown      time.Duration
	ConfidenceMin float64

	// Modes
	TrainingMode bool
	MockMode     bool

	// Timeouts
	PrometheusTimeout time.Duration
	RLAgentTimeout    time.Duration
	K8sTimeout        time.Duration

	// Retry/backoff
	RetryMaxAttempts int
	RetryBaseDelay   time.Duration
	RetryMaxDelay    time.Duration

	// Circuit breaker
	CBFailureThreshold int
	CBOpenDuration     time.Duration

	// Concurrency
	MaxConcurrent int

	// Fallback rule-based scaler
	// Enabled automatically when the RL agent is unavailable; can also be
	// forced on with FALLBACK_ENABLED=true to run rule-based-only mode.
	FallbackEnabled bool

	// Scale-up thresholds (any one exceeded → scale_up)
	FallbackScaleUpCPU     float64 // FALLBACK_SCALEUP_CPU     (default 0.75)
	FallbackScaleUpLatency float64 // FALLBACK_SCALEUP_LATENCY (default 0.50 s)
	FallbackScaleUpError   float64 // FALLBACK_SCALEUP_ERROR   (default 0.05)
	FallbackScaleUpReqRate float64 // FALLBACK_SCALEUP_REQRATE (default 100.0, 0=disabled)

	// Scale-down thresholds (all must be below → scale_down)
	FallbackScaleDownCPU     float64 // FALLBACK_SCALEDOWN_CPU     (default 0.20)
	FallbackScaleDownLatency float64 // FALLBACK_SCALEDOWN_LATENCY (default 0.10 s)
	FallbackScaleDownError   float64 // FALLBACK_SCALEDOWN_ERROR   (default 0.01)
}

// Load is "production first":
// - env vars are the main config (K8s style)
// - flags exist ONLY to override for local dev
func Load() (Config, error) {
	cfg := Config{
		Namespace:     getenv("NAMESPACE", ""), // "" => all namespaces
		LabelSelector: getenv("LABEL_SELECTOR", "rl-autoscale=true"),

		PrometheusURL: mustGetenv("PROMETHEUS_URL"), // require in prod
		RLAgentURL:    mustGetenv("RL_AGENT_URL"),   // require in prod

		Interval: mustDuration(getenv("INTERVAL", "30s")),

		MinReplicas:  int32(mustInt(getenv("MIN_REPLICAS", "1"))),
		MaxReplicas:  int32(mustInt(getenv("MAX_REPLICAS", "10"))),
		MaxScaleStep: int32(mustInt(getenv("MAX_SCALE_STEP", "1"))),

		Cooldown:      mustDuration(getenv("COOLDOWN", "60s")),
		ConfidenceMin: mustFloat(getenv("CONFIDENCE_MIN", "0.60")),

		TrainingMode: mustBool(getenv("TRAINING_MODE", "true")),
		MockMode:     mustBool(getenv("MOCK_MODE", "false")),

		PrometheusTimeout: mustDuration(getenv("PROM_TIMEOUT", "3s")),
		RLAgentTimeout:    mustDuration(getenv("RL_TIMEOUT", "2s")),
		K8sTimeout:        mustDuration(getenv("K8S_TIMEOUT", "5s")),

		RetryMaxAttempts: mustInt(getenv("RETRY_MAX_ATTEMPTS", "3")),
		RetryBaseDelay:   mustDuration(getenv("RETRY_BASE_DELAY", "250ms")),
		RetryMaxDelay:    mustDuration(getenv("RETRY_MAX_DELAY", "3s")),

		CBFailureThreshold: mustInt(getenv("CB_FAILURE_THRESHOLD", "5")),
		CBOpenDuration:     mustDuration(getenv("CB_OPEN_DURATION", "30s")),

		MaxConcurrent: mustInt(getenv("MAX_CONCURRENT", "4")),

		// Fallback rule-based scaler
		FallbackEnabled: mustBool(getenv("FALLBACK_ENABLED", "true")),

		FallbackScaleUpCPU:     mustFloat(getenv("FALLBACK_SCALEUP_CPU", "0.75")),
		FallbackScaleUpLatency: mustFloat(getenv("FALLBACK_SCALEUP_LATENCY", "0.50")),
		FallbackScaleUpError:   mustFloat(getenv("FALLBACK_SCALEUP_ERROR", "0.05")),
		FallbackScaleUpReqRate: mustFloat(getenv("FALLBACK_SCALEUP_REQRATE", "100.0")),

		FallbackScaleDownCPU:     mustFloat(getenv("FALLBACK_SCALEDOWN_CPU", "0.20")),
		FallbackScaleDownLatency: mustFloat(getenv("FALLBACK_SCALEDOWN_LATENCY", "0.10")),
		FallbackScaleDownError:   mustFloat(getenv("FALLBACK_SCALEDOWN_ERROR", "0.01")),
	}

	// Optional: allow CLI overrides (local dev)
	// In Kubernetes you normally won't pass flags, so this won't conflict.
	applyFlagOverrides(&cfg)

	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyFlagOverrides(cfg *Config) {
	// Use a fresh FlagSet each call so repeated Load() calls in tests
	// never hit "flag redefined" panics on the global CommandLine.
	fs := flag.NewFlagSet("config", flag.ContinueOnError)

	var (
		ns          = fs.String("namespace", "", "Override namespace (empty=all)")
		selector    = fs.String("selector", "", "Override label selector")
		prom        = fs.String("prometheus", "", "Override Prometheus URL")
		rl          = fs.String("rl-agent", "", "Override RL Agent URL")
		intervalStr = fs.String("interval", "", "Override interval, e.g. 30s")

		minRep  = fs.Int("min-replicas", -1, "Override min replicas")
		maxRep  = fs.Int("max-replicas", -1, "Override max replicas")
		step    = fs.Int("max-scale-step", -1, "Override max scale step")
		coolStr = fs.String("cooldown", "", "Override cooldown, e.g. 60s")

		conf  = fs.Float64("confidence-min", -1, "Override confidence min 0..1")
		mock  = fs.Bool("mock", cfg.MockMode, "Override mock mode")
		train = fs.Bool("training", cfg.TrainingMode, "Override training mode")
	)

	// Ignore parse errors: unknown flags from `go test` (e.g. -test.v) are fine.
	fs.Parse(os.Args[1:]) //nolint:errcheck

	// fs.Visit only iterates flags that were explicitly set on the CLI,
	// so we never accidentally overwrite env-sourced values.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "namespace":
			cfg.Namespace = *ns
		case "selector":
			if *selector != "" {
				cfg.LabelSelector = *selector
			}
		case "prometheus":
			if *prom != "" {
				cfg.PrometheusURL = *prom
			}
		case "rl-agent":
			if *rl != "" {
				cfg.RLAgentURL = *rl
			}
		case "interval":
			if *intervalStr != "" {
				cfg.Interval = mustDuration(*intervalStr)
			}
		case "min-replicas":
			if *minRep >= 0 {
				cfg.MinReplicas = int32(*minRep)
			}
		case "max-replicas":
			if *maxRep >= 0 {
				cfg.MaxReplicas = int32(*maxRep)
			}
		case "max-scale-step":
			if *step >= 0 {
				cfg.MaxScaleStep = int32(*step)
			}
		case "cooldown":
			if *coolStr != "" {
				cfg.Cooldown = mustDuration(*coolStr)
			}
		case "confidence-min":
			if *conf >= 0 {
				cfg.ConfidenceMin = *conf
			}
		case "mock":
			cfg.MockMode = *mock
		case "training":
			cfg.TrainingMode = *train
		}
	})
}

func validate(c Config) error {
	if c.LabelSelector == "" {
		return fmt.Errorf("LABEL_SELECTOR cannot be empty")
	}
	if c.PrometheusURL == "" {
		return fmt.Errorf("PROMETHEUS_URL is required (set it as env in Kubernetes)")
	}
	if c.RLAgentURL == "" {
		return fmt.Errorf("RL_AGENT_URL is required (set it as env in Kubernetes)")
	}
	if c.MinReplicas < 1 {
		return fmt.Errorf("MIN_REPLICAS must be >= 1")
	}
	if c.MaxReplicas < c.MinReplicas {
		return fmt.Errorf("MAX_REPLICAS must be >= MIN_REPLICAS")
	}
	if c.MaxScaleStep < 1 {
		return fmt.Errorf("MAX_SCALE_STEP must be >= 1")
	}
	if c.Interval < 5*time.Second {
		return fmt.Errorf("INTERVAL must be >= 5s")
	}
	if c.ConfidenceMin < 0 || c.ConfidenceMin > 1 {
		return fmt.Errorf("CONFIDENCE_MIN must be between 0 and 1")
	}
	return nil
}

// -------- env helpers --------

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustGetenv(key string) string {
	v := os.Getenv(key)
	// allow empty during local dev if you want, but in prod validation will fail
	return v
}

func mustInt(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(fmt.Sprintf("invalid int: %q", s))
	}
	return n
}

func mustFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		panic(fmt.Sprintf("invalid float: %q", s))
	}
	return f
}

func mustBool(s string) bool {
	b, err := strconv.ParseBool(s)
	if err != nil {
		panic(fmt.Sprintf("invalid bool: %q", s))
	}
	return b
}

func mustDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(fmt.Sprintf("invalid duration: %q", s))
	}
	return d
}