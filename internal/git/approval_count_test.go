package git

import "testing"

// TestFoldLatestReviewStates_ExcludesConfiguredLogin exercises the
// pr_reviewer exclusion at the folding boundary — a review from
// excludeLogin must never contribute to the returned state map, and thus
// never contribute to GhPrApprovalCount's approval count.
func TestFoldLatestReviewStates_ExcludesConfiguredLogin(t *testing.T) {
	reviews := []ghReview{
		{Author: struct {
			Login string `json:"login"`
		}{Login: "bot-reviewer"}, State: "APPROVED"},
		{Author: struct {
			Login string `json:"login"`
		}{Login: "alice"}, State: "APPROVED"},
	}

	latest := foldLatestReviewStates(reviews, "bot-reviewer")

	if _, ok := latest["bot-reviewer"]; ok {
		t.Fatalf("excludeLogin's review must be dropped, got latest = %v", latest)
	}
	if state := latest["alice"]; state != "APPROVED" {
		t.Fatalf("expected alice's review to survive as APPROVED, got %q", state)
	}
	if len(latest) != 1 {
		t.Fatalf("expected exactly 1 surviving reviewer, got %d: %v", len(latest), latest)
	}
}

// TestFoldLatestReviewStates_ExcludeIsCaseInsensitive matches the
// case-insensitive comparison used by every sibling gate (e.g.
// strings.EqualFold at git.go:2112/2193/2249).
func TestFoldLatestReviewStates_ExcludeIsCaseInsensitive(t *testing.T) {
	reviews := []ghReview{
		{Author: struct {
			Login string `json:"login"`
		}{Login: "Bot-Reviewer"}, State: "APPROVED"},
	}

	latest := foldLatestReviewStates(reviews, "bot-reviewer")

	if len(latest) != 0 {
		t.Fatalf("expected case-insensitive exclusion to drop the review, got %v", latest)
	}
}

// TestFoldLatestReviewStates_ExcludeIsTrimmed guards the live bug this
// round's fix addresses: excludeLogin was ToLower'd but never TrimSpace'd,
// so a config value with a trailing space silently disabled the exclusion.
func TestFoldLatestReviewStates_ExcludeIsTrimmed(t *testing.T) {
	reviews := []ghReview{
		{Author: struct {
			Login string `json:"login"`
		}{Login: "bot-reviewer"}, State: "APPROVED"},
	}

	latest := foldLatestReviewStates(reviews, "bot-reviewer \n")

	if len(latest) != 0 {
		t.Fatalf("expected a whitespace-padded excludeLogin to still exclude, got %v", latest)
	}
}

// TestFoldLatestReviewStates_LatestStateWins confirms a later
// CHANGES_REQUESTED supersedes an earlier APPROVED from the same user, and
// that GhPrApprovalCount's downstream counting logic only counts users
// whose latest state is APPROVED.
func TestFoldLatestReviewStates_LatestStateWins(t *testing.T) {
	reviews := []ghReview{
		{Author: struct {
			Login string `json:"login"`
		}{Login: "alice"}, State: "APPROVED"},
		{Author: struct {
			Login string `json:"login"`
		}{Login: "alice"}, State: "CHANGES_REQUESTED"},
	}

	latest := foldLatestReviewStates(reviews, "")

	if state := latest["alice"]; state != "CHANGES_REQUESTED" {
		t.Fatalf("expected alice's latest state to be CHANGES_REQUESTED, got %q", state)
	}
}

// TestFoldLatestReviewStates_NoExclusionWhenEmpty ensures an empty
// excludeLogin (the "no exclusion configured" sentinel) never matches a
// real login.
func TestFoldLatestReviewStates_NoExclusionWhenEmpty(t *testing.T) {
	reviews := []ghReview{
		{Author: struct {
			Login string `json:"login"`
		}{Login: "alice"}, State: "APPROVED"},
	}

	latest := foldLatestReviewStates(reviews, "")

	if state := latest["alice"]; state != "APPROVED" {
		t.Fatalf("expected alice's review to survive with empty excludeLogin, got %q", state)
	}
}
