// Package transform implements Telegraph L3: envelope builder and rate-limited nudge.
// It is provider-agnostic; it consumes NormalizedEvent from L2 and produces
// mail addressed to the Mayor's Assistant plus at most one nudge per configured
// window. (Routed to the Mayor's Assistant: the assistant triages inbound
// events and briefs the Mayor; see docs/design/assistant-role.md.)
package transform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/telegraph"
	"github.com/steveyegge/gastown/internal/telegraph/prompts"
	"github.com/steveyegge/gastown/internal/telegraph/tlog"
)

// MailSender abstracts mail delivery. Satisfied by *mail.Router in production
// and *mail.MemoryRouter in tests.
type MailSender interface {
	Send(msg *mail.Message) error
}

// Nudger abstracts nudge delivery so the rate-limit logic can be tested
// without spawning a subprocess.
type Nudger interface {
	Nudge(target, message string) error
}

// nudgeExecTimeout bounds the gt nudge subprocess. Nudge runs synchronously in
// the single-threaded dispatch loop (maybeNudge → Transform), so an unbounded
// hang would freeze the daemon and eventually 503 incoming webhooks.
const nudgeExecTimeout = 5 * time.Second

// ExecNudger runs "gt nudge <target> --mode=queue -m <message>" as a subprocess.
// It is the production Nudger.
type ExecNudger struct {
	// TownRoot, if set, is the working directory for the gt nudge subprocess.
	// Queue mode requires a Gas Town workspace, and `gt nudge` resolves it from
	// its cwd (workspace.FindFromCwd, no env fallback), so running from the town
	// root lets it resolve the queue directory even when the telegraph daemon
	// was started from outside the workspace. Empty keeps the inherited cwd.
	TownRoot string
}

// Nudge delivers a nudge by running gt nudge in queue mode.
func (n *ExecNudger) Nudge(target, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), nudgeExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gt", nudgeCommandArgs(target, message)...)
	if n.TownRoot != "" {
		cmd.Dir = n.TownRoot
	}
	return cmd.Run()
}

// Recipient is the single destination for every Telegraph event: the Mayor's
// Assistant inbox. Telegraph has no routing logic; the assistant consolidates
// events and briefs mayor/. The address is a well-known town-level singleton,
// so mail queues in the town store even when the assistant session is down.
const Recipient = "assistant/"

// nudgeCommandArgs builds the `gt nudge` argument vector for telegraph wakeups.
//
// Queue mode (not the default wait-idle): telegraph fires one wakeup per inbound
// webhook event, and its only target — the assistant — is a Claude agent with a
// turn-boundary drain. Wait-idle would optimistically send-keys-inject each
// wakeup the instant the assistant looked idle; under high event volume those
// injections pile into its input box during busy windows. Cooperative queueing
// drains at the next turn boundary instead. The assistant is still woken
// promptly: each underlying message also arrives as a per-message mail
// notification delivered by the router.
func nudgeCommandArgs(target, message string) []string {
	return []string{"nudge", target, "--mode=queue", "-m", message}
}

// Transformer is the L3 layer. It is safe for concurrent use.
type Transformer struct {
	sender      MailSender
	nudger      Nudger
	bodyCap     int
	nudgeWindow time.Duration
	resolver    *prompts.Resolver // nil disables operator prompt blocks
	log         *tlog.Logger      // nil disables logging

	mu        sync.Mutex
	lastNudge time.Time
}

// New returns a Transformer. bodyCap is the maximum bytes of external text
// written into the mail body; content beyond the cap is truncated. nudgeWindow
// is the minimum interval between consecutive Mayor nudges; zero disables nudges.
// resolver may be nil to disable operator prompt blocks. logger may be nil to
// disable structured logging.
func New(sender MailSender, nudger Nudger, bodyCap int, nudgeWindow time.Duration, resolver *prompts.Resolver, logger ...*tlog.Logger) *Transformer {
	var l *tlog.Logger
	if len(logger) > 0 {
		l = logger[0]
	}
	return &Transformer{
		sender:      sender,
		nudger:      nudger,
		bodyCap:     bodyCap,
		nudgeWindow: nudgeWindow,
		resolver:    resolver,
		log:         l,
	}
}

// NewProduction returns a Transformer wired to a production mail router and an
// exec-based nudger. townRoot is used to construct the Router's working directory.
// resolver may be nil to disable operator prompt blocks. logger may be nil to
// disable structured logging.
func NewProduction(townRoot string, bodyCap int, nudgeWindow time.Duration, resolver *prompts.Resolver, logger *tlog.Logger) *Transformer {
	router := mail.NewRouterWithTownRoot(townRoot, townRoot)
	return New(router, &ExecNudger{TownRoot: townRoot}, bodyCap, nudgeWindow, resolver, logger)
}

// Transform builds a mail envelope addressed to the Mayor's Assistant from
// event, sends it via the configured MailSender, and conditionally nudges the
// assistant within the rate-limit window. It is safe to call concurrently.
func (t *Transformer) Transform(event *telegraph.NormalizedEvent) error {
	var promptText, promptKey string
	if t.resolver != nil {
		promptText, promptKey = t.resolver.Resolve(event)
	}

	msg := mail.NewMessage(
		buildFrom(event),
		Recipient,
		buildSubject(event),
		t.buildBody(event, promptText),
	)

	if err := t.sender.Send(msg); err != nil {
		return fmt.Errorf("telegraph/transform: mail send: %w", err)
	}

	// mail_id is not returned by MailSender.Send in v1; omit from log.
	t.log.Deliver(event.Provider, event.EventType, event.EventID, event.Actor, event.Subject, "", promptKey)

	t.maybeNudge(event)
	return nil
}

// maybeNudge sends a nudge to the assistant if the rate-limit window has elapsed.
// event is used for logging context only.
func (t *Transformer) maybeNudge(_ *telegraph.NormalizedEvent) {
	if t.nudgeWindow <= 0 || t.nudger == nil {
		return
	}

	t.mu.Lock()
	now := time.Now()
	if now.Sub(t.lastNudge) < t.nudgeWindow {
		t.mu.Unlock()
		t.log.NudgeSuppressed()
		return
	}
	t.lastNudge = now
	t.mu.Unlock()

	// Nudge failure is non-fatal (e.g. assistant session not running): the
	// mail is already queued and is discovered on the next inbox check.
	_ = t.nudger.Nudge(Recipient, "Telegraph: new events pending")
	t.log.NudgeSent()
}

// operatorPolicyHeading is the section heading the Mayor's Assistant uses
// when it relays a genuine OPERATOR PROMPT block to the Mayor. External text
// must not be able to smuggle that heading through a briefing.
const operatorPolicyHeading = "operator policy (from telegraph config)"

// lineSeparators maps the Unicode line separators (including VT and FF,
// which renderers treat as mandatory breaks) to "\n" so delimiter matching
// sees the same lines a model does.
var lineSeparators = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\u2028", "\n", "\u2029", "\n", "\u0085", "\n")

// stripControls replaces every remaining C0/C1 control character except
// "\n" and "\t" with U+FFFD, so no control sequence can split or hide a line.
func stripControls(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return '\uFFFD'
		}
		return r
	}, text)
}

// trustMarkers are the phrases that, on a line of their own, an LLM reads as
// a trust delimiter or trusted heading. They are compared letters-only (see
// canonicalLine), so spacing, dashes and punctuation never matter. The bare
// "Operator policy" heading is included so a forged heading without the
// "(from telegraph config)" suffix is caught too.
var trustMarkers = []string{"EXTERNALCONTENT", "OPERATORPROMPT", "OPERATORPOLICY", "TELEGRAPHBOUNDARY"}

// homoglyphs folds characters that render like Latin capitals so the
// letters-only comparison sees the Latin word: Cyrillic and Greek capitals,
// Latin small capitals, Cherokee and Lisu letters, Armenian Օ, and the digits
// commonly swapped for letters (0→O, 1→I, 3→E, 4→A, 5→S, 7→T, 8→B). This is
// defense in depth behind the per-message boundary nonce, not the primary
// guard, so the table need not be exhaustive.
var homoglyphs = map[rune]rune{
	// Cyrillic
	'А': 'A', 'В': 'B', 'Е': 'E', 'К': 'K', 'М': 'M', 'Н': 'H', 'О': 'O', 'Р': 'P', 'С': 'C', 'Т': 'T', 'Х': 'X', 'У': 'Y', 'І': 'I', 'Ѕ': 'S', 'Ј': 'J',
	// Greek
	'Α': 'A', 'Β': 'B', 'Ε': 'E', 'Ζ': 'Z', 'Η': 'H', 'Ι': 'I', 'Κ': 'K', 'Μ': 'M', 'Ν': 'N', 'Ο': 'O', 'Ρ': 'P', 'Τ': 'T', 'Υ': 'Y', 'Χ': 'X',
	// Latin small capitals (upper-cased forms have no mapping, so list the originals)
	'ᴀ': 'A', 'ʙ': 'B', 'ᴄ': 'C', 'ᴅ': 'D', 'ᴇ': 'E', 'ꜰ': 'F', 'ɢ': 'G', 'ʜ': 'H', 'ɪ': 'I', 'ᴊ': 'J', 'ᴋ': 'K', 'ʟ': 'L', 'ᴍ': 'M', 'ɴ': 'N', 'ᴏ': 'O', 'ᴘ': 'P', 'ʀ': 'R', 'ꜱ': 'S', 'ᴛ': 'T', 'ᴜ': 'U', 'ᴠ': 'V', 'ᴡ': 'W', 'ʏ': 'Y', 'ᴢ': 'Z',
	// Cherokee
	'Ꭺ': 'A', 'Ꭱ': 'R', 'Ꭲ': 'T', 'Ꭼ': 'E', 'Ꭻ': 'J', 'Ꮇ': 'M', 'Ꮋ': 'H', 'Ꮎ': 'O', 'Ꮐ': 'G', 'Ꮓ': 'Z', 'Ꮩ': 'V', 'Ꮪ': 'S', 'Ꮮ': 'L', 'Ꮯ': 'C', 'Ꮲ': 'P', 'Ꮃ': 'W', 'Ꮶ': 'K', 'Ꭰ': 'D', 'Ꮟ': 'B', 'Ꮢ': 'R', 'Ꮍ': 'Y',
	// Lisu
	'ꓐ': 'B', 'ꓚ': 'C', 'ꓓ': 'D', 'ꓰ': 'E', 'ꓝ': 'F', 'ꓖ': 'G', 'ꓧ': 'H', 'ꓲ': 'I', 'ꓙ': 'J', 'ꓗ': 'K', 'ꓡ': 'L', 'ꓟ': 'M', 'ꓠ': 'N', 'ꓳ': 'O', 'ꓑ': 'P', 'ꓣ': 'R', 'ꓢ': 'S', 'ꓔ': 'T', 'ꓴ': 'U', 'ꓦ': 'V', 'ꓪ': 'W', 'ꓫ': 'X', 'ꓬ': 'Y', 'ꓜ': 'Z', 'ꓮ': 'A',
	// Armenian
	'Օ': 'O', 'Տ': 'S',
	// Digits swapped for letters
	'0': 'O', '1': 'I', '3': 'E', '4': 'A', '5': 'S', '7': 'T', '8': 'B',
}

// invisibleLetters are runes Unicode classes as letters (Lo) but that render
// as blanks: the Hangul choseong/jungseong fillers and the halfwidth filler.
// NFKC maps U+3164 to U+1160, so both forms are listed.
var invisibleLetters = map[rune]bool{0x115F: true, 0x1160: true, 0x3164: true, 0xFFA0: true}

// canonicalForms returns the two canonical spellings of a line that are
// compared against trustMarkers: one with look-alike digits folded to letters
// (catches "C0NTENT") and one with digits dropped (catches "EXTERNAL 1
// CONTENT", where a digit is used as a separator). A line is a marker if
// either form contains one.
func canonicalForms(line string) [2]string {
	return [2]string{canonicalLine(line, true), canonicalLine(line, false)}
}

// canonicalLine reduces a line to the letters an LLM effectively reads:
// NFKC-normalized (fullwidth and compatibility forms folded), every rune that
// is not a letter dropped (whitespace, dashes, punctuation, zero-width and
// invisible fillers, combining marks, braille blanks, emoji), common
// Cyrillic/Greek homoglyphs folded to Latin, and everything upper-cased.
// Matching an allowlist of letters, rather than denying categories one at a
// time, is what makes the comparison robust to new separator tricks.
func canonicalLine(line string, foldDigits bool) string {
	var b strings.Builder
	for _, r := range norm.NFKC.String(line) {
		if unicode.IsDigit(r) && !foldDigits {
			continue
		}
		if folded, ok := homoglyphs[r]; ok {
			b.WriteRune(folded) // digits and small caps fold before the letter test
			continue
		}
		if !unicode.IsLetter(r) || invisibleLetters[r] {
			continue
		}
		r = unicode.ToUpper(r)
		if folded, ok := homoglyphs[r]; ok {
			r = folded
		}
		b.WriteRune(r)
	}
	return b.String()
}

// lineHasTrustMarker reports whether either canonical form of line contains
// a trust marker (delimiter phrase, trusted heading, or the Telegraph-Boundary
// header name — a forged header line must never compete with the real one).
func lineHasTrustMarker(line string) bool {
	for _, canon := range canonicalForms(line) {
		for _, marker := range trustMarkers {
			if strings.Contains(canon, marker) {
				return true
			}
		}
	}
	return false
}

// flattenPrompt prepares resolved operator-prompt text for the nonce-certified
// OPERATOR PROMPT block: substituted values such as {actor} may carry
// U+2028/VT/FF/CR that a model reads as line breaks, so every separator other
// than the template's own "\n" becomes a space and remaining controls are
// stripped. Lines are kept so operator-authored formatting survives.
func flattenPrompt(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\v', '\f', '\u2028', '\u2029', '\u0085':
			return ' '
		}
		return r
	}, text)
	return stripControls(text)
}

// neutralizeDelimiters defuses trust-delimiter lines inside external text so
// a provider comment cannot close its own EXTERNAL CONTENT block, open a fake
// OPERATOR PROMPT, or forge the assistant's "Operator policy" heading and
// have the tail read as trusted. Line separators and control characters are
// normalized first; then any line whose canonical form contains a trust
// marker — regardless of leading dashes, zero-width characters, spacing or
// dash variants — is prefixed with a visible marker, which keeps the text
// readable but stops it reading as a real delimiter or heading.
func neutralizeDelimiters(text string) string {
	text = stripControls(lineSeparators.Replace(text))
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if lineHasTrustMarker(line) {
			lines[i] = "[neutralized delimiter] " + line
		}
	}
	return strings.Join(lines, "\n")
}

// buildFrom returns the sender address: telegraph/<provider>/<actor>. The
// actor is a provider display name and `gt mail read` prints From on a line
// of its own outside the EXTERNAL CONTENT block, so it gets the same
// normalization as every other header field.
func buildFrom(event *telegraph.NormalizedEvent) string {
	return "telegraph/" + sanitizeHeaderValue(event.Provider) + "/" + sanitizeHeaderValue(event.Actor)
}

// buildSubject constructs the mail subject from structured NormalizedEvent fields.
// No raw user text is included; the subject is safe to display untrusted input.
//
// Format: [<PROVIDER> <SUBJECT>] <EventType prose>
func buildSubject(event *telegraph.NormalizedEvent) string {
	// The subject sits outside the EXTERNAL CONTENT block and embeds the
	// provider's subject key and actor verbatim; normalize the whole line so
	// no Unicode line break or control character can smuggle a forged
	// delimiter onto its own line.
	return sanitizeHeaderValue(buildSubjectRaw(event))
}

func buildSubjectRaw(event *telegraph.NormalizedEvent) string {
	provider := strings.ToUpper(event.Provider)
	tag := fmt.Sprintf("[%s %s]", provider, event.Subject)

	switch event.EventType {
	case "issue.created":
		return fmt.Sprintf("%s Issue created: %s", tag, safeTitle(event.Text))
	case "issue.updated":
		return fmt.Sprintf("%s Issue updated", tag)
	case "comment.added":
		return fmt.Sprintf("%s Comment added by %s", tag, event.Actor)
	case "comment.updated":
		return fmt.Sprintf("%s Comment updated by %s", tag, event.Actor)
	case "pull_request.opened":
		return fmt.Sprintf("%s PR opened: %s", tag, safeTitle(event.Text))
	case "pull_request.merged":
		return fmt.Sprintf("%s PR merged: %s", tag, safeTitle(event.Text))
	case "pull_request.closed_unmerged":
		return fmt.Sprintf("%s PR closed (not merged): %s", tag, safeTitle(event.Text))
	case "pull_request.review_submitted":
		return fmt.Sprintf("%s Review %s by %s", tag, reviewState(event.Labels), event.Actor)
	case "pull_request.review_comment":
		return fmt.Sprintf("%s Review comment by %s", tag, event.Actor)
	case "pull_request.comment":
		return fmt.Sprintf("%s Comment added by %s", tag, event.Actor)
	case "check_run.failed":
		return fmt.Sprintf("%s Check run failed", tag)
	case "check_suite.failed":
		return fmt.Sprintf("%s Check suite failed", tag)
	case "workflow_run.failed":
		return fmt.Sprintf("%s Workflow run failed", tag)
	default:
		return fmt.Sprintf("%s %s", tag, event.EventType)
	}
}

// reviewState extracts the review state from a NormalizedEvent.Labels slice.
// The GitHub translator emits the state as "review:<state>" so the subject
// builder can surface it without poking provider payloads.
func reviewState(labels []string) string {
	const prefix = "review:"
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return l[len(prefix):]
		}
	}
	return "submitted"
}

// safeTitle returns at most the first line of text (no raw body content in subject).
// Truncation is rune-based to avoid splitting multi-byte UTF-8 sequences.
func safeTitle(text string) string {
	// Subjects sit outside the EXTERNAL CONTENT block, so a forged delimiter
	// after a Unicode line break would read as a real one: normalize breaks
	// and controls exactly as the body does before cutting at the first line.
	text = stripControls(lineSeparators.Replace(text))
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	const maxTitleRunes = 80
	if utf8.RuneCountInString(text) > maxTitleRunes {
		text = string([]rune(text)[:maxTitleRunes])
	}
	return text
}

// replyHintFor returns the out-of-band reply tool string for a provider.
// Telegraph never propagates Mayor replies back to the provider; this hint
// is what tells Mayor where to actually act. Returning empty string for an
// unknown provider keeps the field optional rather than emitting an empty
// header.
func replyHintFor(provider string) string {
	switch provider {
	case "jira":
		return "out-of-band only — use Jira MCP tools (e.g. mcp__atlassian__addCommentToJiraIssue, mcp__atlassian__transitionJiraIssue); do NOT reply via bead"
	case "github":
		return "out-of-band only — use `gh` CLI or GitHub MCP (e.g. gh pr comment, gh pr review); do NOT reply via bead"
	default:
		return ""
	}
}

// sanitizeHeaderValue strips CR and LF characters from a value that will be
// written into a header line. A newline in a header value would allow
// injection of arbitrary header lines into the mail body.
func sanitizeHeaderValue(s string) string {
	// Header values (actor, labels, check names) sit outside the EXTERNAL
	// CONTENT block: normalize every line separator and control character
	// first so a U+2028- or VT-joined forged delimiter cannot survive as a
	// line of its own, then drop the resulting newlines.
	s = stripControls(lineSeparators.Replace(s))
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
}

// buildBody constructs the full mail body: Telegraph-* metadata headers, an
// optional OPERATOR PROMPT block (when promptText is non-empty), and a delimited
// external-content block. The external text is capped at bodyCap bytes to limit
// context-injection surface.
func (t *Transformer) buildBody(event *telegraph.NormalizedEvent, promptText string) string {
	var b strings.Builder

	// Metadata block — all values come from NormalizedEvent structured fields.
	// sanitizeHeaderValue strips CR/LF to prevent header-injection via untrusted fields.
	b.WriteString("Telegraph-Transport: http-webhook\n")
	fmt.Fprintf(&b, "Telegraph-Provider: %s\n", sanitizeHeaderValue(event.Provider))
	fmt.Fprintf(&b, "Telegraph-Event-Type: %s\n", sanitizeHeaderValue(event.EventType))
	fmt.Fprintf(&b, "Telegraph-Event-ID: %s\n", sanitizeHeaderValue(event.EventID))
	fmt.Fprintf(&b, "Telegraph-Timestamp: %s\n", event.Timestamp.UTC().Format(time.RFC3339))
	// Per-message boundary nonce: the real trust delimiters below carry it, so
	// external text cannot forge them no matter how it is encoded. Readers
	// treat a delimiter-looking line as a delimiter only if it carries the
	// nonce from THIS header — the one in the header block before the first
	// delimiter; any later header-looking line is external text and is
	// neutralized.
	boundary := newBoundary()
	fmt.Fprintf(&b, "Telegraph-Boundary: %s\n", boundary)
	fmt.Fprintf(&b, "Telegraph-Actor: %s\n", sanitizeHeaderValue(event.Actor))
	fmt.Fprintf(&b, "Telegraph-Subject: %s\n", sanitizeHeaderValue(event.Subject))
	if event.CanonicalURL != "" {
		fmt.Fprintf(&b, "Telegraph-URL: %s\n", sanitizeHeaderValue(event.CanonicalURL))
	}
	if len(event.Labels) > 0 {
		sanitizedLabels := make([]string, len(event.Labels))
		for i, l := range event.Labels {
			sanitizedLabels[i] = sanitizeHeaderValue(l)
		}
		fmt.Fprintf(&b, "Telegraph-Labels: %s\n", strings.Join(sanitizedLabels, ", "))
	}
	// Telegraph is one-way. Replying inside the Mayor mail thread does NOT
	// propagate back to the provider — there is no return-channel translator
	// in the pipeline. Always emit a per-provider reply hint so Mayor reaches
	// for the correct out-of-band tool instead of dropping a bead reply
	// that goes nowhere.
	if hint := replyHintFor(event.Provider); hint != "" {
		fmt.Fprintf(&b, "Telegraph-Reply-Via: %s\n", hint)
	}

	// Operator prompt block — emitted only when a prompt resolved.
	// Sits between metadata headers and external content.
	if promptText != "" {
		fmt.Fprintf(&b, "\n--- OPERATOR PROMPT (trusted) %s ---\n%s\n--- END OPERATOR PROMPT %s ---", boundary, flattenPrompt(promptText), boundary)
	}

	// External content block with explicit trust delimiter (nonce-bearing).
	// Provider and Actor are sanitized to prevent delimiter spoofing.
	fmt.Fprintf(&b, "\n--- EXTERNAL CONTENT (untrusted: %s/%s) %s ---\n",
		sanitizeHeaderValue(event.Provider), sanitizeHeaderValue(event.Actor), boundary)

	text := neutralizeDelimiters(event.Text)
	if t.bodyCap > 0 && len(text) > t.bodyCap {
		text = text[:t.bodyCap] + "\n[… truncated]"
	}
	b.WriteString(text)

	fmt.Fprintf(&b, "\n--- END EXTERNAL CONTENT %s ---\n", boundary)

	return b.String()
}

// newBoundary returns the per-message delimiter nonce: 16 hex characters from
// crypto/rand. It is unguessable by the webhook author, so a forged delimiter
// can never carry it. Falls back to a time-derived value only if the system
// random source fails, which still defeats pre-computed forgeries.
func newBoundary() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%016x", uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(buf[:])
}
