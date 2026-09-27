package daemon

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reaper"
)

func TestWispReaperInterval(t *testing.T) {
	// Default (now 1h after Dog-driven refactor)
	if got := wispReaperInterval(nil); got != defaultWispReaperInterval {
		t.Errorf("expected default %v, got %v", defaultWispReaperInterval, got)
	}

	// Custom
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:     true,
				IntervalStr: "2h",
			},
		},
	}
	if got := wispReaperInterval(config); got != 2*time.Hour {
		t.Errorf("expected 2h, got %v", got)
	}

	// Invalid falls back to default
	config.Patrols.WispReaper.IntervalStr = "nope"
	if got := wispReaperInterval(config); got != defaultWispReaperInterval {
		t.Errorf("expected default for invalid, got %v", got)
	}
}

func TestWispReaperMaxAge(t *testing.T) {
	if got := wispReaperMaxAge(nil); got != defaultWispMaxAge {
		t.Errorf("expected default %v, got %v", defaultWispMaxAge, got)
	}

	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:   true,
				MaxAgeStr: "48h",
			},
		},
	}
	if got := wispReaperMaxAge(config); got != 48*time.Hour {
		t.Errorf("expected 48h, got %v", got)
	}
}

func TestWispDeleteAge(t *testing.T) {
	if got := wispDeleteAge(nil); got != defaultWispDeleteAge {
		t.Errorf("expected default %v, got %v", defaultWispDeleteAge, got)
	}

	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:      true,
				DeleteAgeStr: "336h",
			},
		},
	}
	if got := wispDeleteAge(config); got != 14*24*time.Hour {
		t.Errorf("expected 336h, got %v", got)
	}
}

func TestDefaultReaperIntervalIsOneHour(t *testing.T) {
	// Verify the default changed from 30m to 1h per issue gt-caf7.
	if defaultWispReaperInterval != 1*time.Hour {
		t.Errorf("expected default interval 1h, got %v", defaultWispReaperInterval)
	}
}

func TestWispStaleIssueAge(t *testing.T) {
	if got := wispStaleIssueAge(nil); got != defaultStaleIssueAge {
		t.Errorf("expected default %v, got %v", defaultStaleIssueAge, got)
	}

	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:          true,
				StaleIssueAgeStr: "48h",
			},
		},
	}
	if got := wispStaleIssueAge(config); got != 48*time.Hour {
		t.Errorf("expected 48h, got %v", got)
	}

	// Invalid falls back to default
	config.Patrols.WispReaper.StaleIssueAgeStr = "nope"
	if got := wispStaleIssueAge(config); got != defaultStaleIssueAge {
		t.Errorf("expected default for invalid, got %v", got)
	}
}

// TestDefaultStaleIssueAgeIsThirtyDays guards against gt-73to recurring: the
// daemon's default MUST equal reaper.DefaultStaleIssueAge (720h/30d) — the
// same value documented by mol-dog-reaper.formula.toml and used by
// `gt reaper auto-close --stale-age`. It must never silently drift to a
// shorter value (e.g. the previous 168h/7d bug that auto-closed live issues).
func TestDefaultStaleIssueAgeIsThirtyDays(t *testing.T) {
	if defaultStaleIssueAge != reaper.DefaultStaleIssueAge {
		t.Errorf("defaultStaleIssueAge (%v) must equal reaper.DefaultStaleIssueAge (%v)",
			defaultStaleIssueAge, reaper.DefaultStaleIssueAge)
	}
	if defaultStaleIssueAge != 720*time.Hour {
		t.Errorf("expected default stale issue age 720h (30d), got %v", defaultStaleIssueAge)
	}
}

// TestReapWispsDispatchesThirtyDayStaleIssueAge verifies the dispatch vars
// sent to the Dog carry the 30-day default, not the old 7-day one (gt-73to).
func TestReapWispsDispatchesThirtyDayStaleIssueAge(t *testing.T) {
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{Enabled: true},
		},
	}
	got := wispStaleIssueAge(config)
	want := 720 * time.Hour
	if got != want {
		t.Errorf("expected dispatched stale_issue_age %v, got %v", want, got)
	}
}
