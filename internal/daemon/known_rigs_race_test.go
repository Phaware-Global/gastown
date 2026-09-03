package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestKnownRigsCache_ConcurrentAccess exercises getKnownRigs and
// invalidateKnownRigsCache from concurrent goroutines, the shape introduced
// by dispatching runCheckpointDog to a goroutine while the heartbeat loop
// keeps invalidating the cache (PR #184 review). Run with -race: the
// unsynchronized version is a detector-reported data race on
// knownRigsCache/knownRigsCacheValid.
func TestKnownRigsCache_ConcurrentAccess(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rigsJSON := []byte(`{"rigs":{"alpha":{},"beta":{}}}`)
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), rigsJSON, 0o644); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}

	d := &Daemon{
		logger: log.New(io.Discard, "", 0),
		config: &Config{TownRoot: townRoot},
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				rigs := d.getKnownRigs()
				for _, r := range rigs {
					_ = r
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				d.invalidateKnownRigsCache()
			}
		}()
	}
	wg.Wait()

	if rigs := d.getKnownRigs(); len(rigs) != 2 {
		t.Fatalf("getKnownRigs = %v, want the 2 rigs from rigs.json", rigs)
	}
}
