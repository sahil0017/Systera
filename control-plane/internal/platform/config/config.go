package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds control-plane configuration loaded from environment variables.
type Config struct {
	Env             string
	HTTPAddr        string
	WebDir          string
	LogLevel        string
	ShutdownTimeout time.Duration

	// DatabaseURL, when set, overrides the discrete SYSTERA_POSTGRES_* settings.
	DatabaseURL      string
	PostgresHost     string
	PostgresPort     int
	PostgresDB       string
	PostgresUser     string
	PostgresPassword string
	PostgresSSLMode  string
	// AutoMigrate applies pending embedded migrations when the API starts.
	AutoMigrate bool

	// Auth: bearer JWTs are verified server-side with RS256 via JWKS or a static RSA public key.
	AuthJWKSURL        string
	AuthPublicKeyPEM   string
	AuthPublicKeyFile  string
	AuthPrivateKeyFile string
	AuthKeyID          string
	AuthIssuer         string
	AuthAudience       string

	SSEPollInterval      time.Duration
	SSEHeartbeatInterval time.Duration

	NATSURL           string
	NATSStream        string
	NATSSubjectPrefix string
	RedisAddr         string

	// Scheduler (cmd/scheduler): outbox relay + lease reclamation + starvation checks.
	SchedulerHTTPAddr string
	RelayInterval     time.Duration
	RelayBatchSize    int
	ReclaimInterval   time.Duration

	// Agent worker (cmd/worker-agent).
	WorkerID         string
	WorkerAgentTypes []string
	WorkerHTTPAddr   string

	// Engineering Pulse research worker (cmd/worker-research).
	PulseRegistryFile  string        // registry definitions synced at start-up ("" = skip)
	PulseInterval      time.Duration // pipeline cycle interval
	PulseUserAgent     string        // generic crawler identity; never personal details
	PulseGitHubToken   string        // optional; raises the GitHub API rate limit
	ResearchHTTPAddr   string        // probe address of the research worker
	LeaseTTL           time.Duration
	WorkerPollInterval time.Duration

	// Model Gateway. ModelProvider is "openai" (any OpenAI-compatible endpoint, e.g.
	// Ollama, vLLM, Together), "groq", "openrouter", or "huggingface".
	ModelProvider    string
	ModelBaseURL     string
	ModelAPIKey      string
	ModelDefault     string
	ModelPricingJSON string
	ModelTimeout     time.Duration
	ModelTemperature *float64

	S3Endpoint  string
	S3Region    string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
}

// Load reads configuration from environment variables with safe local defaults.
func Load() (Config, error) {
	port, err := envInt("SYSTERA_POSTGRES_PORT", 5432)
	if err != nil {
		return Config{}, fmt.Errorf("invalid SYSTERA_POSTGRES_PORT: %w", err)
	}
	shutdownTimeout, err := envDuration("SYSTERA_SHUTDOWN_TIMEOUT", 15*time.Second)
	if err != nil {
		return Config{}, fmt.Errorf("invalid SYSTERA_SHUTDOWN_TIMEOUT: %w", err)
	}
	autoMigrate, err := envBool("SYSTERA_AUTO_MIGRATE", false)
	if err != nil {
		return Config{}, fmt.Errorf("invalid SYSTERA_AUTO_MIGRATE: %w", err)
	}
	ssePoll, err := envDuration("SYSTERA_SSE_POLL_INTERVAL", 2*time.Second)
	if err != nil {
		return Config{}, fmt.Errorf("invalid SYSTERA_SSE_POLL_INTERVAL: %w", err)
	}
	sseHeartbeat, err := envDuration("SYSTERA_SSE_HEARTBEAT_INTERVAL", 15*time.Second)
	if err != nil {
		return Config{}, fmt.Errorf("invalid SYSTERA_SSE_HEARTBEAT_INTERVAL: %w", err)
	}

	durations := map[string]*time.Duration{}
	relayInterval, reclaimInterval, leaseTTL, workerPoll, modelTimeout := 500*time.Millisecond, 5*time.Second, 60*time.Second, time.Second, 2*time.Minute
	durations["SYSTERA_RELAY_INTERVAL"] = &relayInterval
	durations["SYSTERA_RECLAIM_INTERVAL"] = &reclaimInterval
	durations["SYSTERA_LEASE_TTL"] = &leaseTTL
	durations["SYSTERA_WORKER_POLL_INTERVAL"] = &workerPoll
	durations["SYSTERA_MODEL_TIMEOUT"] = &modelTimeout
	pulseInterval := 15 * time.Minute
	durations["SYSTERA_PULSE_INTERVAL"] = &pulseInterval
	for key, dst := range durations {
		v, err := envDuration(key, *dst)
		if err != nil {
			return Config{}, fmt.Errorf("invalid %s: %w", key, err)
		}
		if v <= 0 {
			return Config{}, fmt.Errorf("invalid %s: must be positive", key)
		}
		*dst = v
	}
	relayBatch, err := envInt("SYSTERA_RELAY_BATCH_SIZE", 100)
	if err != nil || relayBatch <= 0 {
		return Config{}, fmt.Errorf("invalid SYSTERA_RELAY_BATCH_SIZE")
	}
	var temperature *float64
	if v := os.Getenv("SYSTERA_MODEL_TEMPERATURE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 2 {
			return Config{}, fmt.Errorf("invalid SYSTERA_MODEL_TEMPERATURE")
		}
		temperature = &f
	}
	provider := envStr("SYSTERA_MODEL_PROVIDER", "openai")
	defaultBaseURL, defaultModel, defaultPricing := "http://localhost:11434/v1", "llama3.2", `{"llama3.2":{"input_per_mtok":0,"output_per_mtok":0}}`
	fallbackAPIKeyEnv := "OPENAI_API_KEY"
	switch provider {
	case "openai":
	case "groq":
		defaultBaseURL = "https://api.groq.com/openai/v1"
		defaultModel = "llama-3.3-70b-versatile"
		defaultPricing = `{"llama-3.3-70b-versatile":{"input_per_mtok":0,"output_per_mtok":0}}`
		fallbackAPIKeyEnv = "GROQ_API_KEY"
	case "openrouter":
		defaultBaseURL = "https://openrouter.ai/api/v1"
		// Free open-weight model (":free" variants cost $0). The worker verifies at startup
		// that the model is still served, since OpenRouter's free lineup changes.
		defaultModel = "google/gemma-4-31b-it:free"
		defaultPricing = `{"google/gemma-4-31b-it:free":{"input_per_mtok":0,"output_per_mtok":0}}`
		fallbackAPIKeyEnv = "OPENROUTER_API_KEY"
	case "huggingface":
		defaultBaseURL = "https://router.huggingface.co/v1"
		defaultModel = "meta-llama/Llama-3.3-70B-Instruct"
		defaultPricing = `{"meta-llama/Llama-3.3-70B-Instruct":{"input_per_mtok":0,"output_per_mtok":0}}`
		fallbackAPIKeyEnv = "HF_TOKEN"
	default:
		return Config{}, fmt.Errorf("invalid SYSTERA_MODEL_PROVIDER %q (openai|groq|openrouter|huggingface)", provider)
	}
	modelAPIKey := os.Getenv("SYSTERA_MODEL_API_KEY")
	if modelAPIKey == "" {
		modelAPIKey = os.Getenv(fallbackAPIKeyEnv)
	}
	workerID := os.Getenv("SYSTERA_WORKER_ID")
	if workerID == "" {
		host, _ := os.Hostname()
		workerID = fmt.Sprintf("%s-%d", host, os.Getpid())
	}

	return Config{
		WebDir:            os.Getenv("SYSTERA_WEB_DIR"),
		SchedulerHTTPAddr: envStr("SYSTERA_SCHEDULER_HTTP_ADDR", ":8081"),
		RelayInterval:     relayInterval,
		RelayBatchSize:    relayBatch,
		ReclaimInterval:   reclaimInterval,

		WorkerID:           workerID,
		WorkerAgentTypes:   envList("SYSTERA_WORKER_AGENT_TYPES", []string{"requirements_analyst"}),
		WorkerHTTPAddr:     envStr("SYSTERA_WORKER_HTTP_ADDR", ":8082"),
		PulseRegistryFile:  os.Getenv("SYSTERA_PULSE_REGISTRY_FILE"),
		PulseInterval:      pulseInterval,
		PulseUserAgent:     os.Getenv("SYSTERA_PULSE_USER_AGENT"),
		PulseGitHubToken:   os.Getenv("SYSTERA_GITHUB_TOKEN"),
		ResearchHTTPAddr:   envStr("SYSTERA_RESEARCH_HTTP_ADDR", ":8083"),
		LeaseTTL:           leaseTTL,
		WorkerPollInterval: workerPoll,

		ModelProvider:    provider,
		ModelBaseURL:     envStr("SYSTERA_MODEL_BASE_URL", defaultBaseURL),
		ModelAPIKey:      modelAPIKey,
		ModelDefault:     envStr("SYSTERA_MODEL_DEFAULT", defaultModel),
		ModelPricingJSON: envStr("SYSTERA_MODEL_PRICING", defaultPricing),
		ModelTimeout:     modelTimeout,
		ModelTemperature: temperature,

		Env:             envStr("SYSTERA_ENV", "development"),
		HTTPAddr:        envStr("SYSTERA_HTTP_ADDR", ":8080"),
		LogLevel:        envStr("SYSTERA_LOG_LEVEL", "info"),
		ShutdownTimeout: shutdownTimeout,

		DatabaseURL:      os.Getenv("SYSTERA_DATABASE_URL"),
		PostgresHost:     envStr("SYSTERA_POSTGRES_HOST", "localhost"),
		PostgresPort:     port,
		PostgresDB:       envStr("SYSTERA_POSTGRES_DB", "systera"),
		PostgresUser:     envStr("SYSTERA_POSTGRES_USER", "systera"),
		PostgresPassword: envStr("SYSTERA_POSTGRES_PASSWORD", "systera_dev_password"),
		PostgresSSLMode:  envStr("SYSTERA_POSTGRES_SSLMODE", "disable"),
		AutoMigrate:      autoMigrate,

		AuthJWKSURL:        os.Getenv("SYSTERA_AUTH_JWKS_URL"),
		AuthPublicKeyPEM:   os.Getenv("SYSTERA_AUTH_PUBLIC_KEY_PEM"),
		AuthPublicKeyFile:  os.Getenv("SYSTERA_AUTH_PUBLIC_KEY_FILE"),
		AuthPrivateKeyFile: os.Getenv("SYSTERA_AUTH_PRIVATE_KEY_FILE"),
		AuthKeyID:          os.Getenv("SYSTERA_AUTH_KEY_ID"),
		AuthIssuer:         envStr("SYSTERA_AUTH_ISSUER", "systera"),
		AuthAudience:       envStr("SYSTERA_AUTH_AUDIENCE", "systera-control-plane"),

		SSEPollInterval:      ssePoll,
		SSEHeartbeatInterval: sseHeartbeat,

		NATSURL:           envStr("SYSTERA_NATS_URL", "nats://localhost:4222"),
		NATSStream:        envStr("SYSTERA_NATS_STREAM", "SYSTERA_EVENTS"),
		NATSSubjectPrefix: envStr("SYSTERA_NATS_SUBJECT_PREFIX", "systera.events"),
		RedisAddr:         envStr("SYSTERA_REDIS_ADDR", "localhost:6379"),

		S3Endpoint:  envStr("SYSTERA_S3_ENDPOINT", "http://localhost:9000"),
		S3Region:    envStr("SYSTERA_S3_REGION", "us-east-1"),
		S3Bucket:    envStr("SYSTERA_S3_BUCKET", "systera-artifacts"),
		S3AccessKey: os.Getenv("SYSTERA_S3_ACCESS_KEY"),
		S3SecretKey: os.Getenv("SYSTERA_S3_SECRET_KEY"),
	}, nil
}

// ValidateModel reports whether a worker's model settings are usable: hosted providers
// (groq, openrouter, huggingface) need an API key.
func (c Config) ValidateModel() error {
	keyEnv := map[string]string{"groq": "GROQ_API_KEY", "openrouter": "OPENROUTER_API_KEY", "huggingface": "HF_TOKEN"}[c.ModelProvider]
	if keyEnv != "" && c.ModelAPIKey == "" {
		return fmt.Errorf("provider %s needs an API key: set %s or SYSTERA_MODEL_API_KEY", c.ModelProvider, keyEnv)
	}
	if c.ModelBaseURL == "" || c.ModelDefault == "" {
		return errors.New("SYSTERA_MODEL_BASE_URL and SYSTERA_MODEL_DEFAULT must be non-empty")
	}
	return nil
}

// ValidateAuth reports whether the authentication settings are usable. The API refuses
// to start without a verifiable identity configuration (RS256 via JWKS or RSA public key).
func (c Config) ValidateAuth() error {
	if c.AuthJWKSURL == "" && c.AuthPublicKeyPEM == "" && c.AuthPublicKeyFile == "" {
		return errors.New("one of SYSTERA_AUTH_JWKS_URL, SYSTERA_AUTH_PUBLIC_KEY_PEM, or SYSTERA_AUTH_PUBLIC_KEY_FILE must be set")
	}
	if c.AuthIssuer == "" || c.AuthAudience == "" {
		return errors.New("SYSTERA_AUTH_ISSUER and SYSTERA_AUTH_AUDIENCE must be non-empty")
	}
	return nil
}

// PostgresDSN returns a PostgreSQL connection URL with credentials safely escaped.
func (c Config) PostgresDSN() string {
	if c.DatabaseURL != "" {
		return c.DatabaseURL
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.PostgresUser, c.PostgresPassword),
		Host:     fmt.Sprintf("%s:%d", c.PostgresHost, c.PostgresPort),
		Path:     "/" + c.PostgresDB,
		RawQuery: url.Values{"sslmode": {c.PostgresSSLMode}}.Encode(),
	}
	return u.String()
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envList(key string, fallback []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	return strconv.Atoi(v)
}

func envBool(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	return strconv.ParseBool(v)
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	return time.ParseDuration(v)
}
