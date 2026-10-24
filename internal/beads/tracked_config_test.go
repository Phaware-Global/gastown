package beads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bd reads sync.remote from .beads/config.yaml and, on startup, adds it as a
// Dolt remote for the town's database. The databases hold issues and mail, so
// a committed sync.remote silently re-creates a push destination (and can
// point at a public repo) no matter what is cleaned up locally. The tracked
// config must not carry one.
func TestTrackedBeadsConfigHasNoSyncRemote(t *testing.T) {
	path := filepath.Join("..", "..", ".beads", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tracked beads config: %v", err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "sync.remote:") {
			t.Errorf("%s:%d sets sync.remote; bd would create a Dolt remote from it: %s", path, i+1, strings.TrimSpace(line))
		}
	}
}
