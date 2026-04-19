package telegraph

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// Run starts the Telegraph pipeline: L1 HTTP listener → dispatch loop.
// It blocks until ctx is canceled, then gracefully shuts down the HTTP server
// and drains in-flight events.
//
// translators maps provider name → Translator. Pass an empty map when no L2
// providers are registered yet; unknown providers will be rejected with 404.
// dispatcher handles L2→L3 routing; nil means events are logged and discarded.
func Run(ctx context.Context, cfg *TelegraphConfig, translators map[string]Translator, dispatcher Dispatcher, logf LogFunc) {
	rawCh := make(chan RawEvent, cfg.BufferSize)

	handler := newHTTPHandler(translators, rawCh, logf)
	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: handler.mux(),
	}

	// Start HTTP server in background goroutine.
	srvErr := make(chan error, 1)
	go func() {
		logf("telegraph: L1 HTTP listener starting on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	// Dispatch loop: drain rawCh and route through L2→L3.
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		dispatchLoop(ctx, rawCh, dispatcher, logf)
	}()

	// Wait for context cancellation or server error.
	select {
	case <-ctx.Done():
		logf("telegraph: context canceled, shutting down")
	case err := <-srvErr:
		logf("telegraph: HTTP server error: %v", err)
	}

	// Graceful shutdown: stop accepting requests, drain in-flight buffer.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logf("telegraph: HTTP server shutdown error: %v", err)
	}

	// Wait for the dispatch loop to drain and exit.
	<-dispatchDone
	logf("telegraph: shutdown complete")
}

// dispatchLoop reads RawEvents from rawCh and calls dispatcher.Dispatch until
// ctx is canceled and the channel is drained.
func dispatchLoop(ctx context.Context, rawCh <-chan RawEvent, dispatcher Dispatcher, logf LogFunc) {
	for {
		select {
		case ev, ok := <-rawCh:
			if !ok {
				return
			}
			if dispatcher == nil {
				// L2 not yet wired: log and discard.
				logf("telegraph: dispatch stub drop provider=%s source_ip=%s", ev.Provider, ev.SourceIP)
				continue
			}
			if err := dispatcher.Dispatch(ev); err != nil {
				emitReject(logf, ev.Provider, ev.SourceIP, "dispatch_error", "")
			}
		case <-ctx.Done():
			// Drain remaining events before returning.
			for {
				select {
				case ev, ok := <-rawCh:
					if !ok {
						return
					}
					if dispatcher != nil {
						_ = dispatcher.Dispatch(ev)
					}
				default:
					return
				}
			}
		}
	}
}

// emitReject writes a structured reject log line.
func emitReject(logf LogFunc, provider, sourceIP, reason, eventID string) {
	line := struct {
		TS        string `json:"ts"`
		Component string `json:"component"`
		Event     string `json:"event"`
		Provider  string `json:"provider"`
		SourceIP  string `json:"source_ip,omitempty"`
		Reason    string `json:"reason"`
		EventID   string `json:"event_id,omitempty"`
	}{
		TS:        time.Now().UTC().Format(time.RFC3339),
		Component: "telegraph",
		Event:     "reject",
		Provider:  provider,
		SourceIP:  sourceIP,
		Reason:    reason,
		EventID:   eventID,
	}
	if data, err := json.Marshal(line); err == nil {
		logf("%s", string(data))
	} else {
		log.Printf("telegraph: log marshal error: %v", err)
	}
}
