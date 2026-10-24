package doltserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFakeDoltForPush puts a fake `dolt` on PATH. `dolt remote -v` and the
// `SELECT url FROM dolt_remotes` query report remoteURL for a remote named
// "origin"; `dolt push` and any query containing DOLT_PUSH touch the returned
// marker file, so a test can tell whether a push was attempted. Nothing here
// touches the network or a real Dolt database.
func installFakeDoltForPush(t *testing.T, remoteURL string) (pushedMarker string) {
	t.Helper()

	pushedMarker = filepath.Join(t.TempDir(), "pushed")
	script := `#!/usr/bin/env bash
set -u
args="$*"
case "$args" in
  *"DOLT_PUSH"*) touch "$PUSHED_MARKER"; exit 0 ;;
  *"FROM dolt_remotes"*) echo "url"; [ -n "$REMOTE_URL" ] && echo "$REMOTE_URL"; exit 0 ;;
  "remote -v") [ -n "$REMOTE_URL" ] && echo "origin $REMOTE_URL {}"; exit 0 ;;
  "push "*) touch "$PUSHED_MARKER"; exit 0 ;;
esac
exit 0
`
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PUSHED_MARKER", pushedMarker)
	t.Setenv("REMOTE_URL", remoteURL)
	return pushedMarker
}

func stubRemotePushVerdict(t *testing.T, allowed bool, reason string) {
	t.Helper()
	orig := remotePushAllowed
	t.Cleanup(func() { remotePushAllowed = orig })
	remotePushAllowed = func(context.Context, string) (bool, string) { return allowed, reason }
}

func pushed(marker string) bool {
	_, err := os.Stat(marker)
	return err == nil
}

// pushers runs the same scenario against both push entry points of
// `gt dolt sync`: the CLI push and the SQL push through the running server.
var pushers = map[string]func(t *testing.T) error{
	"PushDatabase": func(t *testing.T) error {
		return PushDatabase(t.TempDir(), "origin", false)
	},
	"PushDatabaseSQL": func(t *testing.T) error {
		townRoot := t.TempDir()
		// The SQL path runs dolt with the data dir as its working directory.
		if err := os.MkdirAll(DefaultConfig(townRoot).DataDir, 0o755); err != nil {
			t.Fatal(err)
		}
		return PushDatabaseSQL(townRoot, "gt", "origin", false)
	},
}

func TestPush_RefusesNonPrivateRemote(t *testing.T) {
	const url = "git+https://github.com/example-owner/repo"
	for _, reason := range []string{"public", "internal", "visibility-lookup-failed", "gh-unavailable", "non-github-remote"} {
		for name, push := range pushers {
			t.Run(name+"/"+reason, func(t *testing.T) {
				marker := installFakeDoltForPush(t, url)
				stubRemotePushVerdict(t, false, reason)

				err := push(t)
				if err == nil {
					t.Fatalf("%s pushed to a remote whose verdict was %q", name, reason)
				}
				if !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), reason) {
					t.Errorf("error should say REFUSED and name the reason %q, got: %v", reason, err)
				}
				if pushed(marker) {
					t.Errorf("%s ran dolt push despite refusing", name)
				}
			})
		}
	}
}

func TestPush_AllowsPrivateRemote(t *testing.T) {
	for name, push := range pushers {
		t.Run(name, func(t *testing.T) {
			marker := installFakeDoltForPush(t, "git+https://github.com/example-owner/repo")
			stubRemotePushVerdict(t, true, "private")

			if err := push(t); err != nil {
				t.Fatalf("%s refused a confirmed-private remote: %v", name, err)
			}
			if !pushed(marker) {
				t.Errorf("%s did not push to a confirmed-private remote", name)
			}
		})
	}
}

// The verdict is asked about the URL of the remote actually being pushed, not
// about the first remote the database happens to list.
func TestPush_JudgesTheURLOfTheNamedRemote(t *testing.T) {
	for name, push := range pushers {
		t.Run(name, func(t *testing.T) {
			installFakeDoltForPush(t, "git+https://github.com/example-owner/repo")
			var asked []string
			orig := remotePushAllowed
			t.Cleanup(func() { remotePushAllowed = orig })
			remotePushAllowed = func(_ context.Context, u string) (bool, string) {
				asked = append(asked, u)
				return true, "private"
			}

			if err := push(t); err != nil {
				t.Fatal(err)
			}
			if len(asked) != 1 || asked[0] != "git+https://github.com/example-owner/repo" {
				t.Errorf("verdict asked about %v, want the remote's URL", asked)
			}
		})
	}
}

// A remote whose URL cannot be read is an unknown destination: refuse.
func TestPush_RefusesWhenRemoteURLUnresolvable(t *testing.T) {
	for name, push := range pushers {
		t.Run(name, func(t *testing.T) {
			marker := installFakeDoltForPush(t, "")
			stubRemotePushVerdict(t, true, "private") // must not be reached

			if err := push(t); err == nil {
				t.Fatalf("%s pushed with no resolvable remote URL", name)
			}
			if pushed(marker) {
				t.Errorf("%s ran dolt push with no resolvable remote URL", name)
			}
		})
	}
}

// DoltHub repos cannot be confirmed private, so the guard refuses every
// DoltHub push. SetupDoltHubRemote must therefore refuse up front: creating
// the repo and the remote first would leave both behind for a push that can
// never happen.
func TestSetupDoltHubRemote_RefusesBeforeCreatingAnything(t *testing.T) {
	var apiCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	origBase := dolthubAPIBase
	dolthubAPIBase = srv.URL
	t.Cleanup(func() { dolthubAPIBase = origBase })

	// Record every dolt invocation so a stray `remote add` is visible.
	calls := filepath.Join(t.TempDir(), "dolt-calls")
	binDir := t.TempDir()
	script := "#!/usr/bin/env bash\necho \"$*\" >> \"$DOLT_CALLS\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOLT_CALLS", calls)

	err := SetupDoltHubRemote(t.TempDir(), "example-org", "gt", "token")
	if err == nil {
		t.Fatal("SetupDoltHubRemote succeeded although DoltHub pushes are refused")
	}
	if !strings.Contains(err.Error(), "DoltHub remotes are not allowed for town databases") {
		t.Errorf("error should say DoltHub remotes are not allowed for town databases, got: %v", err)
	}
	if apiCalls != 0 {
		t.Errorf("DoltHub API was called %d time(s); no repo may be created", apiCalls)
	}
	if data, _ := os.ReadFile(calls); len(data) != 0 {
		t.Errorf("dolt was invoked (a remote may have been added): %q", data)
	}
}
