// Package tmuxguard keeps test binaries off the tmux server they were started
// under. Agent panes export TMUX, which tmux follows whenever no socket is
// named, so a test run from a pane would otherwise act on the town's server.
//
// The tmux package calls Isolate when it is linked into a test binary, and
// Check before running tmux, which does nothing until Isolate has run. A test
// that execs tmux without linking that package calls Isolate itself.
package tmuxguard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
)

type guard struct {
	inherited string          // socket path in the TMUX this process started with
	names     map[string]bool // socket names of the town's server
}

var armed atomic.Pointer[guard]

// Isolate unsets the variables that lead to a running tmux server, gives this
// process a private TMUX_TMPDIR, and arms Check. It panics if it cannot, as
// tests must not run unisolated.
//
// The directory is the same for every run of a user's tests, so a later run
// can reach, and kill, a server that a crashed one left behind.
func Isolate() {
	arm()
	for _, name := range []string{"TMUX", "TMUX_PANE", "GT_TOWN_SOCKET", "GT_TMUX_SOCKET"} {
		_ = os.Unsetenv(name)
	}
	base := "/tmp" // socket paths are length-limited, and os.TempDir is long on macOS
	if runtime.GOOS == "windows" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, fmt.Sprintf("gt-test-tmux-%d", os.Getuid()))
	if err := makePrivateDir(dir); err != nil {
		panic(fmt.Sprintf("tmuxguard: making a private TMUX_TMPDIR: %v", err))
	}
	_ = os.Setenv("TMUX_TMPDIR", dir)
}

// makePrivateDir creates dir, or accepts an existing one that no other user
// can have planted or can write to: its parent is shared.
func makePrivateDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return checkOwnerOnly(dir, info)
}

// arm records which server is off limits, from the environment as it is now.
// What an earlier call recorded stays off limits: that call unset the variables
// it was read from.
func arm() {
	g := &guard{names: map[string]bool{}}
	g.inherited, _, _ = strings.Cut(os.Getenv("TMUX"), ",")
	if prev := armed.Load(); prev != nil {
		maps.Copy(g.names, prev.names)
		if g.inherited == "" {
			g.inherited = prev.inherited
		}
	}
	names := []string{os.Getenv("GT_TOWN_SOCKET"), os.Getenv("GT_TMUX_SOCKET")}
	if g.inherited != "" {
		names = append(names, filepath.Base(g.inherited))
	}
	cwd, _ := os.Getwd()
	for _, root := range []string{os.Getenv("GT_TOWN_ROOT"), os.Getenv("GT_ROOT"), enclosingTown(cwd)} {
		if root != "" {
			names = append(names, TownSocketName(root))
		}
	}
	for _, name := range names {
		// "default" and "auto" are not names of a particular town's server.
		if name != "" && name != "default" && name != "auto" {
			g.names[name] = true
		}
	}
	armed.Store(g)
}

// Check panics, failing the test binary, if tmux run with "-L socket" (no -L
// when socket is empty) is aimed at the town's server.
func Check(socket string) {
	g := armed.Load()
	if g == nil {
		return
	}
	if g.names[socket] {
		panic(fmt.Sprintf("tmuxguard: test would talk to the town's tmux socket %q", socket))
	}
	if current, _, _ := strings.Cut(os.Getenv("TMUX"), ","); socket == "" && g.inherited != "" && current == g.inherited {
		panic("tmuxguard: test would follow the inherited $TMUX to a running tmux server")
	}
}

func enclosingTown(dir string) string {
	// The outermost match: a rig inside a town can look like a town itself.
	var found string
	for dir != "" {
		if _, err := os.Stat(filepath.Join(dir, "mayor", "town.json")); err == nil {
			found = dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return found
}

var nonSocketChars = regexp.MustCompile(`[^a-z0-9-]+`)

// TownSocketName is the socket name the session package derives for a town. It
// is repeated here because that package imports tmux, which imports this one.
func TownSocketName(townRoot string) string {
	base := strings.Trim(nonSocketChars.ReplaceAllString(strings.ToLower(filepath.Base(townRoot)), "-"), "-")
	if base == "" {
		base = "default"
	}
	canonical, err := filepath.EvalSymlinks(townRoot)
	if err != nil {
		if canonical, err = filepath.Abs(townRoot); err != nil {
			canonical = townRoot
		}
	}
	sum := sha256.Sum256([]byte(canonical))
	return base + "-" + hex.EncodeToString(sum[:3])
}
