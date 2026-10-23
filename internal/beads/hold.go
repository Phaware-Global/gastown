package beads

import "strings"

// HoldLabels mark a bead whose work an operator has put on hold. The refinery
// treats a held MR or source bead as a PR it must not merge, re-review or
// dispatch fixes for.
var HoldLabels = []string{"hold", "held", "on-hold"}

// HoldReason explains why issue is on hold, or returns "" when it is not: a
// hold label, or the deferred status. A nil issue is not held.
func HoldReason(issue *Issue) string {
	if issue == nil {
		return ""
	}
	for _, label := range issue.Labels {
		for _, hold := range HoldLabels {
			if strings.EqualFold(strings.TrimSpace(label), hold) {
				return "labeled " + hold
			}
		}
	}
	if strings.EqualFold(issue.Status, "deferred") {
		return "status deferred"
	}
	return ""
}
