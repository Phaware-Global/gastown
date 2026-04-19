package cmd

import "testing"

// TestParsePRNumberFromURL covers the URL → PR number helper used by
// gt done's no-merge+pr path to hand off a just-created PR to the
// refinery via an MR bead (G11 fix). If this classifier silently
// miscounts, the MR bead gets the wrong review_pr and the refinery
// can't pick up the right PR.
func TestParsePRNumberFromURL(t *testing.T) {
	type tc struct {
		name    string
		url     string
		want    int
		wantErr bool
	}
	tests := []tc{
		// Positives — standard shapes gh pr create emits.
		{"standard GitHub URL", "https://github.com/Phaware-Global/gastown/pull/10", 10, false},
		{"single-digit PR", "https://github.com/foo/bar/pull/1", 1, false},
		{"triple-digit PR", "https://github.com/foo/bar/pull/999", 999, false},
		{"trailing slash", "https://github.com/foo/bar/pull/42/", 42, false},
		{"trailing subpath", "https://github.com/foo/bar/pull/42/files", 42, false},
		{"with fragment", "https://github.com/foo/bar/pull/42#issuecomment-9999", 42, false},
		{"with query string", "https://github.com/foo/bar/pull/42?notification_referrer_id=xxx", 42, false},
		{"leading whitespace", "  https://github.com/foo/bar/pull/42  ", 42, false},

		// Negatives — things that must error, not silently parse wrong.
		{"empty string", "", 0, true},
		{"no /pull/ segment", "https://github.com/foo/bar/issues/10", 0, true},
		{"non-URL text", "not a url at all", 0, true},
		{"letters after /pull/", "https://github.com/foo/bar/pull/abc", 0, true},
		{"/pull/ but nothing after", "https://github.com/foo/bar/pull/", 0, true},
		{"zero PR number", "https://github.com/foo/bar/pull/0", 0, true},
		{"negative-looking PR", "https://github.com/foo/bar/pull/-1", 0, true},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			got, err := parsePRNumberFromURL(c.url)
			if c.wantErr {
				if err == nil {
					t.Errorf("parsePRNumberFromURL(%q) = %d, nil; want error", c.url, got)
				}
				return
			}
			if err != nil {
				t.Errorf("parsePRNumberFromURL(%q) unexpected error: %v", c.url, err)
				return
			}
			if got != c.want {
				t.Errorf("parsePRNumberFromURL(%q) = %d; want %d", c.url, got, c.want)
			}
		})
	}
}
