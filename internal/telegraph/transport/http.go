// Package transport implements L1 of the Telegraph pipeline: an HTTP webhook
// listener that authenticates incoming requests and enqueues RawEvents for L2.
package transport

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
)

// Handler is an http.Handler that routes POST /webhook/<provider> requests,
// authenticates them via the appropriate Translator, and enqueues RawEvents.
type Handler struct {
	translators map[string]telegraph.Translator // keyed by provider ID
	rawCh       chan<- telegraph.RawEvent
	logW        io.Writer // structured JSON log destination
}

// NewHandler creates a Handler for the given translators and raw-event channel.
// Only translators whose Provider() key is present are accepted; callers should
// exclude disabled providers before calling.
// logW receives one JSON line per accept/reject event. Pass os.Stderr if unset.
func NewHandler(translators []telegraph.Translator, rawCh chan<- telegraph.RawEvent, logW io.Writer) *Handler {
	m := make(map[string]telegraph.Translator, len(translators))
	for _, t := range translators {
		m[t.Provider()] = t
	}
	return &Handler{
		translators: m,
		rawCh:       rawCh,
		logW:        logW,
	}
}

// ServeHTTP handles POST /webhook/<provider>.
// Success: enqueues RawEvent, writes HTTP 200.
// Failures: logs a reject entry and writes an appropriate 4xx/5xx status.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Parse provider from /webhook/<provider>. No sub-paths accepted.
	after, ok := strings.CutPrefix(r.URL.Path, "/webhook/")
	if !ok || after == "" || strings.Contains(after, "/") {
		http.NotFound(w, r)
		return
	}
	provider := after
	sourceIP := r.RemoteAddr

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	t, exists := h.translators[provider]
	if !exists {
		h.writeReject(provider, sourceIP, "provider_disabled", "")
		http.NotFound(w, r)
		return
	}

	// Cap reads at 1 MiB to prevent OOM from oversized payloads.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		h.writeReject(provider, sourceIP, "parse_error", "")
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	headers := lowerHeaders(r.Header)

	if err := t.Authenticate(headers, body); err != nil {
		h.writeReject(provider, sourceIP, "hmac_invalid", "")
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
		h.writeReject(provider, sourceIP, "backpressure", "")
		http.Error(w, "server busy, retry later", http.StatusServiceUnavailable)
		return
	}

	// Event ID is not available at L1 (extracted by L2). Omit from accept log.
	h.writeAccept(provider, sourceIP, "")
	w.WriteHeader(http.StatusOK)
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

// logEntry is the structure emitted for every terminal outcome.
// Fields mirror the Observability spec in docs/design/telegraph.md.
type logEntry struct {
	TS        string `json:"ts"`
	Component string `json:"component"`
	Event     string `json:"event"`
	Provider  string `json:"provider"`
	SourceIP  string `json:"source_ip"`
	Reason    string `json:"reason,omitempty"`
	EventID   string `json:"event_id,omitempty"`
}

func (h *Handler) writeAccept(provider, sourceIP, eventID string) {
	h.writeEntry(logEntry{
		TS:        time.Now().UTC().Format(time.RFC3339),
		Component: "telegraph",
		Event:     "accept",
		Provider:  provider,
		SourceIP:  sourceIP,
		EventID:   eventID,
	})
}

func (h *Handler) writeReject(provider, sourceIP, reason, eventID string) {
	h.writeEntry(logEntry{
		TS:        time.Now().UTC().Format(time.RFC3339),
		Component: "telegraph",
		Event:     "reject",
		Provider:  provider,
		SourceIP:  sourceIP,
		Reason:    reason,
		EventID:   eventID,
	})
}

func (h *Handler) writeEntry(e logEntry) {
	data, err := json.Marshal(e)
	if err != nil {
		// Should never happen with this struct; fall back to plain text.
		fmt.Fprintf(h.logW, `{"component":"telegraph","event":"log_error","err":%q}`+"\n", err.Error())
		return
	}
	fmt.Fprintln(h.logW, string(data))
}
