// Package socket implements the socket execution backend
// (docs/design/remote-polecat-execution-socket.md): running polecats on a
// pre-provisioned machine that runs a persistent gt-worker-client service,
// reached over TCP (mTLS) or a Unix socket. Provision opens a SESSION on a
// machine that persists; Teardown ends it. This is the first real
// ExecutionBackend beyond LocalBackend.
package socket

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/execution"
)

// BackendName is the config backend value that selects this provider.
const BackendName = "socket"

// TLS modes (§8).
const (
	tlsModeAuto   = "auto"   // managed by `gt worker enroll` under ~/.gt/worker-ca/
	tlsModeManual = "manual" // explicit ca/cert/key paths
	tlsModeNone   = "none"   // unix socket + pre-shared token only (§3.3)
)

// Settings is the socket-provider extension of the core execution block
// (§8), parsed from the opaque "socket" sub-object.
type Settings struct {
	// Address is a TCP "host:port" or "unix:///path/to.sock" (§3.3).
	Address string    `json:"address"`
	TLS     TLSConfig `json:"tls"`
	// AdminURL is the proxy admin base URL used to sign session CSRs (§4.2).
	// Empty means DefaultAdminURL. Loopback only — see newAdminSigner.
	AdminURL string `json:"admin_url,omitempty"`
	// Token is the pre-shared token for unix-socket mode (§3.3). Never used
	// on TCP. Typically supplied worker-side / operator env, not committed.
	Token string `json:"token,omitempty"`
}

// TLSConfig is the §8 tls block.
type TLSConfig struct {
	Mode       string `json:"mode"`        // "auto" | "manual" | "none"
	WorkerName string `json:"worker_name"` // enrolled machine identity, name-pinned
	CAFile     string `json:"ca_file"`     // manual: worker CA cert
	CertFile   string `json:"cert_file"`   // manual: orchestrator client cert
	KeyFile    string `json:"key_file"`    // manual: orchestrator client key
}

// isUnix reports whether the address is a Unix socket.
func (s *Settings) isUnix() bool {
	return strings.HasPrefix(s.Address, "unix://")
}

// unixPath returns the filesystem path for a unix:// address.
func (s *Settings) unixPath() string {
	return strings.TrimPrefix(s.Address, "unix://")
}

// tlsMode returns the effective TLS mode, defaulting to "auto".
func (s *Settings) tlsMode() string {
	if s.TLS.Mode == "" {
		return tlsModeAuto
	}
	return s.TLS.Mode
}

// validate checks the socket settings against the §3 / §8 rules.
func (s *Settings) validate() error {
	if s.Address == "" {
		return fmt.Errorf("socket: address is required")
	}
	// A unix socket's trust is filesystem permissions + the pre-shared token
	// (§3.3): dialTransport never runs TLS on a unix connection, so an
	// auto/manual TLS block would be silently ignored — a security-posture
	// surprise. Require mode=none on unix so the fs-permission trust model is
	// explicit, and reject a bare unix address with a TLS mode set.
	if s.isUnix() && s.tlsMode() != tlsModeNone {
		return fmt.Errorf("socket: a unix:// address must use tls.mode=none with a pre-shared token (§3.3); TLS is not applied to unix sockets, so auto/manual would silently connect unauthenticated")
	}
	switch s.tlsMode() {
	case tlsModeAuto:
		if s.TLS.WorkerName == "" {
			return fmt.Errorf("socket: tls.worker_name is required in auto mode (the enrolled machine to pin)")
		}
	case tlsModeManual:
		if s.TLS.CAFile == "" || s.TLS.CertFile == "" || s.TLS.KeyFile == "" {
			return fmt.Errorf("socket: manual TLS requires ca_file, cert_file, and key_file")
		}
		if s.TLS.WorkerName == "" {
			return fmt.Errorf("socket: tls.worker_name is required to pin the machine identity")
		}
	case tlsModeNone:
		// §3.3: plaintext TCP with a bearer token fails the core §7
		// invariants — token auth is permitted ONLY on a permission-gated
		// Unix socket.
		if !s.isUnix() {
			return fmt.Errorf("socket: tls.mode=none is refused on a TCP address (§3.3: token auth is unix-socket only); a TCP worker must use mTLS")
		}
		if s.Token == "" {
			return fmt.Errorf("socket: tls.mode=none requires a pre-shared token")
		}
	default:
		return fmt.Errorf("socket: tls.mode %q, want %q, %q, or %q", s.TLS.Mode, tlsModeAuto, tlsModeManual, tlsModeNone)
	}
	return nil
}

// parseSettings extracts and validates the socket extension from an execution
// config's opaque provider sub-object.
func parseSettings(cfg *config.ExecutionConfig) (*Settings, error) {
	raw := cfg.ProviderExtension()
	if len(raw) == 0 {
		return nil, fmt.Errorf("socket: missing %q extension block in execution config (§8)", BackendName)
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("socket: parsing %q extension: %w", BackendName, err)
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	// §9: sandboxed + native cannot be honored on a machine gastown does not
	// firewall — reject rather than silently degrade.
	if cfg.NetworkMode() == config.NetworkModeSandboxed && cfg.ExecMode == config.ExecModeNative {
		return nil, fmt.Errorf("socket: network.mode=sandboxed with exec_mode=native is not supported on this provider (§9); use container mode")
	}
	return &s, nil
}

func init() {
	execution.Register(BackendName, func(cfg *config.ExecutionConfig) (execution.Backend, error) {
		return New(cfg)
	})
}

// orchestratorID identifies this orchestrator in the §3.2 handshake. The
// hostname is enough to tell two orchestrators apart in a worker's logs, which
// is all the field is used for today.
func orchestratorID() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "gt-orchestrator"
	}
	return h
}
