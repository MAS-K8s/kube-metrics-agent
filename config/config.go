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
}

// Load is “production first”:
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
	}

	// Optional: allow CLI overrides (local dev)
	// In Kubernetes you normally won’t pass flags, so this won’t conflict.
	applyFlagOverrides(&cfg)

	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyFlagOverrides(cfg *Config) {
	// NOTE: only use these when you intentionally pass flags.
	var (
		ns          = flag.String("namespace", "", "Override namespace (empty=all)")
		selector    = flag.String("selector", "", "Override label selector")
		prom        = flag.String("prometheus", "", "Override Prometheus URL")
		rl          = flag.String("rl-agent", "", "Override RL Agent URL")
		intervalStr = flag.String("interval", "", "Override interval, e.g. 30s")

		minRep  = flag.Int("min-replicas", -1, "Override min replicas")
		maxRep  = flag.Int("max-replicas", -1, "Override max replicas")
		step    = flag.Int("max-scale-step", -1, "Override max scale step")
		coolStr = flag.String("cooldown", "", "Override cooldown, e.g. 60s")

		conf = flag.Float64("confidence-min", -1, "Override confidence min 0..1")
		mock = flag.Bool("mock", cfg.MockMode, "Override mock mode")
		train = flag.Bool("training", cfg.TrainingMode, "Override training mode")
	)

	flag.Parse()

	if *ns != "" || flag.Lookup("namespace").Value.String() == "" {
		// If user passed -namespace explicitly (even empty), accept it:
		if flagIsSet("namespace") {
			cfg.Namespace = *ns
		}
	}
	if flagIsSet("selector") && *selector != "" {
		cfg.LabelSelector = *selector
	}
	if flagIsSet("prometheus") && *prom != "" {
		cfg.PrometheusURL = *prom
	}
	if flagIsSet("rl-agent") && *rl != "" {
		cfg.RLAgentURL = *rl
	}
	if flagIsSet("interval") && *intervalStr != "" {
		cfg.Interval = mustDuration(*intervalStr)
	}
	if flagIsSet("min-replicas") && *minRep >= 0 {
		cfg.MinReplicas = int32(*minRep)
	}
	if flagIsSet("max-replicas") && *maxRep >= 0 {
		cfg.MaxReplicas = int32(*maxRep)
	}
	if flagIsSet("max-scale-step") && *step >= 0 {
		cfg.MaxScaleStep = int32(*step)
	}
	if flagIsSet("cooldown") && *coolStr != "" {
		cfg.Cooldown = mustDuration(*coolStr)
	}
	if flagIsSet("confidence-min") && *conf >= 0 {
		cfg.ConfidenceMin = *conf
	}

	// bool flags always have a value, so we only set if flag explicitly provided
	if flagIsSet("mock") {
		cfg.MockMode = *mock
	}
	if flagIsSet("training") {
		cfg.TrainingMode = *train
	}
}

func flagIsSet(name string) bool {
	f := flag.Lookup(name)
	return f != nil && f.Value.String() != f.DefValue
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