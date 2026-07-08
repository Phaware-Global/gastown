package beads

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

// FormatSlingContextDescription serializes SlingContextFields as JSON.
// The context bead description is entirely scheduler-owned, so we use
// JSON instead of key-value lines — no user content collision, no delimiter.
func FormatSlingContextDescription(fields *capacity.SlingContextFields) string {
	b, err := json.Marshal(fields)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ParseSlingContextFields deserialises a context bead description.
// Returns nil if the description is not valid JSON.
func ParseSlingContextFields(description string) *capacity.SlingContextFields {
	var fields capacity.SlingContextFields
	if err := json.Unmarshal([]byte(description), &fields); err != nil {
		return nil
	}
	return &fields
}

// CreateSlingContext creates an ephemeral sling context bead that tracks
// scheduling state for a work bead. The work bead is never modified.
func (b *Beads) CreateSlingContext(workBeadTitle, workBeadID string, fields *capacity.SlingContextFields) (*Issue, error) {
	title := fmt.Sprintf("sling-context: %s", workBeadTitle)
	if len(title) > 200 {
		title = title[:200]
	}

	description := FormatSlingContextDescription(fields)

	args := []string{"create", "--json",
		"--ephemeral",
		"--title=" + title,
		"--description=" + description,
		"--type=task",
		"--labels=" + capacity.LabelSlingContext,
	}

	if actor := b.getActor(); actor != "" {
		args = append(args, "--actor="+actor)
	}

	out, err := b.run(args...)
	if err != nil {
		return nil, fmt.Errorf("creating sling context: %w", err)
	}

	var issue Issue
	if err := json.Unmarshal(out, &issue); err != nil {
		return nil, fmt.Errorf("parsing bd create output: %w", err)
	}

	// Add tracks dependency: context bead → work bead
	_, depErr := b.run("dep", "add", issue.ID, workBeadID, "--type=tracks")
	if depErr != nil {
		// Non-fatal: the context bead was created, just missing the dep link.
		// This can happen if the work bead is in a different DB and external refs aren't set up.
		fmt.Printf("Warning: could not add tracks dep %s → %s: %v\n", issue.ID, workBeadID, depErr)
	}

	return &issue, nil
}

// FindOpenSlingContext finds an open sling context for the given work bead ID.
// Used for idempotency checks. Returns (nil, nil, nil) if none found.
func (b *Beads) FindOpenSlingContext(workBeadID string) (*Issue, *capacity.SlingContextFields, error) {
	contexts, err := b.ListOpenSlingContexts()
	if err != nil {
		return nil, nil, err
	}

	for _, ctx := range contexts {
		fields := ParseSlingContextFields(ctx.Description)
		if fields != nil && fields.WorkBeadID == workBeadID {
			return ctx, fields, nil
		}
	}

	return nil, nil, nil
}

// ListOpenSlingContexts returns all open sling context beads.
//
// Sling contexts are created --ephemeral (see CreateSlingContext), so since beads
// v1.1.0 (schema v53) they live in the dedicated `wisps` table that `bd list`
// cannot see. Union the ephemeral contexts in from the wisps table — otherwise the
// scheduler can never read back the context it just wrote, so every work bead looks
// un-scheduled (convoys re-dispatch, capacity miscounts, idempotency breaks).
func (b *Beads) ListOpenSlingContexts() ([]*Issue, error) {
	out, err := b.run("list",
		"--label="+capacity.LabelSlingContext,
		"--status=open",
		"--json",
		"--limit=0",
	)
	if err != nil {
		return nil, err
	}

	// Handle empty output or non-JSON responses.
	// bd list --json may return plain text like "No issues found." instead
	// of an empty JSON array when there are no results.
	var issues []*Issue
	if len(out) > 0 && isJSONBytes(out) {
		if err := json.Unmarshal(out, &issues); err != nil {
			return nil, fmt.Errorf("parsing sling context list: %w", err)
		}
	}

	// Union ephemeral (wisp) sling contexts — the common case since v1.1.0. Propagate
	// a query failure instead of silently returning fewer contexts: this is symmetric
	// with the bd list path above, and a swallowed error here would look like "no
	// context" and re-introduce the re-dispatch/miscount blindness this fix removes.
	wisps, err := b.listOpenSlingContextWisps()
	if err != nil {
		return nil, fmt.Errorf("listing sling context wisps: %w", err)
	}
	if len(wisps) > 0 {
		seen := make(map[string]bool, len(issues))
		for _, is := range issues {
			if is != nil {
				seen[is.ID] = true
			}
		}
		for _, w := range wisps {
			if w != nil && !seen[w.ID] {
				issues = append(issues, w)
			}
		}
	}

	return issues, nil
}

// listOpenSlingContextWisps returns open sling-context wisps from the wisps table
// (beads v1.1.0+ stores ephemeral beads there, separate from issues). Errors are
// propagated (via wispRowsErr) so a transient Dolt failure is not mistaken for "no
// contexts"; a missing wisps table or no rows yields (nil, nil). The label predicate
// is a package constant (no caller input), so there is no injection surface.
func (b *Beads) listOpenSlingContextWisps() ([]*Issue, error) {
	return b.wispRowsErr("FROM wisps w LEFT JOIN wisp_labels al ON w.id = al.issue_id " +
		"WHERE w.status = 'open' AND EXISTS (" +
		"SELECT 1 FROM wisp_labels f WHERE f.issue_id = w.id AND f.label = '" + capacity.LabelSlingContext + "')")
}

// CloseSlingContext closes a sling context bead with a reason.
// Idempotent: suppresses "already closed" errors so retries are safe.
func (b *Beads) CloseSlingContext(contextID, reason string) error {
	_, err := b.run("close", contextID, "--reason="+reason)
	if err != nil && strings.Contains(err.Error(), "already closed") {
		return nil // Idempotent — already in desired state
	}
	return err
}

// UpdateSlingContextFields updates the description (fields) of a sling context bead.
func (b *Beads) UpdateSlingContextFields(contextID string, fields *capacity.SlingContextFields) error {
	description := FormatSlingContextDescription(fields)
	return b.Update(contextID, UpdateOptions{Description: &description})
}
