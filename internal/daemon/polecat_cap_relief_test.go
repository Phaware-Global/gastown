package daemon

import (
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

func TestShouldReapForCapRelief(t *testing.T) {
	threshold := polecat.MinPolecatDirsPerRig - polecatDirCapReliefHeadroom // 30 - 5 = 25
	cases := []struct {
		dirCount int
		want     bool
	}{
		{0, false},
		{threshold - 1, false},              // just below → keep the pool for reuse
		{threshold, true},                   // at the relief threshold → reap
		{threshold + 1, true},               // over threshold
		{polecat.MinPolecatDirsPerRig, true}, // at the cap → definitely reap
	}
	for _, c := range cases {
		if got := shouldReapForCapRelief(c.dirCount); got != c.want {
			t.Errorf("shouldReapForCapRelief(%d) = %v, want %v", c.dirCount, got, c.want)
		}
	}
}
