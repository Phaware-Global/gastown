package beads

import (
	"strings"
	"testing"
)

// The variants are bd's own wording (store.go, doltserver.go); the first is the
// text the deacon received from `gt patrol new` before it ran `bd dolt start`.
func TestRedirectDoltStartHint(t *testing.T) {
	tests := []struct {
		name string
		in   string
		keep []string // fragments that must survive the rewrite
	}{
		{
			name: "auto-start disabled hint",
			in: "Error: failed to open database: Dolt server unreachable at 127.0.0.1:3307: dial tcp 127.0.0.1:3307: connect: connection refused\n\n" +
				"Dolt server auto-start is disabled (dolt.auto-start: false).\nStart the server manually:\n  bd dolt start",
			keep: []string{"Dolt server unreachable at 127.0.0.1:3307", "connection refused"},
		},
		{
			name: "server may not be running hint",
			in:   "Dolt server unreachable\n\nThe Dolt server may not be running. Try:\n  bd dolt start",
			keep: []string{"Dolt server unreachable"},
		},
		{
			name: "externally managed hint",
			in:   "auto-start is suppressed.\n  To start manually: bd dolt start\n  To check status: bd dolt status",
			keep: []string{"auto-start is suppressed.", "bd dolt status"},
		},
		{
			name: "orchestrator hint",
			in:   "failed to connect\n\nThe Dolt server may not be running. Try:\n  bd dolt start    # Start a local server\n  gt dolt start    # If using an orchestrator",
			keep: []string{"failed to connect"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedirectDoltStartHint(tt.in)
			if strings.Contains(got, "bd dolt start") {
				t.Errorf("output still advises bd dolt start:\n%s", got)
			}
			if strings.Count(got, DoltStartGuidance) != 1 {
				t.Errorf("want guidance exactly once:\n%s", got)
			}
			for _, k := range tt.keep {
				if !strings.Contains(got, k) {
					t.Errorf("output lost %q:\n%s", k, got)
				}
			}
			for _, intro := range []string{"Start the server manually:", "Dolt server may not be running. Try:"} {
				if strings.Contains(got, intro) {
					t.Errorf("output kept the lead-in %q:\n%s", intro, got)
				}
			}
		})
	}
}

func TestRedirectDoltStartHint_PassesOtherOutputThrough(t *testing.T) {
	in := "Error: issue gt-abc not found\nRun 'bd list' to see issues"
	if got := RedirectDoltStartHint(in); got != in {
		t.Errorf("output without the hint changed: %q", got)
	}
}

func TestWrapError_RedirectsDoltStartHint(t *testing.T) {
	stderr := "Error: failed to open database: Dolt server unreachable at 127.0.0.1:3307\n\n" +
		"Dolt server auto-start is disabled (dolt.auto-start: false).\nStart the server manually:\n  bd dolt start\n"
	err := New(t.TempDir()).wrapError(nil, stderr, []string{"list", "--json"})
	if err == nil {
		t.Fatal("wrapError returned nil")
	}
	if strings.Contains(err.Error(), "bd dolt start") {
		t.Errorf("wrapped error still advises bd dolt start: %v", err)
	}
	if !strings.Contains(err.Error(), "gt dolt status") {
		t.Errorf("wrapped error lacks the Gas Town guidance: %v", err)
	}
}
