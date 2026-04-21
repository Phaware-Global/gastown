package transport_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
	"github.com/steveyegge/gastown/internal/telegraph/tlog"
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

// makeHandlerWithLogger returns a Handler wired to a fresh tlog.Logger and buffer.
func makeHandlerWithLogger(t *testing.T, translators []telegraph.Translator, chSize int) (*transport.Handler, chan telegraph.RawEvent, *tlog.Logger, *bytes.Buffer) {
	t.Helper()
	rawCh := make(chan telegraph.RawEvent, chSize)
	logBuf := &bytes.Buffer{}
	logger := tlog.New(logBuf)
	h := transport.NewHandler(translators, rawCh, logger)
	return h, rawCh, logger, logBuf
}

// makeHandler is a backward-compatible helper using the Writer convenience constructor.
func makeHandler(t *testing.T, translators []telegraph.Translator, chSize int) (*transport.Handler, chan telegraph.RawEvent, *bytes.Buffer) {
	t.Helper()
	rawCh := make(chan telegraph.RawEvent, chSize)
	logBuf := &bytes.Buffer{}
	h := transport.NewHandlerWithWriter(translators, rawCh, logBuf)
	return h, rawCh, logBuf
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// logLines parses the log buffer into a slice of JSON objects.
func logLines(buf *bytes.Buffer) []map[string]interface{} {
	var out []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

func strField(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func TestHandler_Accept(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	h, rawCh, _, logBuf := makeHandlerWithLogger(t, []telegraph.Translator{tr}, 8)

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
	if strField(lines[0], "event") != "accept" {
		t.Errorf("log event = %q, want accept", strField(lines[0], "event"))
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
	if strField(lines[0], "event") != "reject" || strField(lines[0], "reason") != "hmac_invalid" {
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
	if strField(lines[0], "reason") != "provider_disabled" {
		t.Errorf("unexpected reason: %v", strField(lines[0], "reason"))
	}
}

func TestHandler_Backpressure(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	// Channel size 0 → immediately full.
	rawCh := make(chan telegraph.RawEvent, 0)
	logBuf := &bytes.Buffer{}
	h := transport.NewHandlerWithWriter([]telegraph.Translator{tr}, rawCh, logBuf)

	req := httptest.NewRequest(http.MethodPost, "/webhook/jira", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}

	lines := logLines(logBuf)
	if len(lines) != 1 || strField(lines[0], "reason") != "backpressure" {
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
	tr2 := &capturingTranslator{fakeTranslator: &fakeTranslator{provider: "jira"}}
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
	for k := range evt.Headers {
		if k != strings.ToLower(k) {
			t.Errorf("header key %q is not lowercase", k)
		}
	}
	if evt.Headers["x-hub-signature"] == "" {
		t.Error("x-hub-signature header missing or not lowercased")
	}
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
		if strField(line, field) == "" {
			t.Errorf("log field %q missing or empty", field)
		}
	}
	if strField(line, "component") != "telegraph" {
		t.Errorf("component = %q, want telegraph", strField(line, "component"))
	}
}

func TestHandler_LogBytesLenAndLatency(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	h, _, _, logBuf := makeHandlerWithLogger(t, []telegraph.Translator{tr}, 8)

	const body = `{"webhookEvent":"jira:issue_created"}`
	post(h, "/webhook/jira", body)

	lines := logLines(logBuf)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	line := lines[0]

	bytesLen, ok := line["bytes_len"].(float64)
	if !ok || bytesLen == 0 {
		t.Errorf("bytes_len missing or zero: %v", line["bytes_len"])
	}
	if int(bytesLen) != len(body) {
		t.Errorf("bytes_len = %v, want %d", bytesLen, len(body))
	}
	if _, ok := line["latency_ms"]; !ok {
		t.Error("latency_ms field missing from accept log entry")
	}
}

func TestHandler_Counters(t *testing.T) {
	trOK := &fakeTranslator{provider: "jira"}
	trFail := &fakeTranslator{provider: "jira", authErr: errors.New("bad sig")}

	t.Run("accept increments counter", func(t *testing.T) {
		rawCh := make(chan telegraph.RawEvent, 8)
		logger := tlog.New(&bytes.Buffer{})
		h := transport.NewHandler([]telegraph.Translator{trOK}, rawCh, logger)
		post(h, "/webhook/jira", `{}`)
		if v := logger.Counters.Accept.Load(); v != 1 {
			t.Errorf("Accept counter = %d, want 1", v)
		}
	})

	t.Run("hmac_invalid increments counter", func(t *testing.T) {
		rawCh := make(chan telegraph.RawEvent, 8)
		logger := tlog.New(&bytes.Buffer{})
		h := transport.NewHandler([]telegraph.Translator{trFail}, rawCh, logger)
		post(h, "/webhook/jira", `{}`)
		if v := logger.Counters.RejectHMACInvalid.Load(); v != 1 {
			t.Errorf("RejectHMACInvalid counter = %d, want 1", v)
		}
	})

	t.Run("provider_disabled increments counter", func(t *testing.T) {
		rawCh := make(chan telegraph.RawEvent, 8)
		logger := tlog.New(&bytes.Buffer{})
		h := transport.NewHandler(nil, rawCh, logger)
		post(h, "/webhook/jira", `{}`)
		if v := logger.Counters.RejectProviderDis.Load(); v != 1 {
			t.Errorf("RejectProviderDis counter = %d, want 1", v)
		}
	})

	t.Run("backpressure increments counter", func(t *testing.T) {
		rawCh := make(chan telegraph.RawEvent, 0) // always full
		logger := tlog.New(&bytes.Buffer{})
		h := transport.NewHandler([]telegraph.Translator{trOK}, rawCh, logger)
		post(h, "/webhook/jira", `{}`)
		if v := logger.Counters.RejectBackpressure.Load(); v != 1 {
			t.Errorf("RejectBackpressure counter = %d, want 1", v)
		}
	})
}

func TestHandler_NilLogger_NoPanic(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	rawCh := make(chan telegraph.RawEvent, 8)
	h := transport.NewHandler([]telegraph.Translator{tr}, rawCh, nil)

	// All code paths with nil logger must not panic.
	post(h, "/webhook/jira", `{}`)                   // accept
	post(h, "/webhook/unknown", `{}`)                // provider_disabled
	_ = <-rawCh
}

// capturingTranslator wraps a fakeTranslator but always succeeds auth.
type capturingTranslator struct {
	*fakeTranslator
	lastHeaders map[string]string
}

func (c *capturingTranslator) Authenticate(headers map[string]string, _ []byte) error {
	c.lastHeaders = headers
	return nil
}

// Ensure Handler satisfies http.Handler at compile time.
var _ http.Handler = (*transport.Handler)(nil)
