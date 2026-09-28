package git

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// shimGhReviews puts a fake `gh` first on PATH that answers
// `gh pr view N --json reviews` with reviewsJSON.
func shimGhReviews(t *testing.T, reviewsJSON string) {
	t.Helper()
	dir := t.TempDir()
	payload := filepath.Join(dir, "reviews.json")
	if err := os.WriteFile(payload, []byte(reviewsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncat '" + payload + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestGhPrApprovedReviewersAtSHA(t *testing.T) {
	const head = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const old = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	tests := []struct {
		name    string
		reviews string
		want    []string
	}{
		{
			name: "approval on an older commit does not count",
			reviews: `{"reviews":[
				{"author":{"login":"alice"},"commit":{"oid":"` + old + `"},"state":"APPROVED","submittedAt":"2026-09-01T00:00:00Z"}]}`,
			want: []string{},
		},
		{
			name: "approval at head counts",
			reviews: `{"reviews":[
				{"author":{"login":"Alice"},"commit":{"oid":"` + head + `"},"state":"APPROVED","submittedAt":"2026-09-01T00:00:00Z"}]}`,
			want: []string{"Alice"},
		},
		{
			name: "later comment at head does not re-anchor an old approval",
			reviews: `{"reviews":[
				{"author":{"login":"alice"},"commit":{"oid":"` + old + `"},"state":"APPROVED","submittedAt":"2026-09-01T00:00:00Z"},
				{"author":{"login":"alice"},"commit":{"oid":"` + head + `"},"state":"COMMENTED","submittedAt":"2026-09-02T00:00:00Z"}]}`,
			want: []string{},
		},
		{
			name: "later changes-requested at head removes the approval",
			reviews: `{"reviews":[
				{"author":{"login":"alice"},"commit":{"oid":"` + head + `"},"state":"APPROVED","submittedAt":"2026-09-01T00:00:00Z"},
				{"author":{"login":"alice"},"commit":{"oid":"` + head + `"},"state":"CHANGES_REQUESTED","submittedAt":"2026-09-02T00:00:00Z"}]}`,
			want: []string{},
		},
		{
			name: "re-approval at head after an old approval counts",
			reviews: `{"reviews":[
				{"author":{"login":"alice"},"commit":{"oid":"` + old + `"},"state":"APPROVED","submittedAt":"2026-09-01T00:00:00Z"},
				{"author":{"login":"alice"},"commit":{"oid":"` + head + `"},"state":"APPROVED","submittedAt":"2026-09-02T00:00:00Z"}]}`,
			want: []string{"alice"},
		},
		{
			name: "result is sorted case-insensitively and excludes other logins' stale approvals",
			reviews: `{"reviews":[
				{"author":{"login":"bob"},"commit":{"oid":"` + head + `"},"state":"APPROVED","submittedAt":"2026-09-01T00:00:00Z"},
				{"author":{"login":"Alice"},"commit":{"oid":"` + head + `"},"state":"APPROVED","submittedAt":"2026-09-01T00:00:01Z"},
				{"author":{"login":"carol"},"commit":{"oid":"` + old + `"},"state":"APPROVED","submittedAt":"2026-09-01T00:00:02Z"}]}`,
			want: []string{"Alice", "bob"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shimGhReviews(t, tc.reviews)
			g := NewGit(t.TempDir())
			got, err := g.GhPrApprovedReviewersAtSHA(1, head)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGhPrApprovedReviewersAtSHA_EmptySHAIsAnError(t *testing.T) {
	// "" must not mean "any commit" — that is the stale-approval bypass.
	shimGhReviews(t, `{"reviews":[]}`)
	if _, err := NewGit(t.TempDir()).GhPrApprovedReviewersAtSHA(1, " "); err == nil {
		t.Fatal("empty sha must be rejected")
	}
}
