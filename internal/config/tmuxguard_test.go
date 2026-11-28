package config

import "github.com/steveyegge/gastown/internal/tmuxguard"

// The integration tests exec tmux, and this package does not link the tmux
// package that would otherwise isolate them.
func init() { tmuxguard.Isolate() }
