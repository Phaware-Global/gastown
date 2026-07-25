package socket

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
)

func TestSettings_ValidationMatrix(t *testing.T) {
	cases := []struct {
		name    string
		s       Settings
		wantErr string
	}{
		{"tcp auto ok", Settings{Address: "10.0.0.1:9878", TLS: TLSConfig{Mode: "auto", WorkerName: "gpu-1"}}, ""},
		{"auto needs worker_name", Settings{Address: "10.0.0.1:9878", TLS: TLSConfig{Mode: "auto"}}, "worker_name"},
		{"manual needs files", Settings{Address: "10.0.0.1:9878", TLS: TLSConfig{Mode: "manual", WorkerName: "x"}}, "ca_file"},
		{"none on tcp refused", Settings{Address: "10.0.0.1:9878", TLS: TLSConfig{Mode: "none"}, Token: "t"}, "unix-socket only"},
		{"none on unix needs token", Settings{Address: "unix:///w.sock", TLS: TLSConfig{Mode: "none"}}, "pre-shared token"},
		{"none on unix with token ok", Settings{Address: "unix:///w.sock", TLS: TLSConfig{Mode: "none"}, Token: "t"}, ""},
		{"no address", Settings{TLS: TLSConfig{Mode: "auto", WorkerName: "x"}}, "address is required"},
		{"bad mode", Settings{Address: "unix:///w.sock", TLS: TLSConfig{Mode: "weird"}}, "tls.mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestParseSettings_ThroughCoreExecutionConfig(t *testing.T) {
	// The socket extension must survive a round-trip through the core
	// ExecutionConfig's opaque-extension (un)marshaling (#156 contract):
	// parse a full strict-JSON execution block and confirm parseSettings
	// recovers the socket sub-object.
	blob := `{
		"backend": "socket",
		"exec_mode": "container",
		"image": "ghcr.io/example/dev:latest",
		"requires_docker": true,
		"network": { "mode": "open" },
		"checkpoint_interval": "5m",
		"socket": {
			"address": "10.0.1.42:9878",
			"tls": { "mode": "auto", "worker_name": "gpu-box-1" }
		}
	}`
	var cfg config.ExecutionConfig
	require.NoError(t, json.Unmarshal([]byte(blob), &cfg))

	s, err := parseSettings(&cfg)
	require.NoError(t, err)
	assert.Equal(t, "10.0.1.42:9878", s.Address)
	assert.Equal(t, "gpu-box-1", s.TLS.WorkerName)

	// And it survives re-marshal (the core preserves the extension verbatim).
	out, err := json.Marshal(&cfg)
	require.NoError(t, err)
	var back config.ExecutionConfig
	require.NoError(t, json.Unmarshal(out, &back))
	s2, err := parseSettings(&back)
	require.NoError(t, err)
	assert.Equal(t, s.Address, s2.Address)
}

func TestParseSettings_SandboxedNativeRejected(t *testing.T) {
	blob := `{
		"backend": "socket",
		"exec_mode": "native",
		"network": { "mode": "sandboxed" },
		"socket": { "address": "unix:///w.sock", "tls": { "mode": "none" }, "token": "t" }
	}`
	var cfg config.ExecutionConfig
	require.NoError(t, json.Unmarshal([]byte(blob), &cfg))
	_, err := parseSettings(&cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sandboxed")
}

func TestParseSettings_MissingExtension(t *testing.T) {
	cfg := config.ExecutionConfig{Backend: "socket", ExecMode: "container"}
	_, err := parseSettings(&cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing")
}
