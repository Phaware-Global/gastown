package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/steveyegge/gastown/internal/lock"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func trustLevels(t *testing.T, path string) map[string]string {
	t.Helper()
	var cfg struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		t.Fatalf("config.toml no longer parses: %v", err)
	}
	out := map[string]string{}
	for k, v := range cfg.Projects {
		out[k] = v.TrustLevel
	}
	return out
}

// The keys cover the work dir, its toplevel and, for a linked worktree of a
// normal repo, the main checkout; a worktree of a bare repo is its own root.
func TestCodexTrustKeys(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	main := filepath.Join(base, "main")
	os.MkdirAll(filepath.Join(main, "sub"), 0o755)
	run(t, main, "init", "-q")
	run(t, main, "-c", "user.email=a@b", "-c", "user.name=n", "commit", "-q", "--allow-empty", "-m", "x")
	wt := filepath.Join(base, "polecats", "rust")
	run(t, main, "worktree", "add", "-q", wt)

	has := func(keys []string, want string) bool {
		for _, k := range keys {
			if k == want {
				return true
			}
		}
		return false
	}
	if keys := codexTrustKeys(filepath.Join(main, "sub")); !has(keys, main) || !has(keys, filepath.Join(main, "sub")) {
		t.Errorf("subdir keys %v, want the subdir and %s", keys, main)
	}
	if keys := codexTrustKeys(wt); !has(keys, wt) || !has(keys, main) {
		t.Errorf("worktree keys %v, want %s and its main checkout %s", keys, wt, main)
	}

	bare := filepath.Join(base, ".repo.git")
	run(t, base, "clone", "-q", "--bare", main, bare)
	bwt := filepath.Join(base, "refinery", "rig")
	run(t, bare, "worktree", "add", "-q", bwt)
	if keys := codexTrustKeys(bwt); !has(keys, bwt) || has(keys, base) {
		t.Errorf("bare-repo worktree keys %v, want %s and not the bare repo's parent", keys, bwt)
	}

	link := filepath.Join(base, "link")
	os.Symlink(main, link)
	if keys := codexTrustKeys(link); !has(keys, link) || !has(keys, main) {
		t.Errorf("symlinked dir keys %v, want both %s and %s", keys, link, main)
	}
}

// New keys are appended as trusted tables, existing content and listed
// projects (including one marked untrusted) are kept, and a second run adds
// nothing.
func TestEnsureCodexProjectTrustIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := "# my settings\nmodel = \"gpt-6.1-sol\"\n\n[projects.\"/keep\"]\ntrust_level = \"untrusted\"\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	keys := []string{"/keep", "/w/a", `/w/odd "quote\path`}
	if err := ensureCodexProjectTrustIn(path, keys); err != nil {
		t.Fatal(err)
	}
	got := trustLevels(t, path)
	if got["/keep"] != "untrusted" || got["/w/a"] != "trusted" || got[`/w/odd "quote\path`] != "trusted" {
		t.Errorf("trust levels = %v", got)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), orig) {
		t.Errorf("existing content was not kept:\n%s", data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 kept", info.Mode().Perm())
	}
	before := string(data)
	if err := ensureCodexProjectTrustIn(path, keys); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) != before {
		t.Errorf("second run changed the file:\n%s", after)
	}

	if err := os.WriteFile(path, []byte("not = [valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureCodexProjectTrustIn(path, keys); err == nil {
		t.Error("an unparseable config was rewritten instead of reported")
	}
}

// Parallel agent starts must not duplicate a table, which would make
// config.toml unparseable for every Codex session: the update waits for the
// lock, and parallel updates write each table once.
func TestEnsureCodexProjectTrustIn_Locked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	unlock, err := lock.FlockAcquire(path + ".gt-trust.lock")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ensureCodexProjectTrustIn(path, []string{"/w/a"}) }()
	select {
	case err := <-done:
		t.Fatalf("update ran while another process held the lock (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ensureCodexProjectTrustIn(path, []string{"/w/shared"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	data, _ := os.ReadFile(path)
	if n := strings.Count(string(data), `[projects."/w/shared"]`); n != 1 {
		t.Errorf("table written %d times:\n%s", n, data)
	}
	trustLevels(t, path)
}

// A config.toml that is a symlink is updated through the link.
func TestEnsureCodexProjectTrustIn_Symlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles-config.toml")
	os.WriteFile(real, []byte("model = \"x\"\n"), 0o644)
	link := filepath.Join(dir, "config.toml")
	os.Symlink(real, link)
	if err := ensureCodexProjectTrustIn(link, []string{"/w/a"}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(link); info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a file")
	}
	if trustLevels(t, real)["/w/a"] != "trusted" {
		t.Error("the link target was not updated")
	}
}
