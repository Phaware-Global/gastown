package cmd

import (
	"path/filepath"
	"testing"
)

// canonicalTempDir returns t.TempDir() with symlinks resolved.
//
// On macOS os.TempDir() lives under /var, a symlink to /private/var. Code under
// test that resolves its paths (os.Getwd after chdir, filepath.EvalSymlinks)
// reports the /private/var form, so a test that builds an expected path or a
// stub's BEADS_DIR from the raw t.TempDir() string compares unequal for reasons
// unrelated to the behavior it is checking.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(TempDir): %v", err)
	}
	return dir
}
