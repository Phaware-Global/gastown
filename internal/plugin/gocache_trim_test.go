package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// goCacheFixture is a directory laid out like a Go build cache.
type goCacheFixture struct {
	t   *testing.T
	dir string
}

func newGoCacheFixture(t *testing.T) goCacheFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("run.sh needs a Unix shell")
	}
	f := goCacheFixture{t: t, dir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(f.dir, "README"),
		[]byte("This directory holds cached build artifacts from the Go build system.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// write creates a file of sizeMB that was last used age ago.
func (f goCacheFixture) write(name string, sizeMB int, age time.Duration) {
	f.t.Helper()
	path := filepath.Join(f.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, sizeMB<<20), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.age(name, age)
}

func (f goCacheFixture) age(name string, age time.Duration) {
	f.t.Helper()
	when := time.Now().Add(-age)
	if err := os.Chtimes(filepath.Join(f.dir, name), when, when); err != nil {
		f.t.Fatal(err)
	}
}

func (f goCacheFixture) assert(present bool, names ...string) {
	f.t.Helper()
	for _, name := range names {
		_, err := os.Stat(filepath.Join(f.dir, name))
		if got := err == nil; got != present {
			f.t.Errorf("%s present = %v, want %v", name, got, present)
		}
	}
}

// run executes the plugin against the fixture with a cap of capMB.
func (f goCacheFixture) run(capMB string) (string, error) {
	f.t.Helper()
	bin := f.t.TempDir()
	for name, script := range map[string]string{
		"bd": "#!/bin/sh\nexit 0\n",
		"go": "#!/bin/sh\necho \"$GOCACHE\"\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "run.sh")
	cmd.Dir = filepath.Join("..", "..", "plugins", "gocache-trim")
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GOCACHE="+f.dir,
		"GT_GOCACHE_CAP_MB="+capMB)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f goCacheFixture) mustRun(capMB string) string {
	f.t.Helper()
	out, err := f.run(capMB)
	if err != nil {
		f.t.Fatalf("run.sh: %v\n%s", err, out)
	}
	return out
}

func TestGoCacheTrim_UnderCapKeepsEverything(t *testing.T) {
	f := newGoCacheFixture(t)
	f.write("00/old-d", 2, 100*time.Hour)

	f.mustRun("10")

	f.assert(true, "00/old-d")
}

func TestGoCacheTrim_OverCapRemovesOnlyOldEntries(t *testing.T) {
	f := newGoCacheFixture(t)
	f.write("00/old-d", 4, 30*time.Hour)
	f.write("ff/old-a", 1, 30*time.Hour)
	f.write("0a/exe-d/tool", 1, 30*time.Hour)
	f.age("0a/exe-d", 30*time.Hour)
	f.write("00/recent-d", 1, 3*time.Hour)
	// Not cache entries, however old.
	f.write("trim.txt", 0, 30*time.Hour)
	f.write("00/other", 0, 30*time.Hour)
	f.write("fuzz/pkg/seed-d", 0, 30*time.Hour)

	f.mustRun("3")

	f.assert(false, "00/old-d", "ff/old-a", "0a/exe-d")
	// Removing the day-old entries is enough, so younger ones stay.
	f.assert(true, "00/recent-d", "trim.txt", "00/other", "fuzz/pkg/seed-d", "README")
}

func TestGoCacheTrim_NeverRemovesEntriesInUse(t *testing.T) {
	f := newGoCacheFixture(t)
	f.write("00/mid-d", 2, 3*time.Hour)
	// Go refreshes an entry's mtime at most hourly while a build uses it.
	f.write("00/live-d", 2, 90*time.Minute)

	out := f.mustRun("1")

	f.assert(false, "00/mid-d")
	f.assert(true, "00/live-d")
	if !strings.Contains(out, "still over cap") {
		t.Errorf("output does not report the cache is still over cap:\n%s", out)
	}
}

func TestGoCacheTrim_RefusesDirectoryThatIsNotAGoCache(t *testing.T) {
	f := newGoCacheFixture(t)
	if err := os.Remove(filepath.Join(f.dir, "README")); err != nil {
		t.Fatal(err)
	}
	f.write("00/old-d", 2, 100*time.Hour)

	if out, err := f.run("1"); err == nil {
		t.Errorf("run.sh succeeded on a directory without a Go cache README:\n%s", out)
	}
	f.assert(true, "00/old-d")
}
