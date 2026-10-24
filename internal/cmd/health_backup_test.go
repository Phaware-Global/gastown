package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newBackupTown returns a temp town root and points HOME at a separate temp dir
// so no test can read the live town or the real home directory.
func newBackupTown(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return t.TempDir()
}

// A symlinked .dolt-backup must be dated by the files inside it, not by the
// link's own mtime.
func TestCheckBackupHealth_DoltBackupSymlinkUsesTargetFiles(t *testing.T) {
	town := newBackupTown(t)
	target := filepath.Join(town, ".dolt-backup-local")
	if err := os.MkdirAll(filepath.Join(target, "hq"), 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(target, "hq", "manifest")
	if err := os.WriteFile(fresh, []byte("x"), 0o644); err != nil { //nolint:gosec // G306: test fixture
		t.Fatal(err)
	}
	link := filepath.Join(town, ".dolt-backup")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Age the link itself; Chtimes would follow it, so use touch -h.
	old := time.Now().Add(-233 * time.Hour).Format("200601021504")
	if out, err := exec.Command("touch", "-h", "-t", old, link).CombinedOutput(); err != nil {
		t.Skipf("touch -h unavailable: %v: %s", err, out)
	}

	bh := checkBackupHealth(town)
	if bh.DoltStale {
		t.Errorf("fresh backup reported stale (%s)", bh.DoltFreshness)
	}
	if bh.DoltFreshness == "" || bh.DoltAgeSeconds > 60 {
		t.Errorf("age = %q (%ds), want under a minute", bh.DoltFreshness, bh.DoltAgeSeconds)
	}
}

func TestFindNewestFile_FollowsSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	want := time.Now().Add(-90 * time.Minute).Truncate(time.Second)
	f := filepath.Join(target, "f")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil { //nolint:gosec // G306: test fixture
		t.Fatal(err)
	}
	if err := os.Chtimes(f, want, want); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := findNewestFile(link); !got.Equal(want) {
		t.Errorf("findNewestFile(symlink) = %v, want %v", got, want)
	}
}

func TestCheckBackupHealth_DoltBackupMissingOrEmptyIsExplicit(t *testing.T) {
	town := newBackupTown(t)
	bh := checkBackupHealth(town)
	if bh.DoltFreshness != "" || bh.DoltNote == "" {
		t.Errorf("missing dir: freshness=%q note=%q, want no age and a note", bh.DoltFreshness, bh.DoltNote)
	}

	if err := os.MkdirAll(filepath.Join(town, ".dolt-backup", "hq"), 0o755); err != nil {
		t.Fatal(err)
	}
	bh = checkBackupHealth(town)
	if bh.DoltFreshness != "" || bh.DoltNote == "" {
		t.Errorf("empty dir: freshness=%q note=%q, want no age and a note", bh.DoltFreshness, bh.DoltNote)
	}
}

// newArchiveTown returns a town whose dolt-archive repo has a single commit
// made the given duration ago.
func newArchiveTown(t *testing.T, ago time.Duration) string {
	t.Helper()
	town := newBackupTown(t)
	repo := filepath.Join(town, ".dolt-archive", "git")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "config", "user.email", "test@example.com")
	gitRun(t, repo, "config", "user.name", "Test")
	when := time.Now().Add(-ago).Format(time.RFC3339)
	cmd := exec.Command("git", "-C", repo, "commit", "-q", "--allow-empty", "-m", "export", "--date", when)
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+when)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	return town
}

// The dolt-archive plugin writes its git repo under the town root.
func TestCheckBackupHealth_JSONLArchiveUnderTownRoot(t *testing.T) {
	bh := checkBackupHealth(newArchiveTown(t, 2*time.Hour))
	if bh.JSONLFreshness == "" {
		t.Fatalf("archive under town root not found (note %q)", bh.JSONLNote)
	}
	if bh.JSONLAgeSeconds < 100*60 || bh.JSONLAgeSeconds > 140*60 {
		t.Errorf("age = %ds, want about 2h", bh.JSONLAgeSeconds)
	}
}

// dolt-archive runs on a 1h cooldown, so an export 70 minutes old is on
// schedule; only an archive that has missed several cycles is stale.
func TestCheckBackupHealth_JSONLStaleThreshold(t *testing.T) {
	cases := []struct {
		name      string
		ago       time.Duration
		wantStale bool
	}{
		{"on schedule at 70m", 70 * time.Minute, false},
		{"one missed cycle at 2h", 2 * time.Hour, false},
		{"several missed cycles at 3h", 3 * time.Hour, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bh := checkBackupHealth(newArchiveTown(t, c.ago))
			if bh.JSONLStale != c.wantStale {
				t.Errorf("JSONLStale = %v at %s, want %v", bh.JSONLStale, bh.JSONLFreshness, c.wantStale)
			}
		})
	}
}

// dolt-backup runs on a 15m cooldown, so its threshold stays at 30m.
func TestCheckBackupHealth_DoltStaleThreshold(t *testing.T) {
	cases := []struct {
		ago       time.Duration
		wantStale bool
	}{
		{20 * time.Minute, false},
		{45 * time.Minute, true},
	}
	for _, c := range cases {
		town := newBackupTown(t)
		dir := filepath.Join(town, ".dolt-backup")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(dir, "manifest")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil { //nolint:gosec // G306: test fixture
			t.Fatal(err)
		}
		when := time.Now().Add(-c.ago)
		if err := os.Chtimes(f, when, when); err != nil {
			t.Fatal(err)
		}
		if bh := checkBackupHealth(town); bh.DoltStale != c.wantStale {
			t.Errorf("DoltStale = %v at %s, want %v", bh.DoltStale, bh.DoltFreshness, c.wantStale)
		}
	}
}

func TestCheckBackupHealth_JSONLMissingIsExplicit(t *testing.T) {
	town := newBackupTown(t)
	bh := checkBackupHealth(town)
	if bh.JSONLFreshness != "" || bh.JSONLNote == "" {
		t.Errorf("missing archive: freshness=%q note=%q, want no age and a note", bh.JSONLFreshness, bh.JSONLNote)
	}
	if !strings.Contains(bh.JSONLNote, "not found") {
		t.Errorf("note = %q, want it to say not found", bh.JSONLNote)
	}
}
