package hooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/steveyegge/gastown/internal/lock"
)

// Codex's interactive CLI stops at a "Trust this folder?" dialog the first
// time it opens a project, and an agent has no one to answer it. Trust is
// read only from the user's config.toml ([projects."<path>"] trust_level),
// never from a -c override, and is keyed by the project root Codex computes:
// the git toplevel, or the main checkout of a linked worktree. Polecats run
// in a new worktree per name, so every new polecat would stop at the dialog.

// EnsureCodexProjectTrust records workDir's project as trusted in Codex's
// config.toml so a Codex agent started there does not stop at the dialog.
// It adds every key Codex may look up for workDir (the directory, its git
// toplevel and, for a linked worktree, the main checkout, each as given and
// with symlinks resolved). A project a person marked untrusted is left alone.
func EnsureCodexProjectTrust(workDir string) error {
	home, err := codexHome()
	if err != nil {
		return err
	}
	return ensureCodexProjectTrustIn(filepath.Join(home, "config.toml"), codexTrustKeys(workDir))
}

func codexHome() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// codexTrustKeys lists the paths Codex may use as workDir's trust key.
func codexTrustKeys(workDir string) []string {
	var keys []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" {
			return
		}
		for _, k := range []string{filepath.Clean(p), resolvedPath(p)} {
			if k != "" && !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	if abs, err := filepath.Abs(workDir); err == nil {
		add(abs)
	}
	add(gitOutput(workDir, "rev-parse", "--show-toplevel"))
	// A linked worktree of a non-bare repo is trusted through its main
	// checkout, the directory holding the shared .git.
	if common := gitOutput(workDir, "rev-parse", "--path-format=absolute", "--git-common-dir"); filepath.Base(common) == ".git" {
		add(filepath.Dir(common))
	}
	return keys
}

func resolvedPath(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	return r
}

func gitOutput(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ensureCodexProjectTrustIn appends a trusted [projects] table for each key
// config.toml does not already list. Appending keeps the person's own
// formatting and comments. Agents start in parallel, and a duplicated table
// would make config.toml unparseable for every Codex session, so the
// read-check-append runs under a file lock.
func ensureCodexProjectTrustIn(configPath string, keys []string) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	unlock, err := lock.FlockAcquire(configPath + ".gt-trust.lock")
	if err != nil {
		return fmt.Errorf("locking Codex config: %w", err)
	}
	defer unlock()

	data, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading Codex config: %w", err)
	}
	var cfg struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
	}
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", configPath, err)
	}

	var add strings.Builder
	for _, key := range keys {
		if _, listed := cfg.Projects[key]; listed {
			continue
		}
		fmt.Fprintf(&add, "\n[projects.%s]\ntrust_level = \"trusted\"\n", tomlQuote(key))
	}
	if add.Len() == 0 {
		return nil
	}
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		data = append(data, '\n')
	}
	data = append(data, add.String()...)
	// Write the whole file back via rename so a reader never sees a
	// half-written table. Rename onto the link target, not the link, when
	// config.toml is a symlink, and keep the file's permissions.
	target, mode := configPath, os.FileMode(0o600)
	if r := resolvedPath(configPath); r != "" {
		target = r
	}
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
	}
	tmp := target + ".gt-trust.tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// tomlQuote returns s as a TOML basic string.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
