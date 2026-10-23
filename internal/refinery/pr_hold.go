package refinery

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
)

// PRDraftChecker is implemented by PRProviders that can say whether a PR is a
// draft. It is optional, like the other capabilities on PRProvider that not
// every provider has: a provider without it is treated as unable to tell.
type PRDraftChecker interface {
	IsDraft(prNumber int) (bool, error)
}

// BeadShower reads a bead by ID. A missing bead is beads.ErrNotFound.
type BeadShower func(id string) (*beads.Issue, error)

// PRHeldError reports that a PR is not to be merged, re-reviewed or fixed
// because it is a draft or an operator has put its MR or source bead on hold.
type PRHeldError struct {
	PRNumber int
	Reason   string
}

func (e *PRHeldError) Error() string {
	return fmt.Sprintf("PR #%d is on hold (%s); the refinery does not merge, re-review or dispatch fixes for it", e.PRNumber, e.Reason)
}

// VerifyPRNotHeld returns a *PRHeldError when prNumber is a draft or the MR
// bead mrID, or its source issue, is on hold. mrID and show may be empty/nil
// when the caller has no MR bead; only the draft check applies then.
func VerifyPRNotHeld(provider PRProvider, prNumber int, mrID string, show BeadShower) error {
	return nil
}
