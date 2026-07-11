package daemon

import (
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

func TestShouldReapForCapRelief(t *testing.T) {
	floor := polecat.MinPolecatDirsPerRig // 30
	cases := []struct {
		name     string
		dirCount int
		cap      int
		want     bool
	}{
		// Floor cap (30): relief threshold is 30 - 5 = 25.
		{"floor: zero", 0, floor, false},
		{"floor: just below threshold", 24, floor, false},
		{"floor: at threshold", 25, floor, true},
		{"floor: at cap", 30, floor, true},
		// High-cap rig (max_polecats=100): threshold is 95 — a floor-based gate would
		// wrongly reap at 25 and loop; the parameterized cap must not.
		{"high cap: 25 dirs is well below its cap", 25, 100, false},
		{"high cap: just below its threshold", 94, 100, false},
		{"high cap: at its threshold", 95, 100, true},
	}
	for _, c := range cases {
		if got := shouldReapForCapRelief(c.dirCount, c.cap); got != c.want {
			t.Errorf("%s: shouldReapForCapRelief(%d, %d) = %v, want %v", c.name, c.dirCount, c.cap, got, c.want)
		}
	}
}
