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
		// Launcher-only plumbing.
		"GT_WORKER_TOKEN", "GT_WORKER_NAME",
		// Unrecognized entirely.
		"CLAUDE_UNKNOWN_FUTURE_VAR", "RANDOM_VAR", "",
	} {
		assert.False(t, EnvAllowed(k), "%s must be refused", k)
	}
}

func TestEnvSecretKey(t *testing.T) {
	assert.True(t, EnvSecretKey("gt_api_key"), "the check is case-insensitive")
	assert.True(t, EnvSecretKey("MY_PASSWD"))
	assert.False(t, EnvSecretKey("GT_ROLE"))
}
