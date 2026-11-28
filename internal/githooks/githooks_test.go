//go:build !windows

package githooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallWritesOnlyTheGuard(t *testing.T) {
	town := t.TempDir()
	for range 2 {
		dir, err := Install(town)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("dir mode = %o, want 700", got)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != prePushName {
			t.Errorf("entries = %v, want only %s", entries, prePushName)
		}
	}
}

func TestUntrustedDirectoryIsRefused(t *testing.T) {
	chmod := func(t *testing.T, path string, mode os.FileMode) {
		t.Helper()
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	for name, spoil := range map[string]func(t *testing.T, dir string){
		"group writable dir":  func(t *testing.T, dir string) { chmod(t, dir, 0o720) },
		"world writable dir":  func(t *testing.T, dir string) { chmod(t, dir, 0o702) },
		"group writable hook": func(t *testing.T, dir string) { chmod(t, filepath.Join(dir, prePushName), 0o720) },
		"missing hook": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, prePushName)); err != nil {
				t.Fatal(err)
			}
		},
		"missing dir": func(t *testing.T, dir string) {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked dir": func(t *testing.T, dir string) {
			if err := os.Rename(dir, dir+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(dir+".real", dir); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, err := Install(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			spoil(t, dir)
			if err := Verify(dir); err == nil {
				t.Error("Verify accepted the directory")
			}
		})
	}

	if err := checkOwnerAndMode(1, 2, 0o700); err == nil {
		t.Error("another user's directory was accepted")
	}
}

// A refused directory must not leave git running with the repository's hooks,
// or with none.
func TestRefineryEnvFailsClosed(t *testing.T) {
	town := t.TempDir()
	dir, err := Install(town)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(town); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Errorf("Install error = %v, want a writable refusal", err)
	}
	for name, root := range map[string]string{"writable dir": town, "no town": ""} {
		env := RefinerySessionEnv(root)
		if len(env) != 1 || env["GIT_CONFIG_COUNT"] != blockedConfigCount {
			t.Errorf("%s: env = %v, want git blocked", name, env)
		}
	}
}
