//go:build !windows

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeAged creates a file under root and backdates it and every directory up
// to root, so the entry looks idle for age.
func writeAged(t *testing.T, root, rel string, age time.Duration) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	for p := path; p != root; p = filepath.Dir(p) {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestGoTmpSweepSelection(t *testing.T) {
	const old, young = 3 * time.Hour, 10 * time.Minute
	tests := []struct {
		name    string
		rel     string // file created under the temp root
		age     time.Duration
		holder  string // a line from the live-process listing
		removed bool
	}{
		{name: "stale build dir", rel: "go-build123/b001/x.a", age: old, removed: true},
		{name: "stale link dir", rel: "go-link-456/go.o", age: old, removed: true},
		{name: "young build dir", rel: "go-build123/b001/x.a", age: young},
		{name: "other name", rel: "go-builder/x", age: old},
		{name: "suffix after digits", rel: "go-build123.keep/x", age: old},
		{name: "link without dash", rel: "go-link456/x", age: old},
		{name: "no digits", rel: "go-build/x", age: old},
		{name: "plain file with matching name", rel: "go-build123", age: old},
		{name: "nested match is below depth 1", rel: "other/go-build123/x", age: old},
		{name: "open file", rel: "go-build123/b001/x.test", age: old,
			holder: "n/private/var/T/go-build123/b001/x.test"},
		{name: "named on a command line", rel: "go-link-456/go.o", age: old,
			holder: "/go/pkg/tool/link -o /var/T/go-link-456/a.out"},
		{name: "working directory", rel: "go-build123/x", age: old, holder: "n/var/T/go-build123"},
		{name: "holder of a longer name", rel: "go-build123/x", age: old,
			holder: "n/var/T/go-build1234/x", removed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeAged(t, root, tt.rel, tt.age)
			s := &goTmpSweeper{
				maxAge:  DefaultGoTmpMaxAge,
				holders: func(context.Context) (string, error) { return tt.holder, nil },
				logf:    func(string, ...interface{}) {},
			}
			s.sweep(context.Background(), root)
			if got := !exists(filepath.Join(root, tt.rel)); got != tt.removed {
				t.Errorf("removed = %v, want %v", got, tt.removed)
			}
		})
	}
}

// An old top directory with a recently written file deeper down is in use.
func TestGoTmpSweepAgeIsNewestEntry(t *testing.T) {
	root := t.TempDir()
	writeAged(t, root, "go-build123/b001/old.a", 3*time.Hour)
	fresh := filepath.Join(root, "go-build123/b001/old.a")
	now := time.Now()
	if err := os.Chtimes(fresh, now, now); err != nil {
		t.Fatal(err)
	}
	s := &goTmpSweeper{maxAge: DefaultGoTmpMaxAge,
		holders: func(context.Context) (string, error) { return "", nil },
		logf:    func(string, ...interface{}) {}}
	s.sweep(context.Background(), root)
	if !exists(fresh) {
		t.Error("removed a directory holding a recently written file")
	}
}

func TestGoTmpSweepNeverFollowsSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeAged(t, outside, "keep/precious", 3*time.Hour)
	writeAged(t, root, "go-build999/x", 3*time.Hour)
	// A matching name that is a symlink, and a symlink inside a stale dir.
	top := filepath.Join(root, "go-build123")
	if err := os.Symlink(filepath.Join(outside, "keep"), top); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "keep"), filepath.Join(root, "go-build999", "link")); err != nil {
		t.Fatal(err)
	}
	s := &goTmpSweeper{maxAge: time.Nanosecond,
		holders: func(context.Context) (string, error) { return "", nil },
		logf:    func(string, ...interface{}) {}}
	s.sweep(context.Background(), root)
	if !exists(top) {
		t.Error("removed a symlink that matched the name pattern")
	}
	if exists(filepath.Join(root, "go-build999")) {
		t.Error("stale directory containing a symlink was not removed")
	}
	if !exists(filepath.Join(outside, "keep", "precious")) {
		t.Error("removed a file through a symlink")
	}
}

func TestGoTmpSweepKeepsEverythingWithoutAHolderListing(t *testing.T) {
	root := t.TempDir()
	writeAged(t, root, "go-build123/x", 3*time.Hour)
	var logged []string
	s := &goTmpSweeper{maxAge: DefaultGoTmpMaxAge,
		holders: func(context.Context) (string, error) { return "", errors.New("lsof failed") },
		logf:    func(f string, a ...interface{}) { logged = append(logged, fmt.Sprintf(f, a...)) }}
	s.sweep(context.Background(), root)
	if !exists(filepath.Join(root, "go-build123/x")) {
		t.Error("removed a directory without knowing who holds it")
	}
	if !strings.Contains(strings.Join(logged, "\n"), "lsof failed") {
		t.Errorf("failure not logged: %q", logged)
	}
}

func TestGoTmpSweepLogsRemovalAndBytes(t *testing.T) {
	root := t.TempDir()
	writeAged(t, root, "go-build123/x", 3*time.Hour)
	var logged []string
	s := &goTmpSweeper{maxAge: DefaultGoTmpMaxAge,
		holders: func(context.Context) (string, error) { return "", nil },
		logf:    func(f string, a ...interface{}) { logged = append(logged, fmt.Sprintf(f, a...)) }}
	if n, freed := s.sweep(context.Background(), root); n != 1 || freed != 10 {
		t.Errorf("sweep = %d dirs, %d bytes; want 1, 10", n, freed)
	}
	out := strings.Join(logged, "\n")
	if !strings.Contains(out, "go-build123") || !strings.Contains(out, "10 B") {
		t.Errorf("log should name the directory and size: %q", out)
	}
}

func TestGoTmpRoot(t *testing.T) {
	private := t.TempDir()
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(private)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, tmpdir, userTmp, want string
	}{
		{name: "empty", tmpdir: ""},
		{name: "relative", tmpdir: "tmp"},
		{name: "missing", tmpdir: filepath.Join(private, "nope")},
		{name: "writable by others", tmpdir: shared},
		{name: "not the platform user temp dir", tmpdir: private, userTmp: shared},
		{name: "private dir", tmpdir: private, want: resolved},
		{name: "matches the platform user temp dir", tmpdir: private + "/", userTmp: private, want: resolved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := goTmpRoot(tt.tmpdir, tt.userTmp)
			if got != tt.want || (err == nil) != (tt.want != "") {
				t.Errorf("goTmpRoot(%q, %q) = %q, %v; want %q", tt.tmpdir, tt.userTmp, got, err, tt.want)
			}
		})
	}
}

// The real listing must see a file this process holds open.
func TestGoTmpHoldersSeesOpenFile(t *testing.T) {
	if _, err := lsofPath(); err != nil {
		t.Skip("lsof not available")
	}
	root := t.TempDir()
	writeAged(t, root, "go-build7777001/x", 3*time.Hour)
	f, err := os.Open(filepath.Join(root, "go-build7777001/x"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// A failed listing also keeps the directory, so check the listing itself.
	listing, err := goTmpHolders(context.Background())
	if err != nil {
		t.Fatalf("goTmpHolders: %v", err)
	}
	if !strings.Contains(listing, "go-build7777001") {
		t.Fatal("listing does not name the directory with an open file")
	}
	s := &goTmpSweeper{maxAge: DefaultGoTmpMaxAge, holders: goTmpHolders,
		logf: t.Logf}
	s.sweep(context.Background(), root)
	if !exists(f.Name()) {
		t.Error("removed a directory with an open file")
	}
}

func TestGoTmpSweepPatrolConfig(t *testing.T) {
	off := false
	tests := []struct {
		name    string
		cfg     *GoTmpSweepConfig
		enabled bool
		maxAge  time.Duration
	}{
		{"unset", nil, true, DefaultGoTmpMaxAge},
		{"age only", &GoTmpSweepConfig{MaxAge: "6h"}, true, 6 * time.Hour},
		{"bad age", &GoTmpSweepConfig{MaxAge: "-1h"}, true, DefaultGoTmpMaxAge},
		{"disabled", &GoTmpSweepConfig{Enabled: &off}, false, DefaultGoTmpMaxAge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{GoTmpSweep: tt.cfg}}
			if got := IsPatrolEnabled(cfg, "go_tmp_sweep"); got != tt.enabled {
				t.Errorf("enabled = %v, want %v", got, tt.enabled)
			}
			if got := tt.cfg.maxAge(); got != tt.maxAge {
				t.Errorf("maxAge = %v, want %v", got, tt.maxAge)
			}
		})
	}
}
