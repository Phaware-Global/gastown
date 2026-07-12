package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Execution backend names known to the core. Any other value is provider-defined
// and validated when the backend is resolved, not at config load.
const (
	ExecutionBackendLocal = "local"
)

// Execution exec modes (docs/design/remote-polecat-execution.md §6).
const (
	ExecModeContainer = "container"
	ExecModeNative    = "native"
)

// Network egress postures (docs/design/remote-polecat-execution.md §7.3).
const (
	NetworkModeSandboxed = "sandboxed"
	NetworkModeGateway   = "gateway"
	NetworkModeOpen      = "open"
)

// Execution lifecycle defaults (docs/design/remote-polecat-execution.md §4).
const (
	DefaultCheckpointInterval = 5 * time.Minute
	DefaultExecutionCooldown  = 10 * time.Minute
	DefaultMaxRuntime         = 4 * time.Hour
)

// ExecutionNetworkConfig holds the work-egress posture for a rig's polecats.
// How each mode is realized is provider-defined.
type ExecutionNetworkConfig struct {
	Mode string `json:"mode,omitempty"` // "sandboxed" | "gateway" | "open"
}

// ExecutionConfig is the per-rig execution block of settings/config.json
// (docs/design/remote-polecat-execution.md §4). Absent or backend "local"
// means today's on-host behavior.
//
// Only the shared, provider-agnostic fields are modeled here. Provider-specific
// keys live in a sibling object keyed by the backend name (e.g. "ec2": {...})
// and are preserved opaquely in Extensions for the backend to parse.
type ExecutionConfig struct {
	Backend        string                  `json:"backend,omitempty"`             // "local" (default) | provider-defined
	ExecMode       string                  `json:"exec_mode,omitempty"`           // "container" | "native"
	Image          string                  `json:"image,omitempty"`               // work image (container mode)
	RequiresDocker bool                    `json:"requires_docker,omitempty"`     // capability gate (§10)
	Network        *ExecutionNetworkConfig `json:"network,omitempty"`             // egress posture (§7.3)
	CheckpointStr  string                  `json:"checkpoint_interval,omitempty"` // duration; default 5m
	CooldownStr    string                  `json:"cooldown,omitempty"`            // duration; default 10m
	MaxRuntimeStr  string                  `json:"max_runtime,omitempty"`         // duration; default 4h

	// Extensions holds provider-specific sub-objects keyed by backend name,
	// preserved verbatim for the selected backend to parse. Not a JSON field
	// itself: any unknown key in the execution block lands here.
	Extensions map[string]json.RawMessage `json:"-"`
}

// executionConfigKnown mirrors ExecutionConfig's JSON fields for two-pass
// unmarshaling; keep in sync with the struct tags above.
var executionConfigKnown = map[string]bool{
	"backend":             true,
	"exec_mode":           true,
	"image":               true,
	"requires_docker":     true,
	"network":             true,
	"checkpoint_interval": true,
	"cooldown":            true,
	"max_runtime":         true,
}

// UnmarshalJSON decodes the shared fields and preserves unknown keys
// (provider extensions) in Extensions.
func (e *ExecutionConfig) UnmarshalJSON(data []byte) error {
	type plain ExecutionConfig
	if err := json.Unmarshal(data, (*plain)(e)); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		if !executionConfigKnown[k] {
			if e.Extensions == nil {
				e.Extensions = map[string]json.RawMessage{}
			}
			e.Extensions[k] = v
		}
	}
	return nil
}

// MarshalJSON re-emits the shared fields with provider extensions as sibling keys.
func (e *ExecutionConfig) MarshalJSON() ([]byte, error) {
	if e == nil {
		return []byte("null"), nil
	}
	type plain ExecutionConfig
	data, err := json.Marshal((*plain)(e))
	if err != nil {
		return nil, err
	}
	if len(e.Extensions) == 0 {
		return data, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, err
	}
	for k, v := range e.Extensions {
		merged[k] = v
	}
	return json.Marshal(merged)
}

// BackendName returns the configured backend, defaulting to "local".
// Nil-safe.
func (e *ExecutionConfig) BackendName() string {
	if e == nil || e.Backend == "" {
		return ExecutionBackendLocal
	}
	return e.Backend
}

// IsRemote reports whether this rig's polecats run off-host.
// Nil-safe: no config means local.
func (e *ExecutionConfig) IsRemote() bool {
	return e.BackendName() != ExecutionBackendLocal
}

// NetworkMode returns the egress posture, defaulting to "open". Nil-safe.
func (e *ExecutionConfig) NetworkMode() string {
	if e == nil || e.Network == nil || e.Network.Mode == "" {
		return NetworkModeOpen
	}
	return e.Network.Mode
}

// CheckpointInterval returns the checkpoint interval, defaulting to 5m. Nil-safe.
func (e *ExecutionConfig) CheckpointInterval() time.Duration {
	if e == nil {
		return DefaultCheckpointInterval
	}
	return ParseDurationOrDefault(e.CheckpointStr, DefaultCheckpointInterval)
}

// Cooldown returns the post-DONE teardown delay, defaulting to 10m. Nil-safe.
func (e *ExecutionConfig) Cooldown() time.Duration {
	if e == nil {
		return DefaultExecutionCooldown
	}
	return ParseDurationOrDefault(e.CooldownStr, DefaultExecutionCooldown)
}

// MaxRuntime returns the absolute zombie cap, defaulting to 4h. Nil-safe.
func (e *ExecutionConfig) MaxRuntime() time.Duration {
	if e == nil {
		return DefaultMaxRuntime
	}
	return ParseDurationOrDefault(e.MaxRuntimeStr, DefaultMaxRuntime)
}

// MaxRuntimeSet reports whether max_runtime was explicitly configured (as
// opposed to defaulted). The reaper's hard-kill cap keys on this so merely
// declaring an execution block — e.g. to select a backend or network posture
// — never silently imposes a wall-clock kill on a busy polecat. Nil-safe.
func (e *ExecutionConfig) MaxRuntimeSet() bool {
	return e != nil && e.MaxRuntimeStr != ""
}

// ProviderExtension returns the raw provider-specific sub-object for the
// selected backend (e.g. the "ec2" object when backend is "ec2"), or nil if
// none was configured. Nil-safe.
func (e *ExecutionConfig) ProviderExtension() json.RawMessage {
	if e == nil {
		return nil
	}
	return e.Extensions[e.BackendName()]
}

// ErrInvalidExecutionConfig indicates an invalid execution block.
var ErrInvalidExecutionConfig = errors.New("invalid execution config")

// validateExecutionConfig validates the shared fields of an execution block.
// Provider extensions are opaque here; the selected backend validates its own.
// The backend NAME is deliberately not validated here: the provider registry
// lives above this package (internal/execution imports config), so an unknown
// name fails closed at backend resolution and at orchestrator-side preflight.
func validateExecutionConfig(e *ExecutionConfig) error {
	if e == nil {
		return nil
	}
	switch e.ExecMode {
	case "", ExecModeContainer, ExecModeNative:
	default:
		return fmt.Errorf("%w: exec_mode %q, want %q or %q",
			ErrInvalidExecutionConfig, e.ExecMode, ExecModeContainer, ExecModeNative)
	}
	if e.Network != nil {
		switch e.Network.Mode {
		case "", NetworkModeSandboxed, NetworkModeGateway, NetworkModeOpen:
		default:
			return fmt.Errorf("%w: network.mode %q, want %q, %q, or %q",
				ErrInvalidExecutionConfig, e.Network.Mode,
				NetworkModeSandboxed, NetworkModeGateway, NetworkModeOpen)
		}
	}
	for name, val := range map[string]string{
		"checkpoint_interval": e.CheckpointStr,
		"cooldown":            e.CooldownStr,
		"max_runtime":         e.MaxRuntimeStr,
	} {
		if val == "" {
			continue
		}
		d, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("%w: invalid %s: %v", ErrInvalidExecutionConfig, name, err)
		}
		if d <= 0 {
			return fmt.Errorf("%w: %s must be positive, got %v", ErrInvalidExecutionConfig, name, d)
		}
	}
	// requires_docker + sandboxed is rejected until rootless dockerd ships
	// (design §12, decision 5): the docker-socket mitigations do not contain a
	// socket-holding agent, so this pairing is unsafe today.
	if e.RequiresDocker && e.NetworkMode() == NetworkModeSandboxed {
		return fmt.Errorf("%w: requires_docker with network.mode=sandboxed is not supported (rootless dockerd not yet available)",
			ErrInvalidExecutionConfig)
	}
	return nil
}
