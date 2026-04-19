package telegraph

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// noopDispatcher implements Dispatcher, recording dispatched events.
type noopDispatcher struct {
	events []RawEvent
	err    error
}

func (d *noopDispatcher) Dispatch(ev RawEvent) error {
	d.events = append(d.events, ev)
	return d.err
}

func TestDispatchLoop_NilDispatcher_Drops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rawCh := make(chan RawEvent, 2)
	logLines := []string{}
	logf := func(format string, args ...any) {
		logLines = append(logLines, fmt.Sprintf(format, args...))
	}

	rawCh <- RawEvent{Provider: "jira", SourceIP: "1.2.3.4"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatchLoop(ctx, rawCh, nil, logf)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	if len(logLines) == 0 {
		t.Error("expected at least one log line for stub drop")
	}
}

func TestDispatchLoop_WithDispatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rawCh := make(chan RawEvent, 2)
	logf := func(format string, args ...any) {}

	disp := &noopDispatcher{}
	rawCh <- RawEvent{Provider: "jira", SourceIP: "1.2.3.4"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatchLoop(ctx, rawCh, disp, logf)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	if len(disp.events) == 0 {
		t.Error("expected at least one dispatched event")
	}
	if disp.events[0].Provider != "jira" {
		t.Errorf("expected provider jira, got %s", disp.events[0].Provider)
	}
}

func TestDispatchLoop_DrainOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rawCh := make(chan RawEvent, 5)
	logf := func(format string, args ...any) {}

	disp := &noopDispatcher{}
	// Pre-fill the channel before starting the loop.
	for i := 0; i < 3; i++ {
		rawCh <- RawEvent{Provider: "jira"}
	}

	cancel() // cancel before starting — loop should drain all 3

	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatchLoop(ctx, rawCh, disp, logf)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch loop did not exit")
	}

	if len(disp.events) != 3 {
		t.Errorf("expected 3 drained events, got %d", len(disp.events))
	}
}

func TestRun_StartsAndStops(t *testing.T) {
	cfg := &TelegraphConfig{
		ListenAddr: "127.0.0.1:0",
		BufferSize: 4,
	}

	// Find an available port.
	const port = 19876
	cfg.ListenAddr = fmt.Sprintf("127.0.0.1:%d", port)

	ctx, cancel := context.WithCancel(context.Background())
	logf := func(format string, args ...any) {}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, cfg, map[string]Translator{}, nil, logf)
	}()

	// Give the server time to bind.
	time.Sleep(50 * time.Millisecond)

	// Verify it's listening.
	resp, err := http.Post(fmt.Sprintf("http://%s/webhook/unknown", cfg.ListenAddr), "application/json", nil)
	if err != nil {
		t.Fatalf("could not reach telegraph listener: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == 0 {
		t.Error("expected a real HTTP status")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after context cancel")
	}
}
