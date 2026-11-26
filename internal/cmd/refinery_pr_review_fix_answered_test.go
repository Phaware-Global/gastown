package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery"
)

// answeredProvider is a PR whose head, author, approvals and blockers a test
// sets directly. approvedAt maps an approver login to the commit it approved.
type answeredProvider struct {
	refinery.PRProvider
	author     string
	head       string
	approvedAt map[string]string
	blockers   []string
	err        error
}

func (p answeredProvider) CurrentHeadSHA(int) (string, error) { return p.head, p.err }
func (p answeredProvider) PRAuthor(int) (string, error)       { return p.author, nil }
func (p answeredProvider) IsPRApprovedAtSHA(_ int, user, sha string) (bool, error) {
	return p.approvedAt[user] == sha, nil
}
func (p answeredProvider) ChangesRequestedReviewers(int) ([]string, error) {
	return p.blockers, nil
}

func TestReviewFixAnsweredGate(t *testing.T) {
	approved := answeredProvider{author: "worker", head: "abc", approvedAt: map[string]string{"reviewer": "abc"}}
	answered := []refinery.ReviewThread{{ID: "t1", LastReplyAuthor: "worker"}, {ID: "t2", LastReplyAuthor: "Worker"}}
	cfg := &refinery.MergeQueueConfig{PRReviewer: "reviewer", PRApprover: "human"}

	stale := approved
	stale.approvedAt = map[string]string{"reviewer": "old"}
	byApprover := approved
	byApprover.approvedAt = map[string]string{"human": "abc"}
	byStranger := approved
	byStranger.approvedAt = map[string]string{"passerby": "abc"}
	blocked := approved
	blocked.blockers = []string{"human"}

	cases := []struct {
		name     string
		provider refinery.PRProvider
		threads  []refinery.ReviewThread
		skip     bool
	}{
		{"approved head, every thread answered", approved, answered, true},
		{"approved by the human approver", byApprover, answered, true},
		{"approval is for an older head", stale, answered, false},
		{"approval is from an unconfigured login", byStranger, answered, false},
		{"a reviewer still requests changes", blocked, answered, false},
		{"one thread has no reply", approved, []refinery.ReviewThread{answered[0], {ID: "t3"}}, false},
		{"the reviewer replied last", approved, []refinery.ReviewThread{{ID: "t4", LastReplyAuthor: "reviewer"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := reviewFixAnsweredGate(tc.provider, cfg, holderState(), tc.threads)
			if !tc.skip {
				if err != nil {
					t.Fatalf("err = %v, want dispatch to proceed", err)
				}
				return
			}
			// Exit 1, never 0: the threads are still open, so the PR must not advance.
			if code, ok := IsSilentExit(err); !ok || code != 1 {
				t.Fatalf("err = %v, want silent exit 1", err)
			}
		})
	}
}

func TestReviewFixAnsweredGate_LookupFailureIsOperational(t *testing.T) {
	p := answeredProvider{author: "worker", err: errors.New("boom")}
	err := reviewFixAnsweredGate(p, &refinery.MergeQueueConfig{PRReviewer: "reviewer"}, holderState(),
		[]refinery.ReviewThread{{ID: "t1", LastReplyAuthor: "worker"}})
	if code, ok := IsSilentExit(err); !ok || code != 4 {
		t.Fatalf("err = %v, want silent exit 4", err)
	}
}

// The answered check stops the dispatch before the holder query runs, and hands
// over to it otherwise.
func TestReviewFixDispatchGate(t *testing.T) {
	cfg := &refinery.MergeQueueConfig{PRReviewer: "reviewer"}
	p := answeredProvider{author: "worker", head: "abc", approvedAt: map[string]string{"reviewer": "abc"}}
	holderErr := errors.New("holder")
	holder := func() error { return holderErr }

	err := reviewFixDispatchGate(p, cfg, holderState(), []refinery.ReviewThread{{LastReplyAuthor: "worker"}}, holder)()
	if code, ok := IsSilentExit(err); !ok || code != 1 {
		t.Fatalf("answered: err = %v, want silent exit 1", err)
	}
	err = reviewFixDispatchGate(p, cfg, holderState(), []refinery.ReviewThread{{}}, holder)()
	if !errors.Is(err, holderErr) {
		t.Fatalf("unanswered: err = %v, want the holder gate's", err)
	}
}
