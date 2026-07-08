package beads

import "testing"

func TestIsSQLSafeID(t *testing.T) {
	safe := []string{"gt-wisp-c9j", "hq-abc123", "heartworks_android-x1", "A-B_9"}
	for _, s := range safe {
		if !isSQLSafeID(s) {
			t.Errorf("isSQLSafeID(%q) = false, want true", s)
		}
	}
	// Empty and anything with a quote, backslash, space, or injection punctuation
	// must be rejected — these are the values that would break out of the SQL literal.
	unsafe := []string{"", "a'", `a\`, "a b", `\' OR 1=1 -- `, "x';DROP TABLE wisps;--", "a\tb"}
	for _, s := range unsafe {
		if isSQLSafeID(s) {
			t.Errorf("isSQLSafeID(%q) = true, want false (injection surface)", s)
		}
	}
}

func TestIsWispID(t *testing.T) {
	for _, id := range []string{"gt-wisp-c9j", "hq-wisp-001le", "heartworks_android-wisp-x"} {
		if !isWispID(id) {
			t.Errorf("isWispID(%q) = false, want true", id)
		}
	}
	// Ordinary issue ids must NOT trigger the child-wisp augmentation.
	for _, id := range []string{"gt-abc", "hq-123", "hga-y3jm", "", "wisp", "gtwisp"} {
		if isWispID(id) {
			t.Errorf("isWispID(%q) = true, want false", id)
		}
	}
}

func TestFilterWisps(t *testing.T) {
	ws := []*Issue{
		{ID: "w1", Status: "hooked", Assignee: "gastown/witness", Priority: 0},
		{ID: "w2", Status: "open", Assignee: "gastown/refinery", Priority: 1},
		{ID: "w3", Status: "closed", Assignee: "gastown/witness", Priority: 2},
		{ID: "w4", Status: "open", Assignee: "", Priority: 1},
		nil,
	}
	ids := func(in []*Issue) []string {
		out := make([]string, 0, len(in))
		for _, w := range in {
			out = append(out, w.ID)
		}
		return out
	}

	// assignee filter
	got := filterWisps(ws, "gastown/witness", false, -1, "")
	if len(got) != 2 || ids(got)[0] != "w1" || ids(got)[1] != "w3" {
		t.Errorf("assignee filter = %v, want [w1 w3]", ids(got))
	}
	// no-assignee filter: only the unassigned wisp, and it takes precedence over assignee
	if got := filterWisps(ws, "gastown/witness", true, -1, ""); len(got) != 1 || got[0].ID != "w4" {
		t.Errorf("no-assignee filter = %v, want [w4]", ids(got))
	}
	// priority filter
	if got := filterWisps(ws, "", false, 1, ""); len(got) != 2 {
		t.Errorf("priority filter = %v, want [w2 w4]", ids(got))
	}
	// status filter (comma-separated, case-insensitive)
	if got := filterWisps(ws, "", false, -1, "closed"); len(got) != 1 || got[0].ID != "w3" {
		t.Errorf("status filter = %v, want [w3]", ids(got))
	}
	// "all" and "" match every (non-nil) wisp
	if got := filterWisps(ws, "", false, -1, "all"); len(got) != 4 {
		t.Errorf(`status "all" = %v, want 4`, ids(got))
	}
	if got := filterWisps(ws, "", false, -1, ""); len(got) != 4 {
		t.Errorf(`status "" = %v, want 4`, ids(got))
	}
}
