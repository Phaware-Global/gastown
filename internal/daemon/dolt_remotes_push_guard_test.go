package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFakeDolt puts a fake `dolt` binary on PATH for the duration of the
// test. It answers `dolt sql -r csv -q "...SELECT url FROM dolt_remotes..."`
// with remoteURL, treats any query containing DOLT_PUSH as the real push
// (touching a "pushed" marker file in its working directory so the test can
// tell whether a push was attempted), and no-ops everything else (ADD,
// COMMIT, the staged-changes count check).
func installFakeDolt(t *testing.T, remoteURL string) {
	t.Helper()

	binDir := t.TempDir()
	script := `#!/usr/bin/env bash
set -euo pipefail
query=""
csv=0
prev=""
for a in "$@"; do
  if [[ "$prev" == "-q" ]]; then query="$a"; fi
  if [[ "$a" == "-r" ]]; then :; fi
  prev="$a"
done
for a in "$@"; do
  if [[ "$a" == "csv" ]]; then csv=1; fi
done

if [[ "$query" == *"SELECT url FROM dolt_remotes"* ]]; then
  echo "url"
  echo "` + remoteURL + `"
  exit 0
fi
if [[ "$query" == *"SELECT COUNT(*) FROM dolt_status"* ]]; then
  echo "count"
  echo "0"
  exit 0
fi
if [[ "$query" == *"DOLT_PUSH"* ]]; then
  touch pushed
  exit 0
fi
exit 0
`
	doltPath := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(doltPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath)
	_ = origPath
}

func TestPushDatabaseRefusesPublicRemote(t *testing.T) {
	installFakeDolt(t, "git+https://github.com/pub-owner/repo")

	origLookPath, origLookup := ghLookPath, ghVisibilityLookup
	defer func() { ghLookPath, ghVisibilityLookup = origLookPath, origLookup }()
	ghLookPath = func() error { return nil }
	ghVisibilityLookup = func(ctx context.Context, ownerRepo string) (string, error) {
		return "public", nil
	}

	dataDir := t.TempDir()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	err := d.pushDatabase(dataDir, "gt", "origin", "main")
	if err == nil {
		t.Fatal("expected pushDatabase to refuse a public remote, got nil error")
	}
	if !strings.Contains(err.Error(), "not a confirmed-private GitHub repo") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "pushed")); statErr == nil {
		t.Error("push was attempted despite the public-remote refusal")
	}
}

func TestPushDatabaseAllowsPrivateRemote(t *testing.T) {
	installFakeDolt(t, "git+https://github.com/priv-owner/repo")

	origLookPath, origLookup := ghLookPath, ghVisibilityLookup
	defer func() { ghLookPath, ghVisibilityLookup = origLookPath, origLookup }()
	ghLookPath = func() error { return nil }
	ghVisibilityLookup = func(ctx context.Context, ownerRepo string) (string, error) {
		return "private", nil
	}

	dataDir := t.TempDir()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	if err := d.pushDatabase(dataDir, "gt", "origin", "main"); err != nil {
		t.Fatalf("expected pushDatabase to succeed for a private remote, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "pushed")); statErr != nil {
		t.Error("expected a push to have been attempted for the private remote")
	}
}

func TestPushDatabaseStillRefusesTestDatabaseName(t *testing.T) {
	// Guard against regressing the pre-existing name-based refusal while
	// adding the destination check above it.
	installFakeDolt(t, "git+https://github.com/priv-owner/repo")

	origLookPath, origLookup := ghLookPath, ghVisibilityLookup
	defer func() { ghLookPath, ghVisibilityLookup = origLookPath, origLookup }()
	ghLookPath = func() error { return nil }
	ghVisibilityLookup = func(ctx context.Context, ownerRepo string) (string, error) {
		return "private", nil
	}

	dataDir := t.TempDir()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	err := d.pushDatabase(dataDir, "testdb_foo", "origin", "main")
	if err == nil {
		t.Fatal("expected pushDatabase to refuse a test-named database, got nil error")
	}
	if !strings.Contains(err.Error(), "looks like a test database") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "pushed")); statErr == nil {
		t.Error("push was attempted despite the test-database refusal")
	}
}
