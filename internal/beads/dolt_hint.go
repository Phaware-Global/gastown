package beads

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DoltStartGuidance replaces bd's "run bd dolt start" advice in anything gt
// relays to an agent.
//
// bd's connection errors tell the reader to start a server with `bd dolt start`.
// That command is an explicit start: it ignores dolt.auto-start: false and
// BEADS_DOLT_AUTO_START=0, and when run from an agent's own directory (the
// deacon's cwd is <town>/deacon) it binds the shared port from that directory's
// empty .beads/dolt store. Every agent then reads empty data until the canonical
// server reclaims the port. Agents follow the advice because gt hands it to them
// verbatim in `gt patrol new` and every wrapped bd error.
const DoltStartGuidance = "Dolt is managed by Gas Town, not by bd. Do NOT start a server with bd: " +
	"it ignores dolt.auto-start and binds the shared port to an empty store. " +
	"Run 'gt dolt status'; if the server is down, escalate: gt escalate -s HIGH \"Dolt: server unreachable\"."

// RedirectDoltStartHint rewrites every line of bd output that advises running
// `bd dolt start` into DoltStartGuidance (once per message) and drops the lead-in
// lines that introduced the advice. Output without the advice is returned as is.
func RedirectDoltStartHint(s string) string {
	if !strings.Contains(s, "bd dolt start") {
		return s
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	redirected := false
	for _, line := range lines {
		if rewritten, keep := redirectHintLine(line, &redirected); keep {
			out = append(out, rewritten)
		}
	}
	return strings.Join(out, "\n")
}

// redirectHintLine applies the rewrite to one line. keep is false for a line
// that must be dropped; redirected tracks whether the guidance was already
// emitted for this message.
func redirectHintLine(line string, redirected *bool) (out string, keep bool) {
	switch trimmed := strings.TrimSpace(line); {
	case trimmed == "Start the server manually:",
		trimmed == "The Dolt server may not be running. Try:":
		return "", false
	case strings.Contains(line, "bd dolt start"):
		if *redirected {
			return "", false
		}
		*redirected = true
		return DoltStartGuidance, true
	default:
		return line, true
	}
}

// HintWriter is an io.Writer that applies RedirectDoltStartHint to a stream of
// bd stderr, for commands whose stderr goes straight to the terminal. It works
// line by line so the advice is caught even when bd's writes split it. The final
// unterminated line is held until Flush, so call Flush after the command exits.
type HintWriter struct {
	w          io.Writer
	pending    []byte
	redirected bool
}

// NewHintWriter wraps w.
func NewHintWriter(w io.Writer) *HintWriter { return &HintWriter{w: w} }

// Write buffers p and forwards each completed line. It reports len(p) on
// success so the caller's copy loop is not confused by the rewritten length.
func (h *HintWriter) Write(p []byte) (int, error) {
	h.pending = append(h.pending, p...)
	for {
		i := bytes.IndexByte(h.pending, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(h.pending[:i])
		h.pending = h.pending[i+1:]
		if err := h.emit(line, "\n"); err != nil {
			return 0, err
		}
	}
}

// Flush forwards the held unterminated line, if any.
func (h *HintWriter) Flush() error {
	if len(h.pending) == 0 {
		return nil
	}
	line := string(h.pending)
	h.pending = nil
	return h.emit(line, "")
}

func (h *HintWriter) emit(line, eol string) error {
	out, keep := redirectHintLine(line, &h.redirected)
	if !keep {
		return nil
	}
	_, err := io.WriteString(h.w, out+eol)
	return err
}

// relayWaitDelay bounds how long Wait lingers on the stderr copy after bd exits.
// Wrapping stderr makes exec create a pipe, and Wait does not return until every
// holder of that pipe closes it: bd can leave a long-lived child (an auto-started
// dolt sql-server) holding it, which would hang the gt command indefinitely.
const relayWaitDelay = time.Second

// RelayStderr swaps a c.Stderr that is os.Stderr for a HintWriter around it and
// returns the flush to call once c has exited. Any other stderr (a buffer, a
// discard) is left alone: those callers capture the output and format it
// themselves, which is where RedirectDoltStartHint applies instead.
//
// A wrapped command gets c.WaitDelay (unless the caller set one), so when bd
// exits successfully but a child still holds stderr, Wait reports
// exec.ErrWaitDelay; pass the result through TrimWaitDelay.
func RelayStderr(c *exec.Cmd) (flush func()) {
	if f, ok := c.Stderr.(*os.File); !ok || f != os.Stderr {
		return func() {}
	}
	if c.WaitDelay == 0 {
		c.WaitDelay = relayWaitDelay
	}
	h := NewHintWriter(os.Stderr)
	c.Stderr = h
	return func() { _ = h.Flush() }
}

// TrimWaitDelay drops exec.ErrWaitDelay, which Wait returns for a command that
// exited successfully while a child it started still held a pipe open. The
// command itself succeeded; any other error is returned unchanged.
func TrimWaitDelay(err error) error {
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

// RunRelayed runs c with its stderr relayed to os.Stderr, minus bd's
// `bd dolt start` advice. Use it instead of setting c.Stderr = os.Stderr on a
// bd command.
func RunRelayed(c *exec.Cmd) error {
	defer relayByDefault(c)()
	return TrimWaitDelay(c.Run())
}

// OutputRelayed is RunRelayed for a command whose stdout the caller reads.
func OutputRelayed(c *exec.Cmd) ([]byte, error) {
	defer relayByDefault(c)()
	out, err := c.Output()
	return out, TrimWaitDelay(err)
}

func relayByDefault(c *exec.Cmd) func() {
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
	return RelayStderr(c)
}
