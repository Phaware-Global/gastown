package refinery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunningAsTestBinary pins the detector used by defaultMailSender
// to choose between memory and exec impls. If the detector ever
// stops recognizing the `.test` suffix, every refinery test would
// silently revert to subprocess-forking `gt mail send` and the G10
// flood would resurface on the next `go test` run.
func TestRunningAsTestBinary(t *testing.T) {
	// We are, in fact, running as a test binary right now.
	if !runningAsTestBinary() {
		t.Errorf("runningAsTestBinary() = false in a test; expected true (os.Args[0]=%q)", os.Args[0])
	}

	// Extra coverage for path-prefixed and Windows forms, via a
	// standalone check that mirrors the same suffix rule. We can't
	// rewrite os.Args without racing other tests in this package, so
	// exercise the suffix check directly on the constants.
	for _, tc := range []struct {
		name string
		arg0 string
		want bool
	}{
		{"bare .test", "foo.test", true},
		{"path-prefixed .test", "/tmp/go-build/foo.test", true},
		{"Windows .test.exe", `C:\Users\x\go-build\foo.test.exe`, true},
		{"production binary", "gt", false},
		{"prod with path", "/usr/local/bin/gt", false},
		{"unrelated with 'test' in middle", "foo.testing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := filepath.Base(tc.arg0)
			got := strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe")
			if got != tc.want {
				t.Errorf("suffix check on %q (base=%q) = %v; want %v", tc.arg0, base, got, tc.want)
			}
		})
	}
}

// TestDefaultMailSender_TestBinaryReturnsMemory verifies the decision
// table: under a test binary, defaultMailSender returns a
// memoryMailSender, not an exec one. This is the load-bearing contract
// — if it regresses, the G10 flood comes back.
func TestDefaultMailSender_TestBinaryReturnsMemory(t *testing.T) {
	s := defaultMailSender("/tmp/some/workdir")
	if _, ok := s.(*memoryMailSender); !ok {
		t.Errorf("defaultMailSender under a test binary returned %T; want *memoryMailSender", s)
	}
}

// TestMemoryMailSender_RecordsEnvelopes covers the memory impl's
// capture-and-inspect flow used by tests that want to assert specific
// mail was (or was not) sent.
func TestMemoryMailSender_RecordsEnvelopes(t *testing.T) {
	s := newMemoryMailSender()
	ctx := context.Background()

	if err := s.Send(ctx, "mayor/", "MERGED: gt-abc", "body 1", true); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(ctx, "witness/", "ping", "body 2", false); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := s.Sent()
	if len(got) != 2 {
		t.Fatalf("got %d envelopes; want 2", len(got))
	}

	want := []MailEnvelope{
		{To: "mayor/", Subject: "MERGED: gt-abc", Body: "body 1", Permanent: true},
		{To: "witness/", Subject: "ping", Body: "body 2", Permanent: false},
	}
	for i, env := range got {
		if env != want[i] {
			t.Errorf("envelope[%d] = %+v; want %+v", i, env, want[i])
		}
	}
}

// TestMemoryMailSender_SentReturnsSnapshot ensures the Sent() slice is
// a copy, not a live reference — mutating the returned slice must not
// affect the sender's internal state (and vice versa: subsequent Send
// calls don't mutate previously-returned snapshots).
func TestMemoryMailSender_SentReturnsSnapshot(t *testing.T) {
	s := newMemoryMailSender()
	ctx := context.Background()

	_ = s.Send(ctx, "a/", "s", "b", false)
	snap := s.Sent()

	_ = s.Send(ctx, "b/", "s", "b", false)
	if len(snap) != 1 {
		t.Errorf("snapshot length changed after subsequent Send; got %d, want 1", len(snap))
	}

	// Mutate the returned slice; internal state should be unaffected.
	snap[0].To = "mutated/"
	if s.Sent()[0].To != "a/" {
		t.Errorf("Sent() is not a snapshot — mutation leaked into internal state")
	}
}

// TestExecMailSender_BuildsCorrectArgs is a structural check — we don't
// actually run `gt` from a test (the default sender is memory under
// test binaries anyway), but we do verify the exec sender's
// constructor stores the workDir and is non-nil, which guards against
// accidental swaps during refactor.
func TestExecMailSender_ConstructorPreservesWorkDir(t *testing.T) {
	const wd = "/some/work/dir"
	s := newExecMailSender(wd)
	if s == nil {
		t.Fatal("newExecMailSender returned nil")
	}
	if s.workDir != wd {
		t.Errorf("workDir = %q; want %q", s.workDir, wd)
	}
}
