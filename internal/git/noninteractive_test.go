package git

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNonInteractiveGitEnv(t *testing.T) {
	env := nonInteractiveGitEnv("FOO=bar")
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "FOO=bar"} {
		if !slices.Contains(env, want) {
			t.Errorf("nonInteractiveGitEnv missing %q", want)
		}
	}
	// Caller-supplied vars come last so they win over the defaults.
	if env[len(env)-1] != "FOO=bar" {
		t.Errorf("extra env should be appended last, got %q", env[len(env)-1])
	}
}

// TestRemoteBranchExists_AuthRequiredFailsFast is the regression test for
// `gt polecat list --all` stopping on a "Username for 'https://github.com'"
// prompt: when no credential helper can answer, ls-remote must fail with
// prompting disabled instead of asking on the terminal.
func TestRemoteBranchExists_AuthRequiredFailsFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	// Isolate from the developer's credential helpers and askpass programs,
	// which would otherwise answer (or open a GUI) before git reaches the
	// terminal prompt.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("SSH_ASKPASS", "")
	// Unset, not empty: git parses an empty GIT_TERMINAL_PROMPT as false.
	t.Setenv("GIT_TERMINAL_PROMPT", "")
	os.Unsetenv("GIT_TERMINAL_PROMPT")

	dir := initTestRepo(t)
	cmd := exec.Command("git", "remote", "add", "origin", srv.URL+"/repo.git")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}

	start := time.Now()
	_, err := NewGit(dir).RemoteBranchExists("origin", "main")
	if err == nil {
		t.Fatal("expected ls-remote against an auth-required remote to fail")
	}
	if !strings.Contains(err.Error(), "terminal prompts disabled") {
		t.Errorf("expected git to report prompts disabled, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("ls-remote took %v; should fail fast", elapsed)
	}
}
