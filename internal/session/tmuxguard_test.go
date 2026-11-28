package session

import (
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/tmuxguard"
)

// The test guard repeats the socket naming; it is useless if the two drift.
func TestGuardNamesTheTownSocket(t *testing.T) {
	for _, base := range []string{"gt", "My Town!", "___"} {
		root := filepath.Join(t.TempDir(), base)
		if got, want := tmuxguard.TownSocketName(root), townSocketName(root); got != want {
			t.Errorf("TownSocketName(%q) = %q, want %q", base, got, want)
		}
	}
}
