package telegraph

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	// DefaultListenAddr is the default HTTP bind address for the Telegraph webhook listener.
	DefaultListenAddr = ":8765"

	// DefaultBufferSize is the default max RawEvents queued between L1 and L2.
	DefaultBufferSize = 256

	// DefaultNudgeWindow is the default rate-limit window for Mayor nudges.
	// At most one nudge is sent per window regardless of event volume.
	DefaultNudgeWindow = 30 * time.Second

	// DefaultBodyCap is the default max bytes of external text included in mail body.
	DefaultBodyCap = 4096

	// ConfigFileName is the town-level config file name for Telegraph.
	ConfigFileName = "telegraph.toml"
)

// Config is the top-level Telegraph configuration loaded from telegraph.toml.
type Config struct {
	Telegraph TelegraphConfig `toml:"telegraph"`
}

// TelegraphConfig holds the core Telegraph settings.
type TelegraphConfig struct {
	ListenAddr  string                     `toml:"listen_addr"`
	BufferSize  int                        `toml:"buffer_size"`
	NudgeWindow string                     `toml:"nudge_window"`
	BodyCap     int                        `toml:"body_cap"`
	LogFile     string                     `toml:"log_file"`
	Providers   map[string]*ProviderConfig `toml:"providers"`
}

// ProviderConfig holds per-provider settings.
type ProviderConfig struct {
	Enabled   bool     `toml:"enabled"`
	SecretEnv string   `toml:"secret_env"`
	Events    []string `toml:"events"`
}

// NudgeWindowDuration parses the NudgeWindow string into a time.Duration.
// Returns DefaultNudgeWindow if unset or unparseable.
func (c *TelegraphConfig) NudgeWindowDuration() time.Duration {
	if c.NudgeWindow == "" {
		return DefaultNudgeWindow
	}
	d, err := time.ParseDuration(c.NudgeWindow)
	if err != nil {
		return DefaultNudgeWindow
	}
	return d
}

// EffectiveListenAddr returns the listen address, falling back to DefaultListenAddr.
func (c *TelegraphConfig) EffectiveListenAddr() string {
	if c.ListenAddr == "" {
		return DefaultListenAddr
	}
	return c.ListenAddr
}

// EffectiveBufferSize returns the buffer size, falling back to DefaultBufferSize.
func (c *TelegraphConfig) EffectiveBufferSize() int {
	if c.BufferSize <= 0 {
		return DefaultBufferSize
	}
	return c.BufferSize
}

// EffectiveBodyCap returns the body cap, falling back to DefaultBodyCap.
func (c *TelegraphConfig) EffectiveBodyCap() int {
	if c.BodyCap <= 0 {
		return DefaultBodyCap
	}
	return c.BodyCap
}

// ResolveSecret returns the HMAC secret for a provider by reading the env var
// named in SecretEnv. Returns an error if the env var is unset or empty.
// The secret value never appears in error messages.
func (p *ProviderConfig) ResolveSecret() (string, error) {
	if p.SecretEnv == "" {
		return "", fmt.Errorf("secret_env not configured for provider")
	}
	val := os.Getenv(p.SecretEnv)
	if val == "" {
		return "", fmt.Errorf("secret env var %q is not set", p.SecretEnv)
	}
	return val, nil
}

// LoadConfig reads the Telegraph config from the town settings directory.
// Returns a default config if the file does not exist.
func LoadConfig(townRoot string) (*TelegraphConfig, error) {
	path := filepath.Join(townRoot, "settings", ConfigFileName)

	var wrapper Config
	wrapper.Telegraph = TelegraphConfig{
		ListenAddr:  DefaultListenAddr,
		BufferSize:  DefaultBufferSize,
		NudgeWindow: DefaultNudgeWindow.String(),
		BodyCap:     DefaultBodyCap,
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return &wrapper.Telegraph, nil
	}

	if _, err := toml.DecodeFile(path, &wrapper); err != nil {
		return nil, fmt.Errorf("parsing telegraph config %s: %w", path, err)
	}

	return &wrapper.Telegraph, nil
}
