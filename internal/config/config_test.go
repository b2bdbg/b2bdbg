package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/b2bdbg/b2bdbg/internal/config"
)

func TestLoadAndValidate(t *testing.T) {
	// yamlFile writes content to a temp file and returns its path.
	yamlFile := func(t *testing.T, content string) string {
		t.Helper()
		f, err := os.CreateTemp(t.TempDir(), "cfg-*.yaml")
		if err != nil {
			t.Fatalf("create temp file: %v", err)
		}
		if _, err := f.WriteString(content); err != nil {
			t.Fatalf("write temp file: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close temp file: %v", err)
		}
		return f.Name()
	}

	tests := []struct {
		name        string
		yaml        string
		env         map[string]string
		overrides   config.Overrides
		wantErr     bool // Load should fail
		validateErr bool // Validate should fail (Load OK but config invalid)
		check       func(t *testing.T, cfg config.Config)
	}{
		{
			name: "defaults when no yaml",
			check: func(t *testing.T, cfg config.Config) {
				if cfg.ListenAddr != ":8080" {
					t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
				}
				if cfg.TelegramBaseURL != "https://api.telegram.org" {
					t.Errorf("TelegramBaseURL = %q, want https://api.telegram.org", cfg.TelegramBaseURL)
				}
				if cfg.LogLevel != "info" {
					t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
				}
				if cfg.ShutdownTimeout != 15*time.Second {
					t.Errorf("ShutdownTimeout = %v, want 15s", cfg.ShutdownTimeout)
				}
			},
		},
		{
			name: "yaml overrides defaults",
			yaml: `
listen_addr: ":9090"
log_level: "debug"
shutdown_timeout: 30s
telegram_base_url: "https://api.telegram.org"
`,
			check: func(t *testing.T, cfg config.Config) {
				if cfg.ListenAddr != ":9090" {
					t.Errorf("ListenAddr = %q, want :9090", cfg.ListenAddr)
				}
				if cfg.LogLevel != "debug" {
					t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
				}
				if cfg.ShutdownTimeout != 30*time.Second {
					t.Errorf("ShutdownTimeout = %v, want 30s", cfg.ShutdownTimeout)
				}
			},
		},
		{
			name: "env vars override yaml",
			yaml: `
listen_addr: ":9090"
telegram_base_url: "https://api.telegram.org"
`,
			env: map[string]string{
				"B2BD_LISTEN_ADDR": ":7777",
				"B2BD_LOG_LEVEL":   "warn",
			},
			check: func(t *testing.T, cfg config.Config) {
				if cfg.ListenAddr != ":7777" {
					t.Errorf("ListenAddr = %q, want :7777", cfg.ListenAddr)
				}
				if cfg.LogLevel != "warn" {
					t.Errorf("LogLevel = %q, want warn", cfg.LogLevel)
				}
			},
		},
		{
			name: "flag overrides beat env and yaml",
			yaml: `
listen_addr: ":9090"
telegram_base_url: "https://api.telegram.org"
`,
			env: map[string]string{
				"B2BD_LISTEN_ADDR": ":7777",
			},
			overrides: config.Overrides{
				ListenAddr: ":5555",
			},
			check: func(t *testing.T, cfg config.Config) {
				if cfg.ListenAddr != ":5555" {
					t.Errorf("ListenAddr = %q, want :5555", cfg.ListenAddr)
				}
			},
		},
		{
			name:    "bad yaml returns error",
			yaml:    `{invalid yaml:::`,
			wantErr: true,
		},
		{
			name: "unknown yaml field returns error",
			yaml: `unknown_field: true
telegram_base_url: "https://api.telegram.org"
`,
			wantErr: true,
		},
		{
			name: "validate rejects bad log level",
			yaml: `
telegram_base_url: "https://api.telegram.org"
log_level: "verbose"
`,
			validateErr: true,
		},
		{
			name: "validate rejects non-http base url",
			yaml: `log_level: info`,
			overrides: config.Overrides{
				TelegramBaseURL: "ftp://bad.example.com",
			},
			validateErr: true,
		},
		{
			name: "otel endpoint optional",
			check: func(t *testing.T, cfg config.Config) {
				// OTelEndpoint is empty string by default — that is valid.
				if err := config.Validate(cfg); err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
			},
		},
		{
			name: "env shutdown_timeout parsed",
			env: map[string]string{
				"B2BD_SHUTDOWN_TIMEOUT": "5s",
			},
			check: func(t *testing.T, cfg config.Config) {
				if cfg.ShutdownTimeout != 5*time.Second {
					t.Errorf("ShutdownTimeout = %v, want 5s", cfg.ShutdownTimeout)
				}
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Sub-tests that call t.Setenv cannot run in parallel.
			if len(tc.env) == 0 {
				t.Parallel()
			}

			// Set env vars scoped to this sub-test.
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			// Write YAML to a temp file if provided.
			var path string
			if tc.yaml != "" {
				path = yamlFile(t, tc.yaml)
			}

			cfg, err := config.Load(path, tc.overrides)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Load() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}

			if tc.validateErr {
				if err := config.Validate(cfg); err == nil {
					t.Fatal("Validate() expected error, got nil")
				}
				return
			}

			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}

	// Ensure the example config file loads and validates successfully.
	t.Run("testdata fixture", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join("..", "..", "config.example.yaml")
		cfg, err := config.Load(path, config.Overrides{})
		if err != nil {
			t.Fatalf("Load(config.example.yaml) error: %v", err)
		}
		if err := config.Validate(cfg); err != nil {
			t.Fatalf("Validate(config.example.yaml) error: %v", err)
		}
	})
}

// TestWebhookRoutesConfig covers webhook_routes parsing, Validate() rules
// (unique labels, label charset, non-empty token/target, scheme), and the
// per-route B2BD_WEBHOOK_TOKEN_<LABEL> / B2BD_WEBHOOK_SECRET_<LABEL> env
// overrides.
func TestWebhookRoutesConfig(t *testing.T) {
	yamlFile := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "cfg.yaml")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write yaml: %v", err)
		}
		return p
	}

	t.Run("valid routes load and validate", func(t *testing.T) {
		t.Parallel()
		path := yamlFile(t, `
telegram_base_url: "https://api.telegram.org"
webhook_routes:
  - label: "router-bot"
    token: "111:AAA"
    target: "http://localhost:4000"
    secret_token: "s1"
  - label: "sales-bot"
    token: "222:BBB"
    target: "https://localhost:4001"
`)
		cfg, err := config.Load(path, config.Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := config.Validate(cfg); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if len(cfg.WebhookRoutes) != 2 {
			t.Fatalf("got %d routes, want 2", len(cfg.WebhookRoutes))
		}
		if cfg.WebhookRoutes[0].Label != "router-bot" || cfg.WebhookRoutes[0].Target != "http://localhost:4000" {
			t.Errorf("route[0] = %+v", cfg.WebhookRoutes[0])
		}
		if cfg.WebhookRoutes[0].SecretToken != "s1" {
			t.Errorf("route[0] secret = %q, want s1", cfg.WebhookRoutes[0].SecretToken)
		}
	})

	invalidCases := []struct {
		name string
		yaml string
	}{
		{"duplicate labels", `
telegram_base_url: "https://api.telegram.org"
webhook_routes:
  - label: "dup"
    token: "1:A"
    target: "http://a"
  - label: "dup"
    token: "2:B"
    target: "http://b"
`},
		{"empty token", `
telegram_base_url: "https://api.telegram.org"
webhook_routes:
  - label: "x"
    token: ""
    target: "http://a"
`},
		{"empty target", `
telegram_base_url: "https://api.telegram.org"
webhook_routes:
  - label: "x"
    token: "1:A"
    target: ""
`},
		{"bad target scheme", `
telegram_base_url: "https://api.telegram.org"
webhook_routes:
  - label: "x"
    token: "1:A"
    target: "ftp://a"
`},
		{"bad label charset", `
telegram_base_url: "https://api.telegram.org"
webhook_routes:
  - label: "bad label!"
    token: "1:A"
    target: "http://a"
`},
		{"empty label", `
telegram_base_url: "https://api.telegram.org"
webhook_routes:
  - label: ""
    token: "1:A"
    target: "http://a"
`},
	}
	for _, tc := range invalidCases {
		tc := tc
		t.Run("invalid: "+tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(yamlFile(t, tc.yaml), config.Overrides{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := config.Validate(cfg); err == nil {
				t.Fatal("Validate() expected error, got nil")
			}
		})
	}

	t.Run("env overrides token and secret per route", func(t *testing.T) {
		path := yamlFile(t, `
telegram_base_url: "https://api.telegram.org"
webhook_routes:
  - label: "router-bot"
    token: "yaml-token"
    target: "http://localhost:4000"
    secret_token: "yaml-secret"
`)
		t.Setenv("B2BD_WEBHOOK_TOKEN_ROUTER_BOT", "env-token")
		t.Setenv("B2BD_WEBHOOK_SECRET_ROUTER_BOT", "env-secret")

		cfg, err := config.Load(path, config.Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.WebhookRoutes[0].Token != "env-token" {
			t.Errorf("Token = %q, want env-token (env overrides yaml)", cfg.WebhookRoutes[0].Token)
		}
		if cfg.WebhookRoutes[0].SecretToken != "env-secret" {
			t.Errorf("SecretToken = %q, want env-secret", cfg.WebhookRoutes[0].SecretToken)
		}
		if err := config.Validate(cfg); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("env token satisfies validation when yaml token empty", func(t *testing.T) {
		path := yamlFile(t, `
telegram_base_url: "https://api.telegram.org"
webhook_routes:
  - label: "envonly"
    token: ""
    target: "http://localhost:4000"
`)
		t.Setenv("B2BD_WEBHOOK_TOKEN_ENVONLY", "from-env")
		cfg, err := config.Load(path, config.Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := config.Validate(cfg); err != nil {
			t.Fatalf("Validate: %v (env token should satisfy non-empty rule)", err)
		}
	})
}

// TestDebugEndpointsConfig verifies the opt-in debug endpoints flag: it
// defaults OFF and is enabled via B2BD_DEBUG_ENDPOINTS.
func TestDebugEndpointsConfig(t *testing.T) {
	t.Run("default off", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Load("", config.Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DebugEndpoints {
			t.Error("DebugEndpoints = true by default, want false")
		}
	})

	t.Run("enabled via env", func(t *testing.T) {
		t.Setenv("B2BD_DEBUG_ENDPOINTS", "true")
		cfg, err := config.Load("", config.Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.DebugEndpoints {
			t.Error("DebugEndpoints = false, want true when B2BD_DEBUG_ENDPOINTS=true")
		}
	})

	t.Run("explicit false via env", func(t *testing.T) {
		t.Setenv("B2BD_DEBUG_ENDPOINTS", "false")
		cfg, err := config.Load("", config.Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DebugEndpoints {
			t.Error("DebugEndpoints = true, want false when B2BD_DEBUG_ENDPOINTS=false")
		}
	})
}

// TestBodyCapBytesConfig verifies body_cap_bytes: defaults to 1 MiB, is
// settable via YAML and B2BD_BODY_CAP_BYTES (env > yaml), and is rejected by
// Validate when 0/negative/below the sane minimum.
func TestBodyCapBytesConfig(t *testing.T) {
	yamlFile := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "cfg.yaml")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write temp yaml: %v", err)
		}
		return p
	}

	t.Run("default is 1 MiB", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Load("", config.Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.BodyCapBytes != 1<<20 {
			t.Errorf("BodyCapBytes = %d, want %d (1 MiB default)", cfg.BodyCapBytes, 1<<20)
		}
		if err := config.Validate(cfg); err != nil {
			t.Fatalf("Validate(default): %v", err)
		}
	})

	t.Run("yaml custom value", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Load(yamlFile(t, "telegram_base_url: \"https://api.telegram.org\"\nbody_cap_bytes: 65536\n"), config.Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.BodyCapBytes != 65536 {
			t.Errorf("BodyCapBytes = %d, want 65536 (from yaml)", cfg.BodyCapBytes)
		}
		if err := config.Validate(cfg); err != nil {
			t.Fatalf("Validate(custom): %v", err)
		}
	})

	t.Run("env overrides yaml", func(t *testing.T) {
		t.Setenv("B2BD_BODY_CAP_BYTES", "131072")
		cfg, err := config.Load(yamlFile(t, "telegram_base_url: \"https://api.telegram.org\"\nbody_cap_bytes: 65536\n"), config.Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.BodyCapBytes != 131072 {
			t.Errorf("BodyCapBytes = %d, want 131072 (env must override yaml)", cfg.BodyCapBytes)
		}
	})

	invalid := []struct {
		name string
		val  int64
	}{
		{"zero", 0},
		{"negative", -1},
		{"below minimum", config.MinBodyCapBytes - 1},
	}
	for _, tc := range invalid {
		tc := tc
		t.Run("invalid: "+tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load("", config.Overrides{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			cfg.BodyCapBytes = tc.val
			if err := config.Validate(cfg); err == nil {
				t.Fatalf("Validate(body_cap_bytes=%d) = nil, want error", tc.val)
			}
		})
	}
}

// TestApplyEnvInvalidValuesFail verifies that a set-but-unparseable typed
// B2BD_* env var causes Load to fail loudly instead of silently falling back
// to the default. Production deploys must not run with a typo masked.
//
// Not t.Parallel(): t.Setenv (used by subtests) is incompatible with parallel
// tests because it mutates process-wide environment state.
func TestApplyEnvInvalidValuesFail(t *testing.T) {
	cases := []struct {
		name string
		env  string
		val  string
	}{
		{"shutdown-timeout-bad", "B2BD_SHUTDOWN_TIMEOUT", "not-a-duration"},
		{"cost-per-k-tokens-bad", "B2BD_COST_PER_K_TOKENS", "free"},
		{"body-cap-bytes-bad", "B2BD_BODY_CAP_BYTES", "lots"},
		{"debug-endpoints-bad", "B2BD_DEBUG_ENDPOINTS", "kinda"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			if _, err := config.Load("", config.Overrides{}); err == nil {
				t.Fatalf("Load with %s=%q returned nil error; want a parse error", tc.env, tc.val)
			}
		})
	}
}
