// Package execution defines the pluggable per-rig execution backend that
// decides where a polecat's agent process runs (docs/design/
// remote-polecat-execution.md §5). The local backend preserves today's
// on-host behavior; remote providers (EC2, socket) implement the same
// interface in their own packages and register themselves.
package execution

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/steveyegge/gastown/internal/config"
)

// IdentityTags is the polecat identity a backend records on provider-side
// resources at Provision so endpoints are re-discoverable after a daemon
// restart (design §5 "Endpoint discovery").
type IdentityTags struct {
	Rig     string
	Polecat string
	Session string
}

// Endpoint is the handle the daemon uses for WrapCommand/Teardown. It MUST be
// reconstructable from provider-side state via Discover, never only from
// daemon memory.
type Endpoint struct {
	Backend  string       // backend name that owns this endpoint
	ID       string       // provider-specific handle (instance ID, session ID, ...)
	Address  string       // provider-specific address, if any
	Identity IdentityTags // identity recorded provider-side at Provision
}

// PolecatSpec carries the resolved per-rig execution config and the polecat
// identity, so backends are config-driven, not hard-coded.
type PolecatSpec struct {
	Rig     string
	Polecat string
	Session string
	Config  *config.ExecutionConfig // shared fields + opaque provider extension
}

// Identity returns the spec's identity as tags for provider-side recording.
func (s PolecatSpec) Identity() IdentityTags {
	return IdentityTags{Rig: s.Rig, Polecat: s.Polecat, Session: s.Session}
}

// Backend is the per-rig execution provider (design §5).
//
// Success is never derived from the exit code of the argv WrapCommand
// returns: completion is the agent calling gt done, and crashes are caught by
// stale-heartbeat liveness. Providers' exec channels are not required to
// propagate remote exit status.
type Backend interface {
	// Provision acquires the execution environment and blocks until the agent
	// can be launched into it. Idempotent for resume: if a live worker already
	// exists for this identity, reattach instead of acquiring a new one.
	Provision(ctx context.Context, spec PolecatSpec) (Endpoint, error)

	// WrapCommand takes the fully-resolved agent argv and session env and
	// returns the complete argv the daemon should exec on the orchestrator
	// host to launch the agent. The backend controls the entire structure —
	// it does not merely prepend a prefix — and is responsible for landing
	// argv and env in the remote process by whatever mechanism its launcher
	// requires (design §7.4).
	//
	// The returned argv is rendered into a host tmux command line and is
	// visible in host process listings. Secret env (LLM keys, registry creds,
	// the proxy key) MUST therefore be delivered out-of-band worker-side
	// (design §7.1–7.2, §7.4), never folded into the returned argv. Only
	// non-secret session env may travel in the launcher payload.
	WrapCommand(ep Endpoint, agentArgv []string, env map[string]string) ([]string, error)

	// Teardown releases the environment: destroy the sandbox, or end the
	// session on a persistent worker. Called by the reaper after cooldown, on
	// max_runtime expiry, or on explicit nuke.
	Teardown(ctx context.Context, ep Endpoint) error

	// Discover re-finds live endpoints from provider-side state by identity,
	// so the daemon can reattach after a restart and the reaper can sweep
	// orphans. Empty filter fields match everything.
	Discover(ctx context.Context, filter IdentityTags) ([]Endpoint, error)
}

// Factory constructs a backend from a rig's execution config (which includes
// the provider's opaque extension via cfg.ProviderExtension()).
type Factory func(cfg *config.ExecutionConfig) (Backend, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register makes a backend available under the given config backend name.
// Provider packages call this from init(). Panics on duplicate registration
// (a wiring bug, not a runtime condition).
func Register(name string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("execution: backend %q registered twice", name))
	}
	registry[name] = f
}

// ForConfig resolves the backend for a rig's execution config. A nil config
// (or backend "local") yields the local backend.
func ForConfig(cfg *config.ExecutionConfig) (Backend, error) {
	name := cfg.BackendName()
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("execution: unknown backend %q (registered: %v)", name, registeredNames())
	}
	return f(cfg)
}

func registeredNames() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
