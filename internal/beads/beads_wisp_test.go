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

func TestFilterWisps(t *testing.T) {
	ws := []*Issue{
		{ID: "w1", Status: "hooked", Assignee: "gastown/witness", Priority: 0},
		{ID: "w2", Status: "open", Assignee: "gastown/refinery", Priority: 1},
		{ID: "w3", Status: "closed", Assignee: "gastown/witness", Priority: 2},
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
	got := filterWisps(ws, "gastown/witness", -1, "")
	if len(got) != 2 || ids(got)[0] != "w1" || ids(got)[1] != "w3" {
		t.Errorf("assignee filter = %v, want [w1 w3]", ids(got))
	}
	// priority filter
	if got := filterWisps(ws, "", 1, ""); len(got) != 1 || got[0].ID != "w2" {
		t.Errorf("priority filter = %v, want [w2]", ids(got))
	}
	// status filter (comma-separated, case-insensitive)
	if got := filterWisps(ws, "", -1, "OPEN,closed"); len(got) != 2 {
		t.Errorf("status filter = %v, want [w2 w3]", ids(got))
	}
	// "all" and "" match every (non-nil) wisp
	if got := filterWisps(ws, "", -1, "all"); len(got) != 3 {
		t.Errorf(`status "all" = %v, want 3`, ids(got))
	}
	if got := filterWisps(ws, "", -1, ""); len(got) != 3 {
		t.Errorf(`status "" = %v, want 3`, ids(got))
	}
}
