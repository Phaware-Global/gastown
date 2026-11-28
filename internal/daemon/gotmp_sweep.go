package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// go build and go test write their work directories (go-build, go-link-) to
// the temp directory and remove them only on a normal exit, so each killed run
// leaves one behind, often hundreds of MB. The build cache trim does not reach
// them.

const (
	// DefaultGoTmpMaxAge is how long a work directory must go unwritten before
	// it can be removed.
	DefaultGoTmpMaxAge = 2 * time.Hour

	// goTmpSweepInterval is the least time between sweeps.
	goTmpSweepInterval = 30 * time.Minute

	// goTmpListTimeout bounds each live-process listing command.
	goTmpListTimeout = time.Minute
)

var (
	// goTmpEntryName matches a whole work directory name.
	goTmpEntryName = regexp.MustCompile(`^(go-build|go-link-)[0-9]+$`)
	// goTmpNameInText finds work directory names in a process listing.
	goTmpNameInText = regexp.MustCompile(`(go-build|go-link-)[0-9]+`)
)

// GoTmpSweepConfig holds configuration for the go_tmp_sweep patrol.
type GoTmpSweepConfig struct {
	// Enabled controls whether the sweep runs. Nil means enabled.
	Enabled *bool `json:"enabled,omitempty"`

	// MaxAge is a Go duration. Empty, unparsable or non-positive values mean
	// DefaultGoTmpMaxAge.
	MaxAge string `json:"max_age,omitempty"`
}

func (c *GoTmpSweepConfig) maxAge() time.Duration {
	if c != nil {
		if d, err := time.ParseDuration(c.MaxAge); err == nil && d > 0 {
			return d
		}
	}
	return DefaultGoTmpMaxAge
}

// goTmpRoot returns the resolved directory to sweep, or an error when tmpdir
// is not this user's own temp directory. userTmp is the platform's per-user
// temp directory, empty where there is no such thing.
func goTmpRoot(tmpdir, userTmp string) (string, error) {
	if !filepath.IsAbs(tmpdir) {
		return "", fmt.Errorf("TMPDIR %q is not an absolute path", tmpdir)
	}
	root, err := filepath.EvalSymlinks(tmpdir)
	if err != nil {
		return "", fmt.Errorf("TMPDIR: %w", err)
	}
	if userTmp != "" {
		want, err := filepath.EvalSymlinks(userTmp)
		if err != nil || want != root {
			return "", fmt.Errorf("TMPDIR %s is not the user temp directory %s", root, userTmp)
		}
	}
	if err := checkPrivateDir(root); err != nil {
		return "", err
	}
	return root, nil
}

// goTmpSweeper removes stale Go work directories. The process listing is
// injected so tests control what counts as in use.
type goTmpSweeper struct {
	maxAge time.Duration
	// holders returns text naming every path live processes have open or
	// mention on their command lines.
	holders func(context.Context) (string, error)
	logf    func(format string, args ...interface{})
}

type goTmpDir struct {
	name  string
	bytes int64
	idle  time.Duration
}

// stale returns the work directories directly under root that nothing has
// written to for maxAge. Symlinks are neither selected nor followed.
func (s *goTmpSweeper) stale(root string) []goTmpDir {
	entries, err := os.ReadDir(root)
	if err != nil {
		s.logf("go_tmp_sweep: cannot read %s: %v", root, err)
		return nil
	}
	var found []goTmpDir
	for _, e := range entries {
		// ReadDir reports a symlink as a symlink, so IsDir excludes it.
		if !e.IsDir() || !goTmpEntryName.MatchString(e.Name()) {
			continue
		}
		var size int64
		var newest time.Time
		err := filepath.WalkDir(filepath.Join(root, e.Name()), func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			if info.Mode().IsRegular() {
				size += info.Size()
			}
			return nil
		})
		if err != nil {
			// An entry that vanished or cannot be read has an unknown age.
			if !errors.Is(err, fs.ErrNotExist) {
				s.logf("go_tmp_sweep: keeping %s: %v", e.Name(), err)
			}
			continue
		}
		if idle := time.Since(newest); idle >= s.maxAge {
			found = append(found, goTmpDir{name: e.Name(), bytes: size, idle: idle})
		}
	}
	return found
}

// sweep removes the stale work directories under root that no live process
// holds, and returns how many it removed and the bytes they held. root must
// come from goTmpRoot.
func (s *goTmpSweeper) sweep(ctx context.Context, root string) (removed int, freed int64) {
	candidates := s.stale(root)
	if len(candidates) == 0 {
		return 0, 0
	}
	// Listed after the age scan, so the answer is as fresh as it can be.
	listing, err := s.holders(ctx)
	if err != nil {
		s.logf("go_tmp_sweep: removing nothing, cannot tell which directories are in use: %v", err)
		return 0, 0
	}
	held := map[string]bool{}
	for _, name := range goTmpNameInText.FindAllString(listing, -1) {
		held[name] = true
	}
	for _, c := range candidates {
		if ctx.Err() != nil {
			break
		}
		if held[c.name] {
			continue
		}
		path := filepath.Join(root, c.name)
		// RemoveAll unlinks symlinks without following them.
		if err := os.RemoveAll(path); err != nil {
			s.logf("go_tmp_sweep: cannot remove %s: %v", path, err)
			continue
		}
		removed++
		freed += c.bytes
		s.logf("go_tmp_sweep: removed %s (%s, idle %s)",
			path, util.FormatBytesHuman(uint64(c.bytes)), c.idle.Round(time.Minute))
	}
	if removed > 0 {
		s.logf("go_tmp_sweep: removed %d directories, freed %s", removed, util.FormatBytesHuman(uint64(freed)))
	}
	return removed, freed
}

// platformUserTmp returns the OS-assigned per-user temp directory on macOS and
// "" on platforms without one.
func platformUserTmp(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", nil
	}
	out, err := listCommand(ctx, "/usr/bin/getconf", "DARWIN_USER_TEMP_DIR")
	dir := strings.TrimSpace(out)
	if err != nil || dir == "" {
		return "", fmt.Errorf("cannot find the user temp directory: %v", err)
	}
	return dir, nil
}

// maybeSweepGoTmp is the heartbeat hook. The sweep walks gigabytes, so it runs
// in the background, one at a time.
func (d *Daemon) maybeSweepGoTmp() {
	if time.Since(d.lastGoTmpSweep) < goTmpSweepInterval || !d.goTmpSweepRunning.CompareAndSwap(false, true) {
		return
	}
	d.lastGoTmpSweep = time.Now()
	var cfg *GoTmpSweepConfig
	if d.patrolConfig != nil && d.patrolConfig.Patrols != nil {
		cfg = d.patrolConfig.Patrols.GoTmpSweep
	}
	s := &goTmpSweeper{maxAge: cfg.maxAge(), holders: goTmpHolders, logf: d.logger.Printf}
	go func() {
		defer d.goTmpSweepRunning.Store(false)
		userTmp, err := platformUserTmp(d.ctx)
		if err == nil {
			var root string
			if root, err = goTmpRoot(os.Getenv("TMPDIR"), userTmp); err == nil {
				s.sweep(d.ctx, root)
				return
			}
		}
		s.logf("go_tmp_sweep: skipped: %v", err)
	}()
}
