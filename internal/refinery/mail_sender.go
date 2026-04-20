package refinery

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/util"
)

// runningAsTestBinary returns true when the current process looks like
// a Go test binary. Mirrors the check in internal/mail/test_guard.go
// (pre-existing from PR #9) without taking a dependency on that
// package's private helpers.
//
// `go test` names the compiled test binary `<pkg>.test` (or
// `.test.exe` on Windows). That suffix on os.Args[0] is the
// canonical detector.
//
// Used by defaultMailSender to pick the memory-backed impl when we
// detect we're in a test binary — so `go test ./...` runs in the
// refinery package (or any downstream consumer) do not subprocess-fork
// `gt mail send` and flood the real mayor. Tests that want to assert
// envelopes can still override via Engineer.SetMailSender; the default
// just keeps them safe by construction.
func runningAsTestBinary() bool {
	if len(os.Args) == 0 {
		return false
	}
	base := filepath.Base(os.Args[0])
	return strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe")
}

// defaultMailSender picks the MailSender impl appropriate for the
// caller's runtime: memory-backed when we're under `go test`, exec-
// backed otherwise. Callers (NewEngineer) use this instead of
// hardcoding newExecMailSender so every Engineer constructed in a
// test — including code paths that never reach an explicit test
// helper — stays safe by default.
func defaultMailSender(workDir string) MailSender {
	if runningAsTestBinary() {
		return newMemoryMailSender()
	}
	return newExecMailSender(workDir)
}

// MailSendOptions carries optional per-call overrides. Zero-value
// defaults reproduce the primary Send path; callers set fields only
// for the rare cases that need to differ from the sender's defaults.
type MailSendOptions struct {
	// Permanent requests durable (non-wisp) delivery. The receiving
	// mailbox keeps the message indefinitely; the sender's inbox
	// subscription still gets an auto-nudge on receipt.
	Permanent bool

	// WorkDir overrides the cwd the production (exec) impl runs
	// `gt mail send` in. When empty, the sender's configured default
	// is used. Set this when the message targets a mailbox that the
	// sender's default cwd might not resolve to — e.g. convoy-
	// completion notifications going to town-level subscribers when
	// the Engineer is configured with a rig-level workDir.
	// The memory (test) impl ignores this field; envelope capture
	// doesn't depend on filesystem resolution.
	WorkDir string
}

// MailSender abstracts the refinery's outbound mail path so tests can
// swap in a non-exec impl.
//
// Why this exists: the refinery notifies the mayor about successful
// merges (and notifies convoy owners about convoy completion) by
// spawning a `gt mail send` subprocess. That was an intentional choice
// — `gt mail send` as a subprocess triggers the auto-nudge side effect
// (the mayor wakes up immediately on receipt), which a direct
// `mail.Router.Send` call from inside this process would miss.
//
// The cost of that design was exposed by the 2026-04-19 Telegraph v1
// dogfood (G10 in docs/design/refinery-pr-workflow.md): when a test
// runs `engineer.doMergePR()` with fixture values (mr-a / feature-a /
// test-rig in batch_test.go), the subprocess `gt mail send` that
// fires is a fresh process — its os.Args[0] is "gt", not "*.test" —
// so PR #9's mail Layer 1 guard (which keys on argv[0]) can't see it.
// The subprocess ships real mail to the real mayor.
//
// Every `go test ./...` run from a polecat shadows-bombs the mayor
// with bogus MERGED mails. The mayor's inbox climbs by tens of
// entries per polecat per test run, burying real completion signals
// (G17: Telegraph L1/L2 MR beads disappeared when the mayor couldn't
// distinguish real from noise).
//
// This seam is the "proper fix" recommended in G10 Option 1: the
// refinery gains a MailSender dependency with a default exec-based
// prod impl and a memory-backed test impl. batch_test.go (and any
// future refinery tests) inject the memory impl, so `go test` stops
// emitting subprocess mail entirely.
//
// The interface is deliberately narrow: Send(ctx, to, subject, body,
// permanent). It covers both present call sites (merge notification,
// convoy-completion notification) without committing to a broader
// mail-protocol surface. If a future caller needs more (CC, attachments,
// etc.), extend Send's signature or add a sibling method rather than
// fattening this up speculatively.
type MailSender interface {
	// Send delivers a mail with the sender's configured defaults.
	// The permanent flag maps to `gt mail send --permanent` in the
	// exec impl; the memory impl records it on the envelope.
	Send(ctx context.Context, to, subject, body string, permanent bool) error

	// SendWithOptions delivers a mail with per-call overrides. Use
	// this when a specific message needs a cwd that differs from the
	// sender's default (e.g. convoy notifications targeting
	// town-level mailboxes from a rig-scoped sender). The Permanent
	// field in MailSendOptions takes precedence over any legacy
	// permanent flag — callers should prefer SendWithOptions when
	// setting WorkDir so both dimensions stay in one struct.
	SendWithOptions(ctx context.Context, to, subject, body string, opts MailSendOptions) error
}

// execMailSender is the default production MailSender. It preserves the
// historical `gt mail send <to> -s <subject> -m <body> [--permanent]`
// subprocess-fork behavior so the auto-nudge side effect survives.
type execMailSender struct {
	// workDir is the directory the subprocess runs in. The `gt` CLI
	// discovers the town/rig from its cwd, so this must be inside the
	// relevant rig's workspace (typically the engineer's workDir).
	workDir string
}

// newExecMailSender returns the prod impl anchored to workDir. Callers
// typically pass Engineer.workDir.
func newExecMailSender(workDir string) *execMailSender {
	return &execMailSender{workDir: workDir}
}

// Send forks `gt mail send <to> -s <subject> -m <body> [--permanent]`.
// Errors from the subprocess (non-zero exit, timeout from ctx) propagate
// to the caller as a wrapped error that includes stdout/stderr so the
// caller can log a useful message.
func (s *execMailSender) Send(ctx context.Context, to, subject, body string, permanent bool) error {
	return s.SendWithOptions(ctx, to, subject, body, MailSendOptions{Permanent: permanent})
}

// SendWithOptions honors opts.WorkDir (overriding the sender's default
// cwd for this call only) and opts.Permanent. Other fields of
// MailSendOptions extend in the future without breaking the simpler
// Send signature.
func (s *execMailSender) SendWithOptions(ctx context.Context, to, subject, body string, opts MailSendOptions) error {
	args := []string{"mail", "send", to, "-s", subject, "-m", body}
	if opts.Permanent {
		args = append(args, "--permanent")
	}
	cmd := exec.CommandContext(ctx, "gt", args...)
	util.SetDetachedProcessGroup(cmd)
	cmd.Dir = s.workDir
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Avoid a double-colon error message when stdout/stderr is empty
		// (common for some exec failure modes). Format with or without
		// the output snippet as appropriate.
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			return fmt.Errorf("gt mail send to %q failed: %w", to, err)
		}
		return fmt.Errorf("gt mail send to %q failed: %s: %w", to, trimmed, err)
	}
	return nil
}

// MailEnvelope is a recorded send captured by memoryMailSender. Exposed
// for test assertions.
type MailEnvelope struct {
	To        string
	Subject   string
	Body      string
	Permanent bool
}

// memoryMailSender records every Send call in-process and never forks
// a subprocess. Intended for use in refinery tests (batch_test.go and
// similar) so `go test ./internal/refinery/...` does not ship mail to
// the real mayor.
//
// Exported only via a testing-friendly constructor and accessor
// methods so the intent is clear at call sites: this is explicitly a
// test double, not a production alternative.
type memoryMailSender struct {
	mu   sync.Mutex
	sent []MailEnvelope
}

// newMemoryMailSender returns a fresh in-memory sender with an empty
// sent list. Tests construct one per Engineer and inspect Sent() at
// the end of the test to assert expected notifications.
func newMemoryMailSender() *memoryMailSender {
	return &memoryMailSender{}
}

// Send captures the envelope and returns nil. No subprocess, no network.
func (s *memoryMailSender) Send(ctx context.Context, to, subject, body string, permanent bool) error {
	return s.SendWithOptions(ctx, to, subject, body, MailSendOptions{Permanent: permanent})
}

// SendWithOptions captures the envelope including opts.Permanent. The
// WorkDir field is deliberately not recorded — it's a cwd hint for the
// exec impl only, not something test assertions need to inspect.
func (s *memoryMailSender) SendWithOptions(_ context.Context, to, subject, body string, opts MailSendOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, MailEnvelope{
		To:        to,
		Subject:   subject,
		Body:      body,
		Permanent: opts.Permanent,
	})
	return nil
}

// Sent returns a copy of the recorded envelopes. Safe to call
// concurrently with Send; the returned slice is a snapshot.
func (s *memoryMailSender) Sent() []MailEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MailEnvelope, len(s.sent))
	copy(out, s.sent)
	return out
}
