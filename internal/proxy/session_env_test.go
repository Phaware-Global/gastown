package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// printGTEnvServer builds a Server whose allowlist contains a script that
// prints the session-identity env vars, so handleExec output shows exactly
// what a proxied gt would see.
func printGTEnvServer(t *testing.T, townRoot string) *Server {
	t.Helper()
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "printgtenv")
	require.NoError(t, os.WriteFile(script,
		[]byte("#!/bin/sh\necho \"session=$GT_SESSION rig=$GT_RIG polecat=$GT_POLECAT role=$GT_ROLE\"\n"), 0755))
	t.Setenv("PATH", scriptDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv, err := New(Config{
		AllowedCommands: []string{"printgtenv"},
		TownRoot:        townRoot,
		Logger:          discardLogger(),
	}, nil)
	require.NoError(t, err)
	return srv
}

func execPrintGTEnv(t *testing.T, srv *Server, cn string) string {
	t.Helper()
	req := makeFakeRequest("POST", "/v1/exec", `{"argv":["printgtenv"]}`, cn)
	rec := httptest.NewRecorder()
	srv.handleExec(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp execResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, 0, resp.ExitCode)
	return strings.TrimSpace(resp.Stdout)
}

func TestExecInjectsSessionIdentityEnv(t *testing.T) {
	t.Run("default prefix when no rigs.json", func(t *testing.T) {
		srv := printGTEnvServer(t, t.TempDir())
		out := execPrintGTEnv(t, srv, "gt-MyRig-furiosa")
		// This is the §8.1 lifeline: GT_SESSION is what persistentPreRun keys
		// the heartbeat touch on, so a remote polecat's proxied gt calls keep
		// its host-side heartbeat fresh.
		assert.Equal(t, "session=gt-furiosa rig=MyRig polecat=furiosa role=polecat", out)
	})

	t.Run("rig prefix from rigs.json", func(t *testing.T) {
		townRoot := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(townRoot, "rigs.json"),
			[]byte(`{"rigs":{"MyRig":{"beads":{"prefix":"mr"}}}}`), 0644))
		srv := printGTEnvServer(t, townRoot)
		out := execPrintGTEnv(t, srv, "gt-MyRig-furiosa")
		assert.Equal(t, "session=mr-furiosa rig=MyRig polecat=furiosa role=polecat", out)
	})

	t.Run("no client cert means no session env — even if the server's own env has one", func(t *testing.T) {
		// The proxy daemon itself may run inside a Gas Town session; its
		// GT_SESSION must never leak into (or be attributable to) an
		// unauthenticated exec.
		t.Setenv("GT_SESSION", "hq-mayor")
		t.Setenv("GT_ROLE", "mayor")
		srv := printGTEnvServer(t, t.TempDir())
		out := execPrintGTEnv(t, srv, "")
		assert.Equal(t, "session= rig= polecat= role=", out)
	})

	t.Run("hyphenated rig maps whole rig into GT_RIG", func(t *testing.T) {
		srv := printGTEnvServer(t, t.TempDir())
		out := execPrintGTEnv(t, srv, "gt-gas-town-furiosa")
		assert.Equal(t, "session=gt-furiosa rig=gas-town polecat=furiosa role=polecat", out)
	})
}

func TestSessionIdentityEnvUnit(t *testing.T) {
	srv, err := New(Config{AllowedCommands: []string{"echo"}, TownRoot: t.TempDir(), Logger: discardLogger()}, nil)
	require.NoError(t, err)

	t.Run("malformed identities yield nil", func(t *testing.T) {
		for _, id := range []string{"", "noslash", "/name", "rig/"} {
			assert.Nil(t, srv.sessionIdentityEnv(id), "identity %q", id)
		}
	})

	t.Run("stripEnvKeys drops exactly the listed keys", func(t *testing.T) {
		env := []string{"HOME=/h", "GT_SESSION=evil", "GT_RIG=evil", "PATH=/p", "GT_ROLE=evil", "GT_POLECAT=evil", "GT_SESSIONX=keep"}
		got := stripEnvKeys(env, sessionIdentityEnvKeys)
		assert.Equal(t, []string{"HOME=/h", "PATH=/p", "GT_SESSIONX=keep"}, got)
	})
}

// TestRunCommand_StripsHostileSessionEnv exercises the strip+inject in
// runCommand itself: a hostile GT_SESSION already present in the base/override
// env (the path minimalEnv would never produce, but the bd-create env
// builders construct overrides) must not survive — the child sees the
// server-derived identity value, or nothing at all.
func TestRunCommand_StripsHostileSessionEnv(t *testing.T) {
	printSession := []string{"sh", "-c", "printf '%s' \"session=$GT_SESSION role=$GT_ROLE\""}
	hostile := []string{"PATH=" + os.Getenv("PATH"), "GT_SESSION=evil", "GT_ROLE=mayor"}

	t.Run("hostile override is replaced by the derived identity", func(t *testing.T) {
		derived := []string{"GT_SESSION=gt-furiosa", "GT_ROLE=polecat"}
		stdout, _, code := runCommand(context.Background(), printSession, "MyRig/furiosa", derived, hostile)
		require.Equal(t, 0, code)
		assert.Equal(t, "session=gt-furiosa role=polecat", stdout)
	})

	t.Run("hostile override with no authenticated identity is stripped to nothing", func(t *testing.T) {
		stdout, _, code := runCommand(context.Background(), printSession, "", nil, hostile)
		require.Equal(t, 0, code)
		assert.Equal(t, "session= role=", stdout)
	})
}
