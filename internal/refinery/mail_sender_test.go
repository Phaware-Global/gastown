package refinery

import (
	"context"
	"os"
	"testing"
)

// TestRunningAsTestBinary pins the detector used by defaultMailSender
// to choose between memory and exec impls. If the detector ever
// stops recognizing the `.test` suffix, every refinery test would
// silently revert to subprocess-forking `gt mail send` and the G10
// flood would resurface on the next `go test` run.
//
// The subtests exercise runningAsTestBinary() itself (not a mirror of
// its logic) by swapping os.Args[0] through t.Cleanup. NOT
// t.Parallel-safe — os.Args is process-global — but the rest of the
// refinery package doesn't run subtests concurrently with this one,
// and the cleanup restores argv before the test returns.
func TestRunningAsTestBinary(t *testing.T) {
	// Sanity: we are, in fact, running as a test binary right now.
	if !runningAsTestBinary() {
		t.Errorf("runningAsTestBinary() = false in a test; expected true (os.Args[0]=%q)", os.Args[0])
	}

	if len(os.Args) == 0 {
		t.Skip("os.Args is empty — can't exercise argv swap")
	}
	orig := os.Args[0]
	t.Cleanup(func() { os.Args[0] = orig })

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
			os.Args[0] = tc.arg0
			got := runningAsTestBinary()
			if got != tc.want {
				t.Errorf("runningAsTestBinary() with os.Args[0]=%q = %v; want %v",
					tc.arg0, got, tc.want)
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

// TestEngineer_MailSenderOrDefaultLazyInits covers the lazy-init
// accessor added so that struct-literal-constructed Engineers (common
// in older tests like engineer_merge_slot_test.go) don't panic when
// they hit HandleMRInfoSuccess or notifyConvoyCompletion with a nil
// mailSender field. NewEngineer always sets it; this guards the
// struct-literal path.
func TestEngineer_MailSenderOrDefaultLazyInits(t *testing.T) {
	// Struct-literal Engineer with nil mailSender — no NewEngineer, no
	// SetMailSender. Simulates the existing tests' construction pattern.
	e := &Engineer{workDir: t.TempDir()}
	if e.mailSender != nil {
		t.Fatalf("precondition: struct-literal Engineer should have nil mailSender; got %T", e.mailSender)
	}
	got := e.mailSenderOrDefault()
	if got == nil {
		t.Fatal("mailSenderOrDefault returned nil")
	}
	// Under a test binary, defaultMailSender returns memory impl.
	if _, ok := got.(*memoryMailSender); !ok {
		t.Errorf("lazy default under test binary returned %T; want *memoryMailSender", got)
	}
	// Second call returns the same instance (no double-init).
	if e.mailSenderOrDefault() != got {
		t.Errorf("mailSenderOrDefault returned a different instance on second call; want cached")
	}
}

// TestMemoryMailSender_SendWithOptionsCapturesPermanent verifies the
// extended-options entry point on the memory impl. WorkDir is
// deliberately NOT recorded in the envelope — it's a cwd hint for
// the exec impl only — but the Permanent flag from opts must flow
// through so convoy-path tests can assert permanent=false semantics
// (the convoy notification is not permanent, unlike the MERGED
// notification to mayor which is).
func TestMemoryMailSender_SendWithOptionsCapturesPermanent(t *testing.T) {
	s := newMemoryMailSender()
	ctx := context.Background()

	if err := s.SendWithOptions(ctx, "crew/alice", "subj1", "body1",
		MailSendOptions{WorkDir: "/town/root", Permanent: false}); err != nil {
		t.Fatalf("SendWithOptions: %v", err)
	}
	if err := s.SendWithOptions(ctx, "mayor/", "subj2", "body2",
		MailSendOptions{WorkDir: "/town/root", Permanent: true}); err != nil {
		t.Fatalf("SendWithOptions: %v", err)
	}

	got := s.Sent()
	if len(got) != 2 {
		t.Fatalf("got %d envelopes; want 2", len(got))
	}
	if got[0].Permanent {
		t.Errorf("envelope[0].Permanent = true; want false")
	}
	if !got[1].Permanent {
		t.Errorf("envelope[1].Permanent = false; want true")
	}
}
