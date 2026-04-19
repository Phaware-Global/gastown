// Package transport implements L1 of the Telegraph pipeline: the HTTP webhook
// listener. It authenticates inbound requests via the provider Translator,
// then enqueues authenticated RawEvents onto a bounded channel for L2 dispatch.
// Unauthenticated or backpressured requests are rejected and logged.
package transport

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
)

// maxBodyRead is a hard upper bound on bytes read from the request body,
// preventing memory exhaustion on oversized payloads. The BodyCap config
// field governs how much of this is forwarded to the mail body (in L3).
const maxBodyRead = 1 << 20 // 1 MiB

// Server is the L1 HTTP webhook listener.
// It routes POST /webhook/{provider} requests to the appropriate Translator,
// authenticates them, and enqueues RawEvents onto a bounded channel.
type Server struct {
	translators map[string]telegraph.Translator
	rawCh       chan telegraph.RawEvent
	logger      *log.Logger
	httpServer  *http.Server
}

// New creates a Server.
//
// translators is the slice of Translators to register, keyed internally by
// Provider(). bufferSize sets the RawEvent channel capacity; when full,
// new requests are rejected with HTTP 503 (backpressure).
func New(addr string, translators []telegraph.Translator, bufferSize int, logger *log.Logger) *Server {
	tm := make(map[string]telegraph.Translator, len(translators))
	for _, t := range translators {
		tm[t.Provider()] = t
	}
	s := &Server{
		translators: tm,
		rawCh:       make(chan telegraph.RawEvent, bufferSize),
		logger:      logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/", s.handleWebhook)
	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return s
}

// RawEvents returns the channel onto which authenticated events are enqueued.
// The caller (L2 dispatcher) should drain this channel continuously.
// The channel is closed when Run returns.
func (s *Server) RawEvents() <-chan telegraph.RawEvent {
	return s.rawCh
}

// Run starts the HTTP server and blocks until ctx is cancelled or a fatal
// listener error occurs. On context cancellation, it performs a graceful
// shutdown: stops accepting new connections, waits up to 10 s for in-flight
// handlers, then closes the RawEvent channel.
func (s *Server) Run(ctx context.Context) error {
	listenErr := make(chan error, 1)
	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			listenErr <- err
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		runErr = s.httpServer.Shutdown(shutCtx)
	case err := <-listenErr:
		runErr = err
	}
	close(s.rawCh)
	return runErr
}

// handleWebhook handles POST /webhook/{provider}.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Path must be exactly /webhook/{provider} — no sub-paths.
	provider := strings.TrimPrefix(r.URL.Path, "/webhook/")
	if provider == "" || strings.ContainsRune(provider, '/') {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	sourceIP := remoteIP(r.RemoteAddr)

	t, ok := s.translators[provider]
	if !ok {
		s.logReject(provider, sourceIP, "provider_disabled", "")
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyRead))
	if err != nil {
		s.logReject(provider, sourceIP, "parse_error", "")
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	headers := lowerHeaders(r.Header)

	if err := t.Authenticate(headers, body); err != nil {
		s.logReject(provider, sourceIP, "hmac_invalid", "")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	evt := telegraph.RawEvent{
		Provider:   provider,
		Headers:    headers,
		Body:       body,
		SourceIP:   sourceIP,
		ReceivedAt: time.Now().UTC(),
	}

	select {
	case s.rawCh <- evt:
		s.logAccept(provider, sourceIP, "")
		w.WriteHeader(http.StatusOK)
	default:
		s.logReject(provider, sourceIP, "backpressure", "")
		http.Error(w, "server busy", http.StatusServiceUnavailable)
	}
}

// lowerHeaders returns a copy of h with all keys lowercased and only the
// first value per key retained, matching the RawEvent.Headers contract.
func lowerHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) > 0 {
			out[strings.ToLower(k)] = vs[0]
		}
	}
	return out
}

// remoteIP extracts the host portion of addr (stripping any port).
func remoteIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// logEntry is the structured JSON shape for all Telegraph L1 log events.
type logEntry struct {
	TS        string `json:"ts"`
	Component string `json:"component"`
	Event     string `json:"event"`
	Provider  string `json:"provider"`
	SourceIP  string `json:"source_ip"`
	Reason    string `json:"reason,omitempty"`
	EventID   string `json:"event_id,omitempty"`
}

func (s *Server) logAccept(provider, sourceIP, eventID string) {
	s.emit(logEntry{
		TS:        time.Now().UTC().Format(time.RFC3339),
		Component: "telegraph",
		Event:     "accept",
		Provider:  provider,
		SourceIP:  sourceIP,
		EventID:   eventID,
	})
}

func (s *Server) logReject(provider, sourceIP, reason, eventID string) {
	s.emit(logEntry{
		TS:        time.Now().UTC().Format(time.RFC3339),
		Component: "telegraph",
		Event:     "reject",
		Provider:  provider,
		SourceIP:  sourceIP,
		Reason:    reason,
		EventID:   eventID,
	})
}

func (s *Server) emit(e logEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		s.logger.Printf("telegraph: marshal error: %v", err)
		return
	}
	s.logger.Printf("%s", b)
}
