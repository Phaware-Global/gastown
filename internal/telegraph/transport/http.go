// Package transport implements L1 of the Telegraph pipeline: an HTTP webhook
// listener that authenticates incoming requests and enqueues RawEvents for L2.
package transport

import (
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
	"github.com/steveyegge/gastown/internal/telegraph/tlog"
)

const bodyReadLimit = 1 << 20 // 1 MiB

// Handler is an http.Handler that routes POST /webhook/<provider> requests,
// authenticates them via the appropriate Translator, and enqueues RawEvents.
type Handler struct {
	translators map[string]telegraph.Translator // keyed by provider ID
	rawCh       chan<- telegraph.RawEvent
	log         *tlog.Logger // nil disables logging
}

// NewHandler creates a Handler for the given translators and raw-event channel.
// logger may be nil to disable structured logging.
func NewHandler(translators []telegraph.Translator, rawCh chan<- telegraph.RawEvent, logger *tlog.Logger) *Handler {
	m := make(map[string]telegraph.Translator, len(translators))
	for _, t := range translators {
		m[t.Provider()] = t
	}
	return &Handler{
		translators: m,
		rawCh:       rawCh,
		log:         logger,
	}
}

// NewHandlerWithWriter creates a Handler using a plain io.Writer for logging.
// Convenience constructor for callers that don't need counter access.
func NewHandlerWithWriter(translators []telegraph.Translator, rawCh chan<- telegraph.RawEvent, w io.Writer) *Handler {
	var logger *tlog.Logger
	if w != nil {
		logger = tlog.New(w)
	}
	return NewHandler(translators, rawCh, logger)
}

// ServeHTTP handles POST /webhook/<provider>.
// Success: enqueues RawEvent, writes HTTP 200.
// Failures: logs a reject entry and writes an appropriate 4xx/5xx status.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Parse provider from /webhook/<provider>. No sub-paths accepted.
	after, ok := strings.CutPrefix(r.URL.Path, "/webhook/")
	if !ok || after == "" || strings.Contains(after, "/") {
		http.NotFound(w, r)
		return
	}
	provider := after
	sourceIP := remoteIP(r.RemoteAddr)

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	t, exists := h.translators[provider]
	if !exists {
		h.log.Reject(provider, sourceIP, tlog.ReasonProviderDisabled, "")
		http.NotFound(w, r)
		return
	}

	// Read up to bodyReadLimit bytes. Use LimitedReader so we can detect
	// truncation: if N reaches 0 the body was at or beyond the limit.
	lr := &io.LimitedReader{R: r.Body, N: bodyReadLimit}
	body, err := io.ReadAll(lr)
	if err != nil {
		h.log.Reject(provider, sourceIP, tlog.ReasonParseError, "")
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if lr.N == 0 {
		// Body was exactly at or exceeded the limit; reject rather than
		// silently forwarding a truncated (and HMAC-mismatching) payload.
		h.log.Reject(provider, sourceIP, tlog.ReasonParseError, "")
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	headers := lowerHeaders(r.Header)

	if err := t.Authenticate(headers, body); err != nil {
		h.log.Reject(provider, sourceIP, tlog.ReasonHMACInvalid, "")
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

	// Non-blocking send: if the channel is full, reject with 503 so the
	// provider (e.g. Jira) can retry with its own backoff.
	select {
	case h.rawCh <- evt:
	default:
		h.log.Reject(provider, sourceIP, tlog.ReasonBackpressure, "")
		http.Error(w, "server busy, retry later", http.StatusServiceUnavailable)
		return
	}

	// Event ID is not available at L1 (extracted by L2 during translation).
	h.log.Accept(provider, sourceIP, "", len(body), time.Since(start).Milliseconds())
	w.WriteHeader(http.StatusOK)
}

// remoteIP extracts the IP address from an "ip:port" RemoteAddr string.
// Falls back to the raw value if parsing fails.
func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// lowerHeaders returns a map of HTTP headers with all keys in lowercase.
// When a header has multiple values only the first is retained, matching
// the RawEvent contract of map[string]string.
func lowerHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) > 0 {
			out[strings.ToLower(k)] = vs[0]
		}
	}
	return out
}
