package beads

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The variants are bd's own wording (store.go, doltserver.go); the first is the
// text the deacon received from `gt patrol new` before it ran `bd dolt start`.
func TestRedirectDoltStartHint(t *testing.T) {
	tests := []struct {
		name string
		in   string
		keep []string // fragments that must survive the rewrite
	}{
		{
			name: "auto-start disabled hint",
			in: "Error: failed to open database: Dolt server unreachable at 127.0.0.1:3307: dial tcp 127.0.0.1:3307: connect: connection refused\n\n" +
				"Dolt server auto-start is disabled (dolt.auto-start: false).\nStart the server manually:\n  bd dolt start",
			keep: []string{"Dolt server unreachable at 127.0.0.1:3307", "connection refused"},
		},
		{
			name: "server may not be running hint",
			in:   "Dolt server unreachable\n\nThe Dolt server may not be running. Try:\n  bd dolt start",
			keep: []string{"Dolt server unreachable"},
		},
		{
			name: "externally managed hint",
			in:   "auto-start is suppressed.\n  To start manually: bd dolt start\n  To check status: bd dolt status",
			keep: []string{"auto-start is suppressed.", "bd dolt status"},
		},
		{
			name: "orchestrator hint",
			in:   "failed to connect\n\nThe Dolt server may not be running. Try:\n  bd dolt start    # Start a local server\n  gt dolt start    # If using an orchestrator",
			keep: []string{"failed to connect"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedirectDoltStartHint(tt.in)
			if strings.Contains(got, "bd dolt start") {
				t.Errorf("output still advises bd dolt start:\n%s", got)
			}
			if strings.Count(got, DoltStartGuidance) != 1 {
				t.Errorf("want guidance exactly once:\n%s", got)
			}
			for _, k := range tt.keep {
				if !strings.Contains(got, k) {
					t.Errorf("output lost %q:\n%s", k, got)
				}
			}
			for _, intro := range []string{"Start the server manually:", "Dolt server may not be running. Try:"} {
				if strings.Contains(got, intro) {
					t.Errorf("output kept the lead-in %q:\n%s", intro, got)
				}
			}
		})
	}
}

func TestRedirectDoltStartHint_PassesOtherOutputThrough(t *testing.T) {
	in := "Error: issue gt-abc not found\nRun 'bd list' to see issues"
	if got := RedirectDoltStartHint(in); got != in {
		t.Errorf("output without the hint changed: %q", got)
	}
}

func TestWrapError_RedirectsDoltStartHint(t *testing.T) {
	stderr := "Error: failed to open database: Dolt server unreachable at 127.0.0.1:3307\n\n" +
		"Dolt server auto-start is disabled (dolt.auto-start: false).\nStart the server manually:\n  bd dolt start\n"
	err := New(t.TempDir()).wrapError(nil, stderr, []string{"list", "--json"})
	if err == nil {
		t.Fatal("wrapError returned nil")
	}
	if strings.Contains(err.Error(), "bd dolt start") {
		t.Errorf("wrapped error still advises bd dolt start: %v", err)
	}
	if !strings.Contains(err.Error(), "gt dolt status") {
		t.Errorf("wrapped error lacks the Gas Town guidance: %v", err)
	}
}

func TestHintWriter_RewritesAcrossSplitWrites(t *testing.T) {
	in := "Error: Dolt server unreachable\n\nDolt server auto-start is disabled (dolt.auto-start: false).\nStart the server manually:\n  bd dolt start\nafter\n"
	// Split mid-token so a per-write rewrite would miss "bd dolt start".
	for _, cut := range []int{5, strings.Index(in, "bd dolt") + 4, len(in) - 3} {
		var buf bytes.Buffer
		w := NewHintWriter(&buf)
		for _, chunk := range []string{in[:cut], in[cut:]} {
			if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
				t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(chunk))
			}
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if want := RedirectDoltStartHint(in); buf.String() != want {
			t.Errorf("cut %d:\n got %q\nwant %q", cut, buf.String(), want)
		}
	}
}

func TestHintWriter_FlushEmitsUnterminatedLine(t *testing.T) {
	var buf bytes.Buffer
	w := NewHintWriter(&buf)
	_, _ = w.Write([]byte("plain error"))
	if buf.Len() != 0 {
		t.Fatalf("partial line forwarded before Flush: %q", buf.String())
	}
	_ = w.Flush()
	if buf.String() != "plain error" {
		t.Errorf("Flush wrote %q", buf.String())
	}
}

func TestHintWriter_GuidanceOncePerStream(t *testing.T) {
	var buf bytes.Buffer
	w := NewHintWriter(&buf)
	_, _ = w.Write([]byte("a\n  bd dolt start\nb\n  bd dolt start\n"))
	_ = w.Flush()
	if got := strings.Count(buf.String(), DoltStartGuidance); got != 1 {
		t.Errorf("guidance appears %d times:\n%s", got, buf.String())
	}
}

// captureStderr swaps os.Stderr for a pipe while fn runs.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	return <-done
}

func TestRunRelayed_RewritesStderr(t *testing.T) {
	const script = "printf 'unreachable\\nStart the server manually:\\n  bd dolt start' >&2; exit 3"
	got := captureStderr(t, func() {
		err := RunRelayed(exec.Command("sh", "-c", script))
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
			t.Errorf("RunRelayed err = %v, want exit 3", err)
		}
	})
	if strings.Contains(got, "bd dolt start") {
		t.Errorf("relayed stderr still advises bd dolt start:\n%s", got)
	}
	if !strings.Contains(got, "unreachable") || !strings.Contains(got, DoltStartGuidance) {
		t.Errorf("relayed stderr lost the error or the guidance:\n%s", got)
	}
}

func TestOutputRelayed_KeepsStdoutAndRewritesStderr(t *testing.T) {
	var out []byte
	got := captureStderr(t, func() {
		var err error
		out, err = OutputRelayed(exec.Command("sh", "-c", "echo data; echo '  bd dolt start' >&2"))
		if err != nil {
			t.Error(err)
		}
	})
	if string(out) != "data\n" {
		t.Errorf("stdout = %q", out)
	}
	if strings.TrimSpace(got) != DoltStartGuidance {
		t.Errorf("stderr = %q", got)
	}
}

func TestRelayStderr_LeavesCallerWriterAlone(t *testing.T) {
	var buf bytes.Buffer
	c := exec.Command("sh", "-c", "echo '  bd dolt start' >&2")
	c.Stderr = &buf
	RelayStderr(c)()
	if c.Stderr != &buf {
		t.Fatal("RelayStderr replaced a caller-supplied stderr")
	}
}

// A bd that leaves a long-lived child holding its stderr (an auto-started dolt
// sql-server) must not keep the relay waiting once bd itself has exited.
func stubBdWithLingeringChild() *exec.Cmd {
	return exec.Command("sh", "-c", "sleep 4 >/dev/null & echo bd-done >&2")
}

func assertReturnsPromptly(t *testing.T, name string, run func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("%s: exit 0 with a lingering child reported %v", name, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s still blocked 2s after bd exited: a child holding stderr hangs the relay", name)
	}
}

func TestRunRelayed_LingeringChildDoesNotHang(t *testing.T) {
	assertReturnsPromptly(t, "RunRelayed", func() error {
		return RunRelayed(stubBdWithLingeringChild())
	})
}

func TestOutputRelayed_LingeringChildDoesNotHang(t *testing.T) {
	assertReturnsPromptly(t, "OutputRelayed", func() error {
		_, err := OutputRelayed(stubBdWithLingeringChild())
		return err
	})
}

func TestRelayStderr_LingeringChildDoesNotHang(t *testing.T) {
	assertReturnsPromptly(t, "RelayStderr", func() error {
		c := stubBdWithLingeringChild()
		c.Stderr = os.Stderr
		defer RelayStderr(c)()
		return TrimWaitDelay(c.Run())
	})
}

// A failing bd keeps its own error; only the "I/O still open" report is dropped.
func TestTrimWaitDelay(t *testing.T) {
	if err := TrimWaitDelay(exec.ErrWaitDelay); err != nil {
		t.Errorf("ErrWaitDelay not dropped: %v", err)
	}
	exit := exec.Command("sh", "-c", "exit 3").Run()
	if got := TrimWaitDelay(exit); got != exit {
		t.Errorf("exit error changed: %v", got)
	}
	if TrimWaitDelay(nil) != nil {
		t.Error("nil changed")
	}
}
