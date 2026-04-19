package telegraph

import (
	"fmt"
	"os"
	"time"
)

// Config is the top-level Telegraph configuration.
// It is parsed from ~/gt/settings/telegraph.toml at daemon startup.
type Config struct {
	// ListenAddr is the bind address for the HTTP listener (e.g. ":8765").
	ListenAddr string

	// BufferSize is the capacity of the L1→L2 RawEvent channel.
	// When full, new requests are rejected with HTTP 503 (backpressure).
	BufferSize int

	// NudgeWindow is the minimum interval between Mayor nudges.
	NudgeWindow time.Duration

	// BodyCap is the maximum bytes of external text included in mail body.
	BodyCap int

	// LogFile is the log destination. Empty means stderr / daemon log.
	LogFile string

	// Providers holds per-provider configuration, keyed by stable provider ID.
	Providers map[string]ProviderConfig
}

// ProviderConfig holds per-provider settings.
type ProviderConfig struct {
	// Enabled controls whether this provider accepts events.
	Enabled bool

	// SecretEnv is the name of the environment variable holding the HMAC secret.
	// The secret value is never stored in config or logs.
	SecretEnv string

	// Events is the list of event type names this provider handles (v1 scope).
	Events []string
}

// DefaultConfig returns a Config populated with design-doc defaults.
func DefaultConfig() *Config {
	return &Config{
		ListenAddr:  ":8765",
		BufferSize:  256,
		NudgeWindow: 30 * time.Second,
		BodyCap:     4096,
		Providers:   make(map[string]ProviderConfig),
	}
}

// ResolveSecret returns the HMAC secret for the given provider by reading
// the env var named in the provider's SecretEnv field.
// Returns an error if the provider is unknown, SecretEnv is unset, or the
// env var is empty. The returned secret must never be logged or written to disk.
func (c *Config) ResolveSecret(provider string) (string, error) {
	pc, ok := c.Providers[provider]
	if !ok {
		return "", fmt.Errorf("telegraph: unknown provider %q", provider)
	}
	if pc.SecretEnv == "" {
		return "", fmt.Errorf("telegraph: provider %q has no secret_env configured", provider)
	}
	secret := os.Getenv(pc.SecretEnv)
	if secret == "" {
		return "", fmt.Errorf("telegraph: env var %q (provider %q) is unset or empty", pc.SecretEnv, provider)
	}
	return secret, nil
}
