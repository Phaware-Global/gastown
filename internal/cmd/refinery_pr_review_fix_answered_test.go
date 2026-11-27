package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/refinery"
)

// answeredProvider is a PR whose head, author, approvals and blockers a test
// sets directly. approvedAt maps an approver login to the commit it approved,
// and approvedOn is when those approvals were submitted.
type answeredProvider struct {
	refinery.PRProvider
	author     string
	head       string
	approvedAt map[string]string
	approvedOn time.Time
	blockers   []string
	err        error
}

func (p answeredProvider) CurrentHeadSHA(int) (string, error) { return p.head, p.err }
func (p answeredProvider) PRAuthor(int) (string, error)       { return p.author, nil }
func (p answeredProvider) HeadApprovalTime(_ int, user, sha string) (time.Time, error) {
	if p.approvedAt[user] != sha {
		return time.Time{}, nil
	}
	return p.approvedOn, nil
}
func (p answeredProvider) ChangesRequestedReviewers(int) ([]string, error) {
	return p.blockers, nil
}

var (
	repliedOn  = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	approvedOn = repliedOn.Add(time.Hour)
)

func ignoreEscalation(string, string, string) error { return nil }

func TestReviewFixAnsweredGate(t *testing.T) {
	approved := answeredProvider{author: "worker", head: "abc", approvedAt: map[string]string{"reviewer": "abc"}, approvedOn: approvedOn}
	answered := []refinery.ReviewThread{{ID: "t1", LastReplyAuthor: "worker", LastReplyAt: repliedOn}, {ID: "t2", LastReplyAuthor: "Worker", LastReplyAt: repliedOn}}
	late := []refinery.ReviewThread{answered[0], {ID: "t5", LastReplyAuthor: "worker", LastReplyAt: approvedOn.Add(time.Minute)}}
	undated := []refinery.ReviewThread{{ID: "t6", LastReplyAuthor: "worker"}}
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
		{"the reviewer replied last", approved, []refinery.ReviewThread{{ID: "t4", LastReplyAuthor: "reviewer", LastReplyAt: repliedOn}}, false},
		{"a reply follows the approval", approved, late, false},
		{"a reply has no timestamp", approved, undated, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := reviewFixAnsweredGate(tc.provider, cfg, holderState(), "gt-mr-1", tc.threads, ignoreEscalation)
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
	err := reviewFixAnsweredGate(p, &refinery.MergeQueueConfig{PRReviewer: "reviewer"}, holderState(), "gt-mr-1",
		[]refinery.ReviewThread{{ID: "t1", LastReplyAuthor: "worker"}}, ignoreEscalation)
	if code, ok := IsSilentExit(err); !ok || code != 4 {
		t.Fatalf("err = %v, want silent exit 4", err)
	}
}

// The answered check stops the dispatch before the holder query runs, and hands
// over to it otherwise.
func TestReviewFixDispatchGate(t *testing.T) {
	cfg := &refinery.MergeQueueConfig{PRReviewer: "reviewer"}
	p := answeredProvider{author: "worker", head: "abc", approvedAt: map[string]string{"reviewer": "abc"}, approvedOn: approvedOn}
	holderErr := errors.New("holder")
	holder := func() error { return holderErr }

	err := reviewFixDispatchGate(p, cfg, holderState(), "gt-mr-1",
		[]refinery.ReviewThread{{LastReplyAuthor: "worker", LastReplyAt: repliedOn}}, holder, ignoreEscalation)()
	if code, ok := IsSilentExit(err); !ok || code != 1 {
		t.Fatalf("answered: err = %v, want silent exit 1", err)
	}
	err = reviewFixDispatchGate(p, cfg, holderState(), "gt-mr-1", []refinery.ReviewThread{{}}, holder, ignoreEscalation)()
	if !errors.Is(err, holderErr) {
		t.Fatalf("unanswered: err = %v, want the holder gate's", err)
	}
}

// A skip dispatches nobody, so it has to reach someone: one escalation per MR
// and head, naming the threads to resolve.
func TestReviewFixAnsweredGate_Escalates(t *testing.T) {
	cfg := &refinery.MergeQueueConfig{PRReviewer: "reviewer"}
	p := answeredProvider{author: "worker", head: "abc", approvedAt: map[string]string{"reviewer": "abc"}, approvedOn: approvedOn}
	threads := []refinery.ReviewThread{{ID: "t1", URL: "https://example.test/t1", LastReplyAuthor: "worker", LastReplyAt: repliedOn}}

	var fingerprints []string
	var reason string
	record := func(_, r, fp string) error {
		reason = r
		fingerprints = append(fingerprints, fp)
		return nil
	}
	err := reviewFixAnsweredGate(p, cfg, holderState(), "gt-mr-1", threads, record)
	if code, ok := IsSilentExit(err); !ok || code != 1 {
		t.Fatalf("err = %v, want silent exit 1", err)
	}
	if len(fingerprints) != 1 || fingerprints[0] != "dispatch-review-fix:answered:gt-mr-1:abc" {
		t.Fatalf("fingerprints = %v", fingerprints)
	}
	if !strings.Contains(reason, "https://example.test/t1") {
		t.Fatalf("reason = %q, want the thread to resolve", reason)
	}

	failing := func(string, string, string) error { return errors.New("boom") }
	err = reviewFixAnsweredGate(p, cfg, holderState(), "gt-mr-1", threads, failing)
	if code, ok := IsSilentExit(err); !ok || code != 4 {
		t.Fatalf("failed escalation: err = %v, want silent exit 4", err)
	}
}
