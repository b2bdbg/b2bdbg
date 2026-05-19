// Package config provides typed configuration loading for b2bdbg.
//
// Precedence (highest to lowest): CLI flags > environment variables > YAML file.
// All fields are also settable via B2BD_* environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/b2bdbg/b2bdbg/internal/proxy"
)

// labelRE validates webhook route labels: URL-safe, non-empty, alphanumeric
// plus hyphens and underscores.
var labelRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// MinBodyCapBytes is the smallest accepted value for body_cap_bytes. A cap
// below this would truncate essentially every Bot API body, making capture
// useless, so [Validate] rejects 0, negative, and absurdly small values with a
// clear error rather than silently degrading.
const MinBodyCapBytes = 1024 // 1 KiB

// WebhookRoute describes a single inbound webhook endpoint registered by b2bdbg.
//
// When Telegram (or any caller) POSTs an update to /webhook/<Label>, b2bdbg:
//  1. Optionally validates the X-Telegram-Bot-Api-Secret-Token header.
//  2. Captures the exchange with the correct TokenHash derived from Token.
//  3. Forwards the update body to Target.
//
// Token holds the operator-supplied raw bot token for the lifetime of the
// in-memory Config (it originates from YAML or a B2BD_WEBHOOK_TOKEN_<LABEL>
// env var the operator controls, like any service credential). b2bdbg derives
// a SHA-256 token-hash for telemetry/logs and never logs, persists, or emits
// the raw value, and webhook paths carry only the URL-safe label — but the
// raw token is not zeroed or discarded after load; it remains in the process
// Config in memory.
type WebhookRoute struct {
	// Label is the URL-safe name for this route. It appears in the path as
	// /webhook/<label>. Must match [A-Za-z0-9_-]+. Required.
	Label string `yaml:"label"`

	// Token is the Telegram bot token for this route. It is used only to derive
	// the TokenHash; it is never logged or stored after hashing.
	//
	// Env override (12-factor; keeps the secret out of the YAML file):
	// B2BD_WEBHOOK_TOKEN_<LABEL> where <LABEL> is the upper-cased label with
	// '-' replaced by '_'. The env value, when set, takes precedence over the
	// YAML token for that route.
	Token string `yaml:"token"`

	// Target is the full URL of the bot server that should receive the forwarded
	// Telegram update body. Required. Must be an http:// or https:// URL.
	Target string `yaml:"target"`

	// SecretToken is the optional value of the X-Telegram-Bot-Api-Secret-Token
	// header that Telegram sends with webhook deliveries. When non-empty, b2bdbg
	// rejects requests whose header does not match. Set via setWebhook's
	// secret_token parameter.
	//
	// Env override: B2BD_WEBHOOK_SECRET_<LABEL> (same label normalisation as
	// the token override). The env value, when set, takes precedence.
	SecretToken string `yaml:"secret_token"`
}

// Config holds the complete runtime configuration for b2bdbg.
type Config struct {
	// ListenAddr is the TCP address the admin/proxy server binds to.
	// Default: ":8080"
	ListenAddr string `yaml:"listen_addr"`

	// TelegramBaseURL is the upstream Telegram Bot API base URL.
	// Default: https://api.telegram.org
	TelegramBaseURL string `yaml:"telegram_base_url"`

	// OTelEndpoint is the OTLP gRPC endpoint for OpenTelemetry traces.
	// Example: localhost:4317
	OTelEndpoint string `yaml:"otel_endpoint"`

	// LogLevel is the minimum log level: debug, info, warn, error.
	// Default: info
	LogLevel string `yaml:"log_level"`

	// ShutdownTimeout is how long graceful shutdown waits for in-flight
	// requests before forcibly closing.
	// Default: 15s
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`

	// CostPerKTokens is the estimated cost in USD per 1 000 tokens, used to
	// populate the b2b_token_cost_usd metric and b2b.cost.usd.est span
	// attribute. A value of 0 (the default) disables cost accumulation.
	// Env: B2BD_COST_PER_K_TOKENS
	CostPerKTokens float64 `yaml:"cost_per_k_tokens"`

	// BodyCapBytes is the maximum number of request/response body bytes that are
	// buffered for capture/telemetry per exchange. The full body is always still
	// forwarded transparently; this only bounds what is parsed for span
	// attributes and loop detection. When a body exceeds this cap the
	// b2b.capture.truncated span attribute is set true.
	// Default: proxy.DefaultBodyCapBytes (1 MiB). Must be > 0.
	// Env: B2BD_BODY_CAP_BYTES (bytes, integer).
	BodyCapBytes int64 `yaml:"body_cap_bytes"`

	// DebugEndpoints, when true, enables local-only introspection endpoints
	// (currently GET /debug/registry, which lists the bot id↔token-hash
	// mappings learned from getMe so operators can see why telegram.bot.to is
	// empty). It is OFF by default; when disabled the route is not registered
	// and returns 404 with zero overhead. Raw bot tokens are NEVER exposed —
	// only the same hashes already written to spans, plus counts.
	// Env: B2BD_DEBUG_ENDPOINTS (true/false).
	DebugEndpoints bool `yaml:"debug_endpoints"`

	// WebhookRoutes is the list of inbound Telegram webhook endpoints.
	// Each entry registers /webhook/<label> on the server mux and captures
	// updates with the full token-hash / bot-registry / loop-detection
	// pipeline, identical to long-polling capture.
	// No environment variable override — configure via YAML.
	WebhookRoutes []WebhookRoute `yaml:"webhook_routes"`
}

// defaults returns a Config pre-filled with default values.
func defaults() Config {
	return Config{
		ListenAddr:      ":8080",
		TelegramBaseURL: "https://api.telegram.org",
		LogLevel:        "info",
		ShutdownTimeout: 15 * time.Second,
		BodyCapBytes:    proxy.DefaultBodyCapBytes,
	}
}

// Overrides holds values injected from CLI flags (supersede everything else).
type Overrides struct {
	ListenAddr      string
	TelegramBaseURL string
	OTelEndpoint    string
	LogLevel        string
}

// Load builds a Config by merging (in order): defaults, YAML file at path,
// environment variables, then the explicit flag overrides.
// If path is empty the YAML step is skipped gracefully.
func Load(path string, overrides Overrides) (Config, error) {
	cfg := defaults()

	if path != "" {
		if err := loadYAML(path, &cfg); err != nil {
			return Config{}, fmt.Errorf("config: load yaml %q: %w", path, err)
		}
	}

	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	applyOverrides(&cfg, overrides)

	return cfg, nil
}

// Validate checks the Config for logical consistency and returns an error
// describing all problems found (joined by "; ").
func Validate(cfg Config) error {
	var errs []string

	if cfg.ListenAddr == "" {
		errs = append(errs, "listen_addr must not be empty")
	}
	if cfg.TelegramBaseURL == "" {
		errs = append(errs, "telegram_base_url must not be empty")
	}
	if !strings.HasPrefix(cfg.TelegramBaseURL, "http://") &&
		!strings.HasPrefix(cfg.TelegramBaseURL, "https://") {
		errs = append(errs, "telegram_base_url must start with http:// or https://")
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
		// valid
	default:
		errs = append(errs, fmt.Sprintf("log_level %q must be one of: debug, info, warn, error", cfg.LogLevel))
	}
	if cfg.ShutdownTimeout <= 0 {
		errs = append(errs, "shutdown_timeout must be positive")
	}
	if cfg.BodyCapBytes < MinBodyCapBytes {
		errs = append(errs, fmt.Sprintf(
			"body_cap_bytes (%d) must be >= %d bytes (a too-small cap would truncate every body)",
			cfg.BodyCapBytes, MinBodyCapBytes))
	}

	// Validate webhook routes.
	labels := make(map[string]struct{}, len(cfg.WebhookRoutes))
	for i, r := range cfg.WebhookRoutes {
		prefix := fmt.Sprintf("webhook_routes[%d]", i)

		if r.Label == "" {
			errs = append(errs, prefix+": label must not be empty")
		} else if !labelRE.MatchString(r.Label) {
			errs = append(errs, fmt.Sprintf("%s: label %q must match [A-Za-z0-9_-]+", prefix, r.Label))
		} else {
			if _, dup := labels[r.Label]; dup {
				errs = append(errs, fmt.Sprintf("%s: label %q is not unique", prefix, r.Label))
			}
			labels[r.Label] = struct{}{}
		}

		if r.Token == "" {
			errs = append(errs, prefix+": token must not be empty")
		}

		if r.Target == "" {
			errs = append(errs, prefix+": target must not be empty")
		} else if !strings.HasPrefix(r.Target, "http://") && !strings.HasPrefix(r.Target, "https://") {
			errs = append(errs, fmt.Sprintf("%s: target %q must start with http:// or https://", prefix, r.Target))
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// loadYAML reads path and unmarshals it into cfg.
func loadYAML(path string, cfg *Config) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
	}()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return err
	}
	return nil
}

// applyEnv reads B2BD_* environment variables and overwrites cfg fields when
// the variable is non-empty.
//
// Typed variables (duration / float / int / bool) are validated: a set but
// unparseable value is a hard error rather than a silent fall-back to the
// default, so a production typo fails fast instead of running with surprising
// behaviour. String variables cannot fail to parse.
func applyEnv(cfg *Config) error {
	if v := os.Getenv("B2BD_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("B2BD_TELEGRAM_BASE_URL"); v != "" {
		cfg.TelegramBaseURL = v
	}
	if v := os.Getenv("B2BD_OTEL_ENDPOINT"); v != "" {
		cfg.OTelEndpoint = v
	}
	if v := os.Getenv("B2BD_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("B2BD_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("config: B2BD_SHUTDOWN_TIMEOUT %q: %w", v, err)
		}
		cfg.ShutdownTimeout = d
	}
	if v := os.Getenv("B2BD_COST_PER_K_TOKENS"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("config: B2BD_COST_PER_K_TOKENS %q: %w", v, err)
		}
		cfg.CostPerKTokens = f
	}
	if v := os.Getenv("B2BD_BODY_CAP_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("config: B2BD_BODY_CAP_BYTES %q: %w", v, err)
		}
		cfg.BodyCapBytes = n
	}
	if v := os.Getenv("B2BD_DEBUG_ENDPOINTS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: B2BD_DEBUG_ENDPOINTS %q: %w", v, err)
		}
		cfg.DebugEndpoints = b
	}

	// Per-route secret overrides: B2BD_WEBHOOK_TOKEN_<LABEL> and
	// B2BD_WEBHOOK_SECRET_<LABEL>. <LABEL> = upper-cased label, '-' → '_'.
	// These let operators keep bot tokens out of the YAML file (12-factor)
	// while routes/targets stay declarative.
	for i := range cfg.WebhookRoutes {
		key := envLabelKey(cfg.WebhookRoutes[i].Label)
		if key == "" {
			continue
		}
		if v := os.Getenv("B2BD_WEBHOOK_TOKEN_" + key); v != "" {
			cfg.WebhookRoutes[i].Token = v
		}
		if v := os.Getenv("B2BD_WEBHOOK_SECRET_" + key); v != "" {
			cfg.WebhookRoutes[i].SecretToken = v
		}
	}
	return nil
}

// envLabelKey normalises a webhook route label into the env-var suffix used by
// the per-route overrides: upper-cased, with '-' replaced by '_'. Labels are
// already constrained to [A-Za-z0-9_-]+ by Validate, so this yields a valid
// env-var name fragment. An empty label returns "".
func envLabelKey(label string) string {
	if label == "" {
		return ""
	}
	return strings.ToUpper(strings.ReplaceAll(label, "-", "_"))
}

// applyOverrides writes non-zero flag values on top of cfg.
func applyOverrides(cfg *Config, o Overrides) {
	if o.ListenAddr != "" {
		cfg.ListenAddr = o.ListenAddr
	}
	if o.TelegramBaseURL != "" {
		cfg.TelegramBaseURL = o.TelegramBaseURL
	}
	if o.OTelEndpoint != "" {
		cfg.OTelEndpoint = o.OTelEndpoint
	}
	if o.LogLevel != "" {
		cfg.LogLevel = o.LogLevel
	}
}
