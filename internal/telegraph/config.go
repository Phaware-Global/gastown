package telegraph

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the top-level Telegraph configuration, loaded from
// ~/gt/settings/telegraph.toml at daemon startup.
//
// Schema:
//
//	[telegraph]
//	listen_addr  = ":8765"
//	buffer_size  = 256
//	nudge_window = "30s"
//	body_cap     = 4096
//	log_file     = ""
//
//	[telegraph.providers.jira]
//	enabled    = true
//	secret_env = "GT_TELEGRAPH_JIRA_SECRET"
//	events     = ["issue_created", "issue_updated", "comment_added", "comment_updated"]
type Config struct {
	Telegraph TelegraphConfig `toml:"telegraph"`
}

// TelegraphConfig holds the [telegraph] block settings.
type TelegraphConfig struct {
	// ListenAddr is the TCP address for the HTTP listener (e.g. ":8765").
	ListenAddr string `toml:"listen_addr"`

	// BufferSize is the capacity of the RawEvent channel between L1 and L2.
	// When full, L1 returns HTTP 503 (backpressure). Default: 256.
	BufferSize int `toml:"buffer_size"`

	// NudgeWindow is the minimum interval between Mayor nudges.
	// Parsed as a Go duration string (e.g. "30s", "1m"). Default: 30s.
	NudgeWindow string `toml:"nudge_window"`

	// BodyCap is the maximum bytes of external text included in the mail body.
	// Content beyond this limit is truncated with a notice. Default: 4096.
	BodyCap int `toml:"body_cap"`

	// LogFile is the path for structured JSON log output.
	// Empty string means stderr / daemon log.
	LogFile string `toml:"log_file"`

	// Providers maps provider IDs to per-provider configuration.
	// Keys are stable provider identifiers matching Translator.Provider() ("jira", "github", …).
	Providers map[string]*ProviderConfig `toml:"providers"`
}

// ProviderConfig holds per-provider settings under [telegraph.providers.<name>].
type ProviderConfig struct {
	// Enabled controls whether this provider accepts events.
	// Set to false to stop delivery without removing the config stanza.
	// Requires a daemon restart to take effect (v1).
	Enabled bool `toml:"enabled"`

	// SecretEnv is the name of the environment variable that holds the
	// HMAC shared secret or API token for this provider.
	// The value is never stored in config — only the variable name.
	SecretEnv string `toml:"secret_env"`

	// Events lists the provider-native event type strings to accept.
	// Unknown event types are always rejected regardless of this list.
	Events []string `toml:"events"`
}

// defaults contains compile-time defaults applied when fields are zero-valued.
var defaults = TelegraphConfig{
	ListenAddr:  ":8765",
	BufferSize:  256,
	NudgeWindow: "30s",
	BodyCap:     4096,
}

// LoadConfig reads and parses the TOML config file at path, applies defaults for
// any missing fields, then resolves provider secrets from environment variables.
// Returns an error if the file cannot be read, parsed, or if a secret env var
// referenced by an enabled provider is unset.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is from trusted settings directory
	if err != nil {
		return nil, fmt.Errorf("telegraph: reading config %s: %w", path, err)
	}

	var cfg Config
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, fmt.Errorf("telegraph: parsing config %s: %w", path, err)
	}

	applyDefaults(&cfg.Telegraph)

	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("telegraph: invalid config: %w", err)
	}

	return &cfg, nil
}

// DefaultConfigPath returns the canonical path to the Telegraph config file.
func DefaultConfigPath(townRoot string) string {
	return filepath.Join(townRoot, "settings", "telegraph.toml")
}

// ResolvedSecrets holds the in-memory resolved secrets for all enabled providers.
// Secrets are never written to disk, logs, or error messages.
// Call ResolveSecrets after LoadConfig to populate this.
type ResolvedSecrets struct {
	// Providers maps provider ID to its resolved secret value.
	Providers map[string]string
}

// ResolveSecrets reads the secret env var for each enabled provider and returns
// a ResolvedSecrets map. Fails fast if any enabled provider's env var is unset or empty.
// The resolved values live in memory only — they are never logged.
func ResolveSecrets(cfg *Config) (*ResolvedSecrets, error) {
	out := &ResolvedSecrets{Providers: make(map[string]string)}
	for id, p := range cfg.Telegraph.Providers {
		if !p.Enabled {
			continue
		}
		if p.SecretEnv == "" {
			return nil, fmt.Errorf("telegraph: provider %q is enabled but secret_env is not set", id)
		}
		val := os.Getenv(p.SecretEnv)
		if val == "" {
			return nil, fmt.Errorf("telegraph: provider %q requires env var %s (unset or empty)", id, p.SecretEnv)
		}
		out.Providers[id] = val
	}
	return out, nil
}

// NudgeWindowDuration parses cfg.Telegraph.NudgeWindow as a time.Duration.
func NudgeWindowDuration(cfg *Config) (time.Duration, error) {
	d, err := time.ParseDuration(cfg.Telegraph.NudgeWindow)
	if err != nil {
		return 0, fmt.Errorf("telegraph: invalid nudge_window %q: %w", cfg.Telegraph.NudgeWindow, err)
	}
	return d, nil
}

func applyDefaults(t *TelegraphConfig) {
	if t.ListenAddr == "" {
		t.ListenAddr = defaults.ListenAddr
	}
	if t.BufferSize <= 0 {
		t.BufferSize = defaults.BufferSize
	}
	if t.NudgeWindow == "" {
		t.NudgeWindow = defaults.NudgeWindow
	}
	if t.BodyCap <= 0 {
		t.BodyCap = defaults.BodyCap
	}
}

func validateConfig(cfg *Config) error {
	if cfg.Telegraph.ListenAddr == "" {
		return fmt.Errorf("listen_addr is required")
	}
	if _, err := time.ParseDuration(cfg.Telegraph.NudgeWindow); err != nil {
		return fmt.Errorf("nudge_window %q is not a valid duration: %w", cfg.Telegraph.NudgeWindow, err)
	}
	for id, p := range cfg.Telegraph.Providers {
		if p == nil {
			return fmt.Errorf("provider %q config is nil", id)
		}
		if p.Enabled && p.SecretEnv == "" {
			return fmt.Errorf("provider %q is enabled but secret_env is not set", id)
		}
	}
	return nil
}
