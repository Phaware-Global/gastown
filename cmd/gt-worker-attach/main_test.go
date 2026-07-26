package main

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/sockproto"
)

// TestCanonicalSignalName pins that the wire carries CANONICAL names. Go's
// os.Signal.String() yields descriptive forms ("interrupt", "terminated"); a
// worker that only parsed those would silently drop the pane's Ctrl-C.
func TestCanonicalSignalName(t *testing.T) {
	assert.Equal(t, "SIGINT", canonicalSignalName(syscall.SIGINT))
	assert.Equal(t, "SIGTERM", canonicalSignalName(syscall.SIGTERM))
	assert.Equal(t, "SIGHUP", canonicalSignalName(syscall.SIGHUP))
	assert.Equal(t, "SIGQUIT", canonicalSignalName(syscall.SIGQUIT))
	// Anything else still crosses in an upper-cased form the worker can reject
	// cleanly rather than misinterpret.
	assert.Equal(t, "USER DEFINED SIGNAL 1", canonicalSignalName(syscall.SIGUSR1))
}

// TestSessionEnv_ForwardsOnlyWirePolicy pins that the launcher forwards exactly
// what the worker will accept: its own credential (GT_WORKER_TOKEN) and
// unrelated host env must not ride along, and no secret-shaped key may.
func TestSessionEnv_ForwardsOnlyWirePolicy(t *testing.T) {
	t.Setenv("GT_ROLE", "polecat")
	t.Setenv("GT_RIG", "demo")
	t.Setenv("ANTHROPIC_MODEL", "opus")
	t.Setenv("GT_WORKER_TOKEN", "t0k")
	t.Setenv("ANTHROPIC_API_KEY", "sk-host")
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")

	env := sessionEnv()
	assert.Equal(t, "polecat", env["GT_ROLE"])
	assert.Equal(t, "demo", env["GT_RIG"])
	assert.Equal(t, "opus", env["ANTHROPIC_MODEL"])
	for _, k := range []string{"GT_WORKER_TOKEN", "ANTHROPIC_API_KEY", "LD_PRELOAD", "PATH", "HOME"} {
		_, ok := env[k]
		assert.False(t, ok, "%s must not be forwarded", k)
	}
	// Every forwarded key must pass the same policy the worker enforces, or the
	// attach would be refused outright.
	for k := range env {
		require.True(t, sockproto.EnvAllowed(k), "forwarded %q must be wire-legal", k)
	}
}
