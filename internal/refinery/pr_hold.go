package refinery

import (
	"errors"
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
//
// It fails closed: if draft status or a bead cannot be read, the error is
// returned and the caller must not proceed on a guess. A provider that cannot
// report draft status (ErrUnsupported or no PRDraftChecker) and a bead that no
// longer exists (the usual state of a source issue after gt done) are not holds.
func VerifyPRNotHeld(provider PRProvider, prNumber int, mrID string, show BeadShower) error {
	if checker, ok := provider.(PRDraftChecker); ok {
		draft, err := checker.IsDraft(prNumber)
		switch {
		case errors.Is(err, ErrUnsupported):
		case err != nil:
			return fmt.Errorf("checking whether PR #%d is a draft: %w", prNumber, err)
		case draft:
			return &PRHeldError{PRNumber: prNumber, Reason: "draft PR"}
		}
	}

	if mrID == "" || show == nil {
		return nil
	}
	mr, err := show(mrID)
	if errors.Is(err, beads.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading MR bead %s for the hold check on PR #%d: %w", mrID, prNumber, err)
	}
	if reason := beads.HoldReason(mr); reason != "" {
		return &PRHeldError{PRNumber: prNumber, Reason: fmt.Sprintf("MR bead %s %s", mrID, reason)}
	}

	fields := beads.ParseMRFields(mr)
	if fields == nil || fields.SourceIssue == "" {
		return nil
	}
	src, err := show(fields.SourceIssue)
	if errors.Is(err, beads.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading source issue %s for the hold check on PR #%d: %w", fields.SourceIssue, prNumber, err)
	}
	if reason := beads.HoldReason(src); reason != "" {
		return &PRHeldError{PRNumber: prNumber, Reason: fmt.Sprintf("source issue %s %s", fields.SourceIssue, reason)}
	}
	return nil
}
