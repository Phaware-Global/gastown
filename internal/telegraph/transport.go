package telegraph

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const maxBodyBytes = 1 << 20 // 1 MiB hard limit on request body reads

// httpHandler is the L1 HTTP handler for inbound webhooks.
// It routes requests to the matching Translator, authenticates, and enqueues.
type httpHandler struct {
	translators map[string]Translator // keyed by provider name
	rawCh       chan<- RawEvent       // bounded L1→L2 channel
	logf        LogFunc
}

// newHTTPHandler constructs an httpHandler.
func newHTTPHandler(translators map[string]Translator, rawCh chan<- RawEvent, logf LogFunc) *httpHandler {
	return &httpHandler{
		translators: translators,
		rawCh:       rawCh,
		logf:        logf,
	}
}

// ServeHTTP handles inbound webhook requests on /webhook/<provider>.
func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only POST is accepted.
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract provider from path: /webhook/<provider>
	provider := extractProvider(r.URL.Path)
	if provider == "" {
		http.Error(w, "invalid webhook path", http.StatusBadRequest)
		return
	}

	translator, ok := h.translators[provider]
	if !ok {
		// Unknown provider — reject with 404.
		h.reject(provider, r.RemoteAddr, "unknown_provider", "", http.StatusNotFound, w)
		return
	}

	// Read body with a hard cap to prevent memory exhaustion.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		h.reject(provider, r.RemoteAddr, "read_error", "", http.StatusInternalServerError, w)
		return
	}

	// Collect headers (lowercased keys) for the translator.
	headers := collectHeaders(r)

	// Authenticate via the provider translator (HMAC or other scheme).
	if err := translator.Authenticate(headers, body); err != nil {
		h.reject(provider, r.RemoteAddr, "hmac_invalid", "", http.StatusUnauthorized, w)
		return
	}

	// Build RawEvent and attempt non-blocking enqueue.
	ev := RawEvent{
		Provider:   provider,
		Headers:    headers,
		Body:       body,
		SourceIP:   remoteIP(r.RemoteAddr),
		ReceivedAt: time.Now().UTC(),
	}

	select {
	case h.rawCh <- ev:
		h.accept(provider, r.RemoteAddr)
		w.WriteHeader(http.StatusOK)
	default:
		// Channel full — apply backpressure; Jira will retry.
		h.reject(provider, r.RemoteAddr, "backpressure", "", http.StatusServiceUnavailable, w)
	}
}

// mux returns an http.ServeMux rooted at /webhook/.
func (h *httpHandler) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/webhook/", h)
	return mux
}

// accept emits a structured accept log line.
func (h *httpHandler) accept(provider, remoteAddr string) {
	line := logLine{
		TS:        time.Now().UTC().Format(time.RFC3339),
		Component: "telegraph",
		Event:     "accept",
		Provider:  provider,
		SourceIP:  remoteIP(remoteAddr),
	}
	h.emitLog(line)
}

// reject emits a structured reject log line and writes the HTTP error.
func (h *httpHandler) reject(provider, remoteAddr, reason, eventID string, status int, w http.ResponseWriter) {
	line := logLine{
		TS:        time.Now().UTC().Format(time.RFC3339),
		Component: "telegraph",
		Event:     "reject",
		Provider:  provider,
		SourceIP:  remoteIP(remoteAddr),
		Reason:    reason,
		EventID:   eventID,
	}
	h.emitLog(line)
	http.Error(w, reason, status)
}

// emitLog marshals v to a single-line JSON log entry via h.logf.
func (h *httpHandler) emitLog(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("telegraph: log marshal error: %v", err)
		return
	}
	h.logf("%s", string(data))
}

// logLine is the common structured log record for all telegraph events.
type logLine struct {
	TS        string `json:"ts"`
	Component string `json:"component"`
	Event     string `json:"event"`
	Provider  string `json:"provider"`
	SourceIP  string `json:"source_ip,omitempty"`
	Reason    string `json:"reason,omitempty"`
	EventID   string `json:"event_id,omitempty"`
	EventType string `json:"event_type,omitempty"`
	Actor     string `json:"actor,omitempty"`
	Subject   string `json:"subject,omitempty"`
	MailID    string `json:"mail_id,omitempty"`
}

// extractProvider parses the provider name from a path like /webhook/<provider>.
func extractProvider(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || parts[0] != "webhook" || parts[1] == "" {
		return ""
	}
	return parts[1]
}

// collectHeaders returns a lowercased-key copy of the request headers.
func collectHeaders(r *http.Request) map[string]string {
	out := make(map[string]string, len(r.Header))
	for k, vs := range r.Header {
		if len(vs) > 0 {
			out[strings.ToLower(k)] = vs[0]
		}
	}
	return out
}

// remoteIP strips the port from a "host:port" remote address string.
func remoteIP(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i]
	}
	return addr
}
