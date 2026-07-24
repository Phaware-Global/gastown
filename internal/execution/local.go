package execution

import (
	"context"

	"github.com/steveyegge/gastown/internal/config"
)

// LocalBackend runs polecats on the orchestrator host — today's behavior,
// refactored behind the Backend interface with no behavior change.
// Provision/Teardown are no-ops and WrapCommand returns the argv unchanged.
type LocalBackend struct{}

func init() {
	Register(config.ExecutionBackendLocal, func(*config.ExecutionConfig) (Backend, error) {
		return LocalBackend{}, nil
	})
}

// Provision is a no-op: the host is already provisioned.
func (LocalBackend) Provision(_ context.Context, spec PolecatSpec) (Endpoint, error) {
	return Endpoint{
		Backend:  config.ExecutionBackendLocal,
		Identity: spec.Identity(),
	}, nil
}

// WrapCommand returns the agent argv unchanged; env stays with the caller's
// existing exec-env prefix path.
func (LocalBackend) WrapCommand(_ Endpoint, agentArgv []string, _ map[string]string) ([]string, error) {
	return agentArgv, nil
}

// Teardown is a no-op: there is nothing to release on the host.
func (LocalBackend) Teardown(context.Context, Endpoint) error { return nil }

// Discover returns nothing: local polecats are tracked by tmux sessions, not
// provider-side state.
func (LocalBackend) Discover(context.Context, IdentityTags) ([]Endpoint, error) {
	return nil, nil
}
