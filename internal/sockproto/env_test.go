package sockproto

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEnvAllowed(t *testing.T) {
	for _, k := range []string{"GT_ROLE", "GT_RIG", "GT_SESSION", "BD_DB",
		"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL"} {
		assert.True(t, EnvAllowed(k), "%s is session env the agent needs", k)
	}
	for _, k := range []string{
		// Loader vars and PATH are code execution against a native agent.
		"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH", "PATH", "SHELL", "HOME",
		// Credentials come from the worker's agent env file, never the wire —
		// and neither does the endpoint a credential is sent TO, or a confused
		// orchestrator could exfiltrate a file-provisioned key.
		"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "GITHUB_TOKEN", "GT_AUTH_TOKEN", "BD_SECRET", "SSH_PRIVATE_KEY",
		// Endpoints are worker-local facts: an orchestrator-supplied one is at
		// best unreachable from the worker and at worst a redirect (a wire
		// GT_PROXY_URL turns injected RPC responses into fake mail/beads).
		"GT_PROXY_URL", "GT_OTEL_LOGS_URL", "GT_DOLT_HOST", "BD_SERVER_ADDR", "GT_RELAY_PORT",
		// Launcher-only plumbing, and the relay toggle that suppresses
		// gt-proxy-client's loopback guard — only the worker may assert that.
		"GT_WORKER_TOKEN", "GT_WORKER_NAME", "GT_PROXY_RELAY",
		// Unrecognized entirely.
		"CLAUDE_UNKNOWN_FUTURE_VAR", "RANDOM_VAR", "",
	} {
		assert.False(t, EnvAllowed(k), "%s must be refused", k)
	}
}

func TestEnvEndpointKey(t *testing.T) {
	for _, k := range []string{"GT_PROXY_URL", "gt_otel_logs_url", "GT_DOLT_HOST", "X_ENDPOINT", "Y_PORT", "Z_ADDRESS"} {
		assert.True(t, EnvEndpointKey(k), "%s names a destination", k)
	}
	for _, k := range []string{"GT_ROLE", "GT_TOWN_ROOT", "ANTHROPIC_MODEL", "GT_PORTABLE"} {
		assert.False(t, EnvEndpointKey(k), "%s does not name a destination", k)
	}
}

func TestEnvSecretKey(t *testing.T) {
	assert.True(t, EnvSecretKey("gt_api_key"), "the check is case-insensitive")
	assert.True(t, EnvSecretKey("MY_PASSWD"))
	assert.False(t, EnvSecretKey("GT_ROLE"))
}
