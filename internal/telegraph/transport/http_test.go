package transport_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
	"github.com/steveyegge/gastown/internal/telegraph/transport"
)

// fakeTranslator is a Translator for tests.
type fakeTranslator struct {
	provider    string
	authErr     error
	translateFn func([]byte) (*telegraph.NormalizedEvent, error)
}

func (f *fakeTranslator) Provider() string { return f.provider }
func (f *fakeTranslator) Authenticate(_ map[string]string, _ []byte) error {
	return f.authErr
}
func (f *fakeTranslator) Translate(body []byte) (*telegraph.NormalizedEvent, error) {
	if f.translateFn != nil {
		return f.translateFn(body)
	}
	return &telegraph.NormalizedEvent{Provider: f.provider, EventType: "test.event"}, nil
}

func makeHandler(t *testing.T, translators []telegraph.Translator, chSize int) (*transport.Handler, chan telegraph.RawEvent, *bytes.Buffer) {
	t.Helper()
	rawCh := make(chan telegraph.RawEvent, chSize)
	logBuf := &bytes.Buffer{}
	h := transport.NewHandler(translators, rawCh, logBuf)
	return h, rawCh, logBuf
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// logLines parses the log buffer into a slice of JSON objects.
func logLines(buf *bytes.Buffer) []map[string]string {
	var out []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

func TestHandler_Accept(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	h, rawCh, logBuf := makeHandler(t, []telegraph.Translator{tr}, 8)

	w := post(h, "/webhook/jira", `{"webhookEvent":"jira:issue_created"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	select {
	case evt := <-rawCh:
		if evt.Provider != "jira" {
			t.Errorf("provider = %q, want jira", evt.Provider)
		}
		if evt.ReceivedAt.IsZero() {
			t.Error("ReceivedAt not set")
		}
	case <-time.After(time.Second):
		t.Fatal("no RawEvent enqueued")
	}

	lines := logLines(logBuf)
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d", len(lines))
	}
	if lines[0]["event"] != "accept" {
		t.Errorf("log event = %q, want accept", lines[0]["event"])
	}
}

func TestHandler_AuthFailure(t *testing.T) {
	tr := &fakeTranslator{provider: "jira", authErr: errors.New("bad sig")}
	h, rawCh, logBuf := makeHandler(t, []telegraph.Translator{tr}, 8)

	w := post(h, "/webhook/jira", `{}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	if len(rawCh) != 0 {
		t.Error("event should not be enqueued on auth failure")
	}

	lines := logLines(logBuf)
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d", len(lines))
	}
	if lines[0]["event"] != "reject" || lines[0]["reason"] != "hmac_invalid" {
		t.Errorf("unexpected log: %v", lines[0])
	}
}

func TestHandler_UnknownProvider(t *testing.T) {
	h, rawCh, logBuf := makeHandler(t, nil, 8)

	w := post(h, "/webhook/github", `{}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
	if len(rawCh) != 0 {
		t.Error("event should not be enqueued for unknown provider")
	}

	lines := logLines(logBuf)
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d", len(lines))
	}
	if lines[0]["reason"] != "provider_disabled" {
		t.Errorf("unexpected reason: %v", lines[0]["reason"])
	}
}

func TestHandler_Backpressure(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	// Channel size 0 → immediately full.
	rawCh := make(chan telegraph.RawEvent, 0)
	logBuf := &bytes.Buffer{}
	h := transport.NewHandler([]telegraph.Translator{tr}, rawCh, logBuf)

	req := httptest.NewRequest(http.MethodPost, "/webhook/jira", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}

	lines := logLines(logBuf)
	if len(lines) != 1 || lines[0]["reason"] != "backpressure" {
		t.Errorf("unexpected log: %v", lines)
	}
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	h, _, _ := makeHandler(t, []telegraph.Translator{tr}, 8)

	req := httptest.NewRequest(http.MethodGet, "/webhook/jira", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestHandler_BadPaths(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	h, _, _ := makeHandler(t, []telegraph.Translator{tr}, 8)

	for _, path := range []string{
		"/webhook/",
		"/webhook",
		"/webhook/jira/extra",
		"/other/path",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("path %q: want 404, got %d", path, w.Code)
		}
	}
}

func TestHandler_HeadersLowercased(t *testing.T) {
	var gotHeaders map[string]string
	tr := &fakeTranslator{
		provider: "jira",
		// Capture headers by inspecting through Authenticate (headers are passed in).
	}
	// Override Authenticate to capture headers.
	tr2 := &capturingTranslator{fakeTranslator: tr}
	h, rawCh, _ := makeHandler(t, []telegraph.Translator{tr2}, 8)

	req := httptest.NewRequest(http.MethodPost, "/webhook/jira", strings.NewReader(`{}`))
	req.Header.Set("X-Hub-Signature", "sha256=abc")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	evt := <-rawCh
	gotHeaders = evt.Headers

	for k := range gotHeaders {
		if k != strings.ToLower(k) {
			t.Errorf("header key %q is not lowercase", k)
		}
	}
	if gotHeaders["x-hub-signature"] == "" {
		t.Error("x-hub-signature header missing or not lowercased")
	}
	_ = gotHeaders
}

func TestHandler_BodyPreserved(t *testing.T) {
	const payload = `{"webhookEvent":"jira:issue_created","issue":{"key":"PROJ-1"}}`
	tr := &fakeTranslator{provider: "jira"}
	h, rawCh, _ := makeHandler(t, []telegraph.Translator{tr}, 8)

	post(h, "/webhook/jira", payload)

	evt := <-rawCh
	if string(evt.Body) != payload {
		t.Errorf("body = %q, want %q", evt.Body, payload)
	}
}

func TestHandler_LogFieldsPresent(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	h, _, logBuf := makeHandler(t, []telegraph.Translator{tr}, 8)

	post(h, "/webhook/jira", `{}`)

	lines := logLines(logBuf)
	if len(lines) == 0 {
		t.Fatal("no log output")
	}
	line := lines[0]
	for _, field := range []string{"ts", "component", "event", "provider", "source_ip"} {
		if line[field] == "" {
			t.Errorf("log field %q missing or empty", field)
		}
	}
	if line["component"] != "telegraph" {
		t.Errorf("component = %q, want telegraph", line["component"])
	}
}

// capturingTranslator wraps a fakeTranslator but always succeeds auth.
type capturingTranslator struct {
	*fakeTranslator
	lastHeaders map[string]string
}

func (c *capturingTranslator) Authenticate(headers map[string]string, body []byte) error {
	c.lastHeaders = headers
	return nil
}

// Ensure Handler satisfies http.Handler at compile time.
var _ http.Handler = (*transport.Handler)(nil)

// Ensure io.Writer is accepted (compile-time check via io.Discard).
var _ io.Writer = io.Discard

// ---- fixes for PR #22 review comments ----

func TestHandler_NilLogWDefaultsToStderr(t *testing.T) {
	// NewHandler with nil logW must not panic; it falls back to os.Stderr.
	tr := &fakeTranslator{provider: "jira"}
	rawCh := make(chan telegraph.RawEvent, 8)
	h := transport.NewHandler([]telegraph.Translator{tr}, rawCh, nil)

	w := post(h, "/webhook/jira", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (nil logW should not panic)", w.Code)
	}
}

func TestHandler_SourceIPStripsPort(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	_, rawCh, logBuf := makeHandler(t, []telegraph.Translator{tr}, 8)

	req := httptest.NewRequest(http.MethodPost, "/webhook/jira", strings.NewReader(`{}`))
	// httptest sets RemoteAddr; override it to a realistic "ip:port" value.
	req.RemoteAddr = "203.0.113.42:54321"
	w := httptest.NewRecorder()
	transport.NewHandler([]telegraph.Translator{tr}, rawCh, logBuf).ServeHTTP(w, req)

	lines := logLines(logBuf)
	if len(lines) == 0 {
		t.Fatal("no log lines")
	}
	sourceIP := lines[0]["source_ip"]
	if strings.Contains(sourceIP, ":") {
		t.Errorf("source_ip contains port: %q — expected bare IP", sourceIP)
	}
	if sourceIP != "203.0.113.42" {
		t.Errorf("source_ip = %q, want 203.0.113.42", sourceIP)
	}
}

func TestHandler_SourceIPStripsPort_RawEventAlso(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	rawCh := make(chan telegraph.RawEvent, 8)
	logBuf := &bytes.Buffer{}
	h := transport.NewHandler([]telegraph.Translator{tr}, rawCh, logBuf)

	req := httptest.NewRequest(http.MethodPost, "/webhook/jira", strings.NewReader(`{}`))
	req.RemoteAddr = "198.51.100.7:12345"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	evt := <-rawCh
	if strings.Contains(evt.SourceIP, ":") {
		t.Errorf("RawEvent.SourceIP contains port: %q", evt.SourceIP)
	}
}

func TestHandler_OversizeBodyReturns413(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	_, rawCh, logBuf := makeHandler(t, []telegraph.Translator{tr}, 8)

	// Body exactly at the limit (1 MiB) should be rejected with 413.
	oversizeBody := strings.Repeat("x", 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/webhook/jira", strings.NewReader(oversizeBody))
	w := httptest.NewRecorder()
	transport.NewHandler([]telegraph.Translator{tr}, rawCh, logBuf).ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", w.Code)
	}
	if len(rawCh) != 0 {
		t.Error("oversize body must not be enqueued")
	}
	lines := logLines(logBuf)
	if len(lines) != 1 || lines[0]["reason"] != "parse_error" {
		t.Errorf("unexpected log: %v", lines)
	}
}

func TestHandler_UndersizeBodyAccepted(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	h, rawCh, _ := makeHandler(t, []telegraph.Translator{tr}, 8)

	// Body 1 byte under the limit must be accepted normally.
	body := strings.Repeat("x", (1<<20)-1)
	w := post(h, "/webhook/jira", body)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	evt := <-rawCh
	if len(evt.Body) != (1<<20)-1 {
		t.Errorf("body len = %d, want %d", len(evt.Body), (1<<20)-1)
	}
}
