package telegraph

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the top-level TOML structure for telegraph.toml.
type Config struct {
	Telegraph TelegraphConfig `toml:"telegraph"`
}

// TelegraphConfig holds global Telegraph settings.
type TelegraphConfig struct {
	// ListenAddr is the TCP address for the HTTP webhook listener (e.g. ":8765").
	ListenAddr string `toml:"listen_addr"`

	// BufferSize is the maximum number of RawEvents queued between L1 and L2.
	// When the buffer is full, L1 returns HTTP 503 (backpressure rejection).
	BufferSize int `toml:"buffer_size"`

	// NudgeWindow is the minimum duration between Mayor nudges (e.g. "30s").
	// Telegraph sends at most one nudge per window regardless of event volume.
	NudgeWindow string `toml:"nudge_window"`

	// BodyCap is the maximum bytes of external text included in the mail body.
	// Content beyond the cap is truncated with a "[… truncated]" notice.
	BodyCap int `toml:"body_cap"`

	// PromptCap is the maximum bytes of a resolved operator prompt before
	// truncation. Default 2048. Truncation appends "\n[… prompt truncated]".
	PromptCap int `toml:"prompt_cap"`

	// LogFile is the path to Telegraph's log file. Empty means stderr.
	LogFile string `toml:"log_file"`

	// Prompts holds per-event-type operator prompt templates, keyed by
	// "<provider>:<event_type>" (e.g. "jira:comment.added") or "default".
	// If absent or empty, no OPERATOR PROMPT block is emitted.
	Prompts map[string]string `toml:"prompts"`

	// Providers is a map of provider ID → per-provider configuration.
	// Example provider IDs: "jira", "github".
	Providers map[string]*ProviderConfig `toml:"providers"`
}

// ProviderConfig holds per-provider Telegraph settings.
type ProviderConfig struct {
	// Enabled controls whether this provider accepts webhook events.
	// Set to false to stop delivery. Requires daemon restart in v1.
	Enabled bool `toml:"enabled"`

	// SecretEnv is the name of the environment variable holding the HMAC secret.
	// The value must never be committed or logged; only the env var name is stored here.
	SecretEnv string `toml:"secret_env"`

	// Events is the list of provider event types to accept.
	// Unrecognised event types are rejected with ErrUnknownEventType.
	Events []string `toml:"events"`

	// IgnoreActors is a list of actor display-names whose events are silently
	// dropped before L3 enqueue. Empty or absent means no actor filtering.
	// Strings are compared case-sensitively against NormalizedEvent.Actor.
	// An empty-string entry is rejected at config-load time.
	IgnoreActors []string `toml:"ignore_actors"`

	// Repos is an allow-list of repository identifiers whose events the
	// provider accepts. Empty or absent means no repository filtering (all
	// authenticated events from configured event types are accepted).
	//
	// Currently consumed by the GitHub provider only; entries are formatted
	// as "owner/repo" and matched case-insensitively against the webhook's
	// repository.full_name. Other providers ignore this field; rather than
	// reject it at validation time we let provider translators enforce
	// semantics so the option remains forward-compatible.
	//
	// An empty-string entry is rejected at config-load time.
	Repos []string `toml:"repos"`
}

// DefaultConfig returns a Config with sensible defaults.
// ListenAddr and per-provider SecretEnv must still be set explicitly.
func DefaultConfig() *Config {
	return &Config{
		Telegraph: TelegraphConfig{
			ListenAddr:  ":8765",
			BufferSize:  256,
			NudgeWindow: "30s",
			BodyCap:     4096,
			PromptCap:   2048,
			LogFile:     "",
			Providers:   map[string]*ProviderConfig{},
		},
	}
}

// DefaultPath returns the canonical path for telegraph.toml given the town root.
// Design spec: ~/gt/settings/telegraph.toml (town-level, single instance).
func DefaultPath(townRoot string) string {
	return filepath.Join(townRoot, "settings", "telegraph.toml")
}

// Load reads and parses a telegraph.toml file at path.
// Fields not present in the file retain their zero values; callers should apply
// defaults with DefaultConfig() if needed.
func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("telegraph config not found: %s", path)
		}
		return nil, fmt.Errorf("parsing telegraph config %s: %w", path, err)
	}
	return &cfg, nil
}

// LoadWithDefaults reads the config at path and fills unset fields with defaults.
func LoadWithDefaults(path string) (*Config, error) {
	cfg := DefaultConfig()
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("telegraph config not found: %s", path)
		}
		return nil, fmt.Errorf("parsing telegraph config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks required fields and structural constraints.
// Returns an error describing the first violation found.
func (c *Config) Validate() error {
	t := &c.Telegraph
	if t.ListenAddr == "" {
		return errors.New("telegraph.listen_addr is required")
	}
	if t.BufferSize <= 0 {
		return errors.New("telegraph.buffer_size must be positive")
	}
	if t.BodyCap <= 0 {
		return errors.New("telegraph.body_cap must be positive")
	}
	nw, err := time.ParseDuration(t.NudgeWindow)
	if err != nil {
		return fmt.Errorf("telegraph.nudge_window %q is not a valid duration: %w", t.NudgeWindow, err)
	}
	if nw < 0 {
		return fmt.Errorf("telegraph.nudge_window %q must be non-negative", t.NudgeWindow)
	}
	for id, p := range t.Providers {
		if p == nil {
			return fmt.Errorf("telegraph.providers.%s: nil config", id)
		}
		if p.Enabled && p.SecretEnv == "" {
			return fmt.Errorf("telegraph.providers.%s: secret_env is required when enabled=true", id)
		}
		if p.Enabled && len(p.Events) == 0 {
			return fmt.Errorf("telegraph.providers.%s: events list must be non-empty when enabled=true", id)
		}
		for _, a := range p.IgnoreActors {
			if a == "" {
				return fmt.Errorf("telegraph.providers.%s: ignore_actors must not contain empty strings", id)
			}
		}
		for _, r := range p.Repos {
			if r == "" {
				return fmt.Errorf("telegraph.providers.%s: repos must not contain empty strings", id)
			}
		}
	}
	return nil
}

// ParsedNudgeWindow returns the NudgeWindow parsed as a time.Duration.
// Returns an error if the value is invalid; call Validate() first to surface this earlier.
func (t *TelegraphConfig) ParsedNudgeWindow() (time.Duration, error) {
	return time.ParseDuration(t.NudgeWindow)
}

// ResolveSecret reads the provider's HMAC secret from the environment variable
// named by SecretEnv. Returns an error if the env var is unset or empty.
// The returned secret lives in memory only and must never be logged or written to disk.
func (p *ProviderConfig) ResolveSecret() (string, error) {
	if p.SecretEnv == "" {
		return "", errors.New("secret_env is not configured")
	}
	secret := os.Getenv(p.SecretEnv)
	if secret == "" {
		return "", fmt.Errorf("env var %s is unset or empty", p.SecretEnv)
	}
	return secret, nil
}

// ResolvedProvider bundles a ProviderConfig with its resolved secret.
// The Secret field must never be logged or written to disk.
type ResolvedProvider struct {
	Config *ProviderConfig
	Secret string // resolved from Config.SecretEnv at startup
}

// String redacts Secret so ResolvedProvider is safe to pass to log/fmt.
func (r *ResolvedProvider) String() string {
	return fmt.Sprintf("ResolvedProvider{Config:%v Secret:[REDACTED]}", r.Config)
}

// GoString redacts Secret in %#v output.
func (r *ResolvedProvider) GoString() string {
	return fmt.Sprintf("&telegraph.ResolvedProvider{Config:%#v, Secret:\"[REDACTED]\"}", r.Config)
}

// ResolveProviders resolves secrets for all enabled providers.
// Returns an error immediately if any enabled provider's secret env var is unset.
// Disabled providers are skipped — their secrets are not required.
func (c *Config) ResolveProviders() (map[string]*ResolvedProvider, error) {
	out := make(map[string]*ResolvedProvider, len(c.Telegraph.Providers))
	for id, p := range c.Telegraph.Providers {
		if p == nil || !p.Enabled {
			continue
		}
		secret, err := p.ResolveSecret()
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", id, err)
		}
		out[id] = &ResolvedProvider{Config: p, Secret: secret}
	}
	return out, nil
}
