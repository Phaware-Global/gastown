package polecat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

func TestFindIdlePolecatContextStopsWhenDone(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "polecats", "toast"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(&rig.Rig{Name: "testrig", Path: root}, git.NewGit(root), nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checks := 0
	_, err := m.FindIdlePolecatContext(ctx, func(int, int) { checks++ })
	if !errors.Is(err, context.Canceled) || checks != 0 {
		t.Errorf("err = %v after %d checks, want context.Canceled before any", err, checks)
	}
}
