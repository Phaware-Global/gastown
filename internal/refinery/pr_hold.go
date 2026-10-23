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

// SourceIssueNotFoundError reports that the source issue recorded on an MR
// reads as not found. The merge is refused, but the usual cause is the MR
// itself: gt mq submit can record a source_issue guessed from the branch name
// that never existed. A misrouted lookup reads the same way, so the text names
// both the MR and the value to correct.
type SourceIssueNotFoundError struct {
	PRNumber    int
	MRID        string
	SourceIssue string
	Err         error
}

func (e *SourceIssueNotFoundError) Error() string {
	return fmt.Sprintf("source issue %q recorded in the source_issue field of MR %s reads as not found, "+
		"so the hold check on PR #%d cannot complete and the PR is not merged; "+
		"correct that MR's source_issue to a bead that exists (or clear it), "+
		"or check the beads routing if %q does exist",
		e.SourceIssue, e.MRID, e.PRNumber, e.SourceIssue)
}

func (e *SourceIssueNotFoundError) Unwrap() error { return e.Err }

// IsHoldRefusal reports whether err means the PR is held or cannot be
// cleared by the hold check (as opposed to the check itself failing to run).
func IsHoldRefusal(err error) bool {
	var held *PRHeldError
	var missing *SourceIssueNotFoundError
	return errors.As(err, &held) || errors.As(err, &missing)
}

// VerifyPRNotHeld returns a *PRHeldError when prNumber is a draft or the MR
// bead mrID, or its source issue, is on hold. mrID and show may be empty/nil
// when the caller has no MR bead; only the draft check applies then.
//
// It fails closed: if draft status or a bead cannot be read, the error is
// returned and the caller must not proceed on a guess. That includes an MR or
// source bead that reads as not found: gt done closes the source issue rather
// than deleting it, and a misrouted lookup (wrong database, embedded clone)
// reports "not found" too. A provider that cannot report draft status
// (ErrUnsupported or no PRDraftChecker) is not a hold.
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
		return &SourceIssueNotFoundError{PRNumber: prNumber, MRID: mrID, SourceIssue: fields.SourceIssue, Err: err}
	}
	if err != nil {
		return fmt.Errorf("reading source issue %s for the hold check on PR #%d: %w", fields.SourceIssue, prNumber, err)
	}
	if reason := beads.HoldReason(src); reason != "" {
		return &PRHeldError{PRNumber: prNumber, Reason: fmt.Sprintf("source issue %s %s", fields.SourceIssue, reason)}
	}
	return nil
}
