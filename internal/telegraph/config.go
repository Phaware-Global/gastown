package telegraph

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	// DefaultListenAddr is the default bind address for the HTTP listener.
	DefaultListenAddr = ":8765"
	// DefaultBufferSize is the default bounded channel capacity between L1 and L2.
	DefaultBufferSize = 256
	// DefaultNudgeWindow is the minimum interval between Mayor nudges.
	DefaultNudgeWindow = 30 * time.Second
	// DefaultBodyCap is the default maximum bytes of external text in a mail body.
	DefaultBodyCap = 4096

	// ConfigFile is the relative path under the town root for the config file.
	ConfigFile = "settings/telegraph.toml"
)

// Config holds the full telegraph configuration loaded from telegraph.toml.
type Config struct {
	Telegraph TelegraphConfig `toml:"telegraph"`
}

// TelegraphConfig is the [telegraph] stanza.
type TelegraphConfig struct {
	// Enabled controls whether Telegraph starts. Defaults to false.
	Enabled bool `toml:"enabled"`

	// ListenAddr is the bind address for the HTTP listener (e.g. ":8765").
	ListenAddr string `toml:"listen_addr"`

	// BufferSize is the maximum number of RawEvents queued between L1 and L2.
	// When full, L1 returns HTTP 503 (backpressure).
	BufferSize int `toml:"buffer_size"`

	// NudgeWindowStr is the minimum interval between Mayor nudges (e.g. "30s").
	NudgeWindowStr string `toml:"nudge_window"`

	// BodyCap is the maximum bytes of external text included in a mail body.
	BodyCap int `toml:"body_cap"`

	// LogFile is the path for structured log output. Empty means stderr.
	LogFile string `toml:"log_file"`

	// Providers maps provider name → ProviderConfig.
	Providers map[string]ProviderConfig `toml:"providers"`
}

// ProviderConfig is the per-provider stanza under [telegraph.providers.<name>].
type ProviderConfig struct {
	// Enabled controls whether this provider accepts webhooks.
	Enabled bool `toml:"enabled"`

	// SecretEnv is the name of the environment variable holding the HMAC secret.
	// The secret itself is never stored in config.
	SecretEnv string `toml:"secret_env"`

	// Events lists the event type strings this provider should process.
	Events []string `toml:"events"`
}

// NudgeWindow returns the parsed nudge window duration, falling back to the default.
func (c *TelegraphConfig) NudgeWindow() time.Duration {
	if c.NudgeWindowStr != "" {
		if d, err := time.ParseDuration(c.NudgeWindowStr); err == nil && d > 0 {
			return d
		}
	}
	return DefaultNudgeWindow
}

// effective returns a copy of c with zero fields replaced by defaults.
func (c TelegraphConfig) effective() TelegraphConfig {
	if c.ListenAddr == "" {
		c.ListenAddr = DefaultListenAddr
	}
	if c.BufferSize <= 0 {
		c.BufferSize = DefaultBufferSize
	}
	if c.BodyCap <= 0 {
		c.BodyCap = DefaultBodyCap
	}
	return c
}

// ResolveSecret returns the HMAC secret for a provider by reading its SecretEnv.
// Returns an error if the env var is unset or empty.
func (p *ProviderConfig) ResolveSecret() (string, error) {
	if p.SecretEnv == "" {
		return "", fmt.Errorf("provider has no secret_env configured")
	}
	val := os.Getenv(p.SecretEnv)
	if val == "" {
		return "", fmt.Errorf("env var %s is unset or empty", p.SecretEnv)
	}
	return val, nil
}

// LoadConfig reads ~/gt/settings/telegraph.toml from the given town root.
// Returns nil, nil if the file does not exist (telegraph is not configured).
// Returns a non-nil error only on parse failures.
func LoadConfig(townRoot string) (*TelegraphConfig, error) {
	path := filepath.Join(townRoot, ConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading telegraph config: %w", err)
	}

	var cfg Config
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, fmt.Errorf("parsing telegraph config %s: %w", path, err)
	}

	effective := cfg.Telegraph.effective()
	return &effective, nil
}
