//go:build !windows

package tmuxguard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMakePrivateDir(t *testing.T) {
	base := t.TempDir()
	good := filepath.Join(base, "good")
	for range 2 { // creating it, then accepting it
		if err := makePrivateDir(good); err != nil {
			t.Fatalf("makePrivateDir: %v", err)
		}
	}

	open := filepath.Join(base, "open")
	file := filepath.Join(base, "file")
	link := filepath.Join(base, "link")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{open, file, link} {
		if makePrivateDir(dir) == nil {
			t.Errorf("makePrivateDir accepted %s", filepath.Base(dir))
		}
	}
}
