package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/witness"
)

type fakeSpawnManager struct {
	scanDelay time.Duration // how long the idle scan takes
	idle      *polecat.Polecat
	healthErr error
	allocErr  error
	clonePath string
	release   chan struct{} // closed at cleanup to end a slow scan
}

func (f *fakeSpawnManager) CheckDoltHealth() error         { return f.healthErr }
func (f *fakeSpawnManager) CheckDoltServerCapacity() error { return nil }

// FindIdlePolecatContext ignores ctx, as a single slow bd call would.
func (f *fakeSpawnManager) FindIdlePolecatContext(_ context.Context, progress func(int, int)) (*polecat.Polecat, error) {
	if progress != nil {
		progress(1, 3)
	}
	select {
	case <-time.After(f.scanDelay):
	case <-f.release:
	}
	return f.idle, nil
}

func (f *fakeSpawnManager) ReuseIdlePolecat(string, polecat.AddOptions) (*polecat.Polecat, error) {
	return f.idle, nil
}

func (f *fakeSpawnManager) AllocateAndAdd(polecat.AddOptions) (string, *polecat.Polecat, error) {
	if f.allocErr != nil {
		return "", nil, f.allocErr
	}
	return "fresh", nil, nil
}

func (f *fakeSpawnManager) Get(name string) (*polecat.Polecat, error) {
	return &polecat.Polecat{Name: name, ClonePath: f.clonePath}, nil
}

func (f *fakeSpawnManager) Remove(string, bool) error { return nil }

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestSpawnPolecatForSlingScanBudgetAndRespawnCount(t *testing.T) {
	const slow = 10 * time.Second
	idle := &polecat.Polecat{Name: "idler"}
	noRoom := errors.New("no room")
	cases := []struct {
		name         string
		fake         fakeSpawnManager
		wantPolecat  string // "" means the spawn fails
		wantHint     bool
		wantOverrun  bool
		wantRespawns int
	}{
		{"slow scan falls back to a fresh polecat", fakeSpawnManager{scanDelay: slow, idle: idle}, "fresh", false, true, 1},
		{"slow scan and failed allocation", fakeSpawnManager{scanDelay: slow, allocErr: noRoom}, "", true, true, 0},
		{"quick scan and failed allocation", fakeSpawnManager{allocErr: noRoom}, "", false, false, 0},
		{"failed health check", fakeSpawnManager{healthErr: errors.New("dolt down")}, "", false, false, 0},
		{"fresh polecat", fakeSpawnManager{}, "fresh", false, false, 1},
		{"reused idle polecat", fakeSpawnManager{idle: idle}, "idler", false, false, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			town := t.TempDir()
			for rel, body := range map[string]string{
				"mayor/town.json":     `{"type":"town","version":2,"name":"test"}`,
				"mayor/rigs.json":     `{"version":1,"rigs":{"testrig":{"git_url":"file:///nowhere"}}}`,
				"testrig/config.json": `{"type":"rig","version":1,"name":"testrig"}`,
			} {
				path := filepath.Join(town, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(town)
			if msg, err := exec.Command("git", "init", "-q", filepath.Join(town, "clone")).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, msg)
			}

			fake := tc.fake
			fake.clonePath = filepath.Join(town, "clone")
			fake.release = make(chan struct{})
			out := &lockedBuffer{}
			oldNew, oldBudget, oldEvery, oldOut := newSlingPolecatManager, idleScanBudget, idleScanProgressEvery, slingProgressOut
			t.Cleanup(func() {
				close(fake.release)
				newSlingPolecatManager, idleScanBudget, idleScanProgressEvery, slingProgressOut = oldNew, oldBudget, oldEvery, oldOut
			})
			newSlingPolecatManager = func(*rig.Rig, *git.Git, *tmux.Tmux) slingPolecatManager { return &fake }
			idleScanBudget, idleScanProgressEvery, slingProgressOut = 300*time.Millisecond, 50*time.Millisecond, out

			start := time.Now()
			info, err := SpawnPolecatForSling("testrig", SlingSpawnOptions{
				TownRoot: town, HookBead: "te-1", BaseBranch: "main", SkipAdmission: true,
			})
			if elapsed := time.Since(start); elapsed >= slow {
				t.Errorf("spawn waited %v for the scan", elapsed)
			}

			switch {
			case tc.wantPolecat == "":
				if err == nil {
					t.Fatal("spawn succeeded, want an error")
				}
				if got := strings.Contains(err.Error(), "gt sling te-1 testrig/<polecat>"); got != tc.wantHint {
					t.Errorf("by-name hint = %v, want %v: %v", got, tc.wantHint, err)
				}
			case err != nil:
				t.Fatalf("SpawnPolecatForSling: %v", err)
			case info.PolecatName != tc.wantPolecat:
				t.Errorf("polecat = %q, want %q", info.PolecatName, tc.wantPolecat)
			}

			log := out.String()
			if got := strings.Contains(log, "allocating a fresh polecat"); got != tc.wantOverrun {
				t.Errorf("overrun notice = %v, want %v: %q", got, tc.wantOverrun, log)
			}
			if tc.wantOverrun && !strings.Contains(log, "checked 1 of 3") {
				t.Errorf("no progress while scanning: %q", log)
			}

			// The next recorded attempt tells how many came before it.
			if got := witness.RecordBeadRespawn(town, "te-1") - 1; got != tc.wantRespawns {
				t.Errorf("respawns recorded = %d, want %d", got, tc.wantRespawns)
			}
		})
	}
}
