package polecat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/execution"
)

// rigExecutionConfig loads the rig's execution block from settings/config.json.
// Returns nil (local execution) when no settings file exists. A settings file
// that fails to load is tolerated as local with a warning, consistent with the
// rest of this package's settings handling.
func rigExecutionConfig(rigPath string) *config.ExecutionConfig {
	settings, err := config.LoadRigSettings(config.RigSettingsPath(rigPath))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "Warning: could not load rig settings for execution config: %v\n", err)
		}
		return nil
	}
	return settings.Execution
}

// remoteProvision is the result of provisioning a polecat's remote execution
// worker: the launcher argv to run in the host pane, plus the backend and
// endpoint so the caller can tear the worker down if session start later fails.
type remoteProvision struct {
	argv    []string
	backend execution.Backend
	ep      execution.Endpoint
}

// teardown releases the provisioned worker. Best-effort — used on the
// start-failure cleanup path so a provisioned worker is never orphaned when a
// later step of session start errors out (remote-polecat-execution §9.5).
func (r *remoteProvision) teardown() {
	if r == nil || r.backend == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.backend.Teardown(ctx, r.ep); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: tearing down remote worker after failed session start: %v\n", err)
	}
}

// buildRemoteArgv provisions the rig's execution backend for this polecat and
// delegates final launcher-argv construction to it
// (docs/design/remote-polecat-execution.md §5, §7.4). Provision is idempotent:
// a live worker for the same identity is reattached, not re-acquired.
//
// The returned argv is the blocking-pane launcher command the host tmux pane
// runs; the backend is responsible for landing agentArgv and env in the remote
// process. The returned remoteProvision carries the backend + endpoint so the
// caller can teardown() on a later start failure.
func buildRemoteArgv(ctx context.Context, execCfg *config.ExecutionConfig, spec execution.PolecatSpec, agentArgv []string, env map[string]string) (*remoteProvision, error) {
	backend, err := execution.ForConfig(execCfg)
	if err != nil {
		return nil, err
	}
	ep, err := backend.Provision(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("provisioning %s execution environment: %w", execCfg.BackendName(), err)
	}
	wrapped, err := backend.WrapCommand(ep, agentArgv, env)
	if err != nil {
		// Provision succeeded but wrapping failed — release the worker now.
		_ = backend.Teardown(ctx, ep)
		return nil, fmt.Errorf("wrapping command for backend %s: %w", execCfg.BackendName(), err)
	}
	if len(wrapped) == 0 {
		_ = backend.Teardown(ctx, ep)
		return nil, fmt.Errorf("backend %s returned an empty launcher argv", execCfg.BackendName())
	}
	return &remoteProvision{argv: wrapped, backend: backend, ep: ep}, nil
}

// shellJoinArgv renders a launcher argv as a single shell command line for
// tmux, quoting each token with the same discipline BuildStartupCommand uses.
func shellJoinArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, tok := range argv {
		quoted[i] = config.ShellQuote(tok)
	}
	return strings.Join(quoted, " ")
}
