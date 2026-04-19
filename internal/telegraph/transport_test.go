package telegraph

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubTranslator implements Translator for test purposes.
type stubTranslator struct {
	provider     string
	authErr      error
	translated   *NormalizedEvent
	translateErr error
}

func (s *stubTranslator) Provider() string { return s.provider }
func (s *stubTranslator) Authenticate(_ map[string]string, _ []byte) error {
	return s.authErr
}
func (s *stubTranslator) Translate(_ []byte) (*NormalizedEvent, error) {
	return s.translated, s.translateErr
}

func newTestHandler(t *testing.T, translators map[string]Translator, rawCh chan RawEvent) *httpHandler {
	t.Helper()
	logf := func(format string, args ...any) {}
	return newHTTPHandler(translators, rawCh, logf)
}

func TestHandler_RejectsNonPOST(t *testing.T) {
	h := newTestHandler(t, nil, make(chan RawEvent, 1))
	req := httptest.NewRequest(http.MethodGet, "/webhook/jira", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestHandler_RejectsInvalidPath(t *testing.T) {
	h := newTestHandler(t, nil, make(chan RawEvent, 1))
	for _, path := range []string{"/", "/webhook/", "/webhook/a/b", "/other"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusOK {
			t.Fatalf("path %q: expected non-200, got 200", path)
		}
	}
}

func TestHandler_RejectsUnknownProvider(t *testing.T) {
	h := newTestHandler(t, map[string]Translator{}, make(chan RawEvent, 1))
	req := httptest.NewRequest(http.MethodPost, "/webhook/unknown", bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandler_RejectsHMACFailure(t *testing.T) {
	st := &stubTranslator{provider: "jira", authErr: errors.New("bad sig")}
	h := newTestHandler(t, map[string]Translator{"jira": st}, make(chan RawEvent, 1))
	req := httptest.NewRequest(http.MethodPost, "/webhook/jira", bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandler_AcceptsValidRequest(t *testing.T) {
	st := &stubTranslator{provider: "jira"}
	rawCh := make(chan RawEvent, 1)
	h := newTestHandler(t, map[string]Translator{"jira": st}, rawCh)
	body := []byte(`{"issue": "test"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/jira", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature", "sha256=abc123")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	select {
	case ev := <-rawCh:
		if ev.Provider != "jira" {
			t.Errorf("expected provider jira, got %s", ev.Provider)
		}
		if !bytes.Equal(ev.Body, body) {
			t.Errorf("body mismatch")
		}
		if ev.Headers["x-hub-signature"] != "sha256=abc123" {
			t.Errorf("headers not lowercased or missing")
		}
	default:
		t.Fatal("no event enqueued")
	}
}

func TestHandler_BackpressureWhenChannelFull(t *testing.T) {
	st := &stubTranslator{provider: "jira"}
	rawCh := make(chan RawEvent, 0) // zero-capacity: always full
	h := newTestHandler(t, map[string]Translator{"jira": st}, rawCh)
	req := httptest.NewRequest(http.MethodPost, "/webhook/jira", bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestExtractProvider(t *testing.T) {
	cases := []struct {
		path    string
		want    string
	}{
		{"/webhook/jira", "jira"},
		{"/webhook/github", "github"},
		{"/webhook/", ""},
		{"/", ""},
		{"/webhook/a/b", ""},
		{"webhook/jira", "jira"}, // no leading slash: still parsed (Trim handles both ends)
	}
	for _, tc := range cases {
		got := extractProvider(tc.path)
		if got != tc.want {
			t.Errorf("extractProvider(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestCollectHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Hub-Signature", "sha256=abc")
	req.Header.Set("Content-Type", "application/json")
	headers := collectHeaders(req)
	if headers["x-hub-signature"] != "sha256=abc" {
		t.Errorf("expected x-hub-signature to be lowercased")
	}
	if headers["content-type"] != "application/json" {
		t.Errorf("expected content-type to be present")
	}
	for k := range headers {
		if k != strings.ToLower(k) {
			t.Errorf("header key %q is not lowercased", k)
		}
	}
}

func TestRemoteIP(t *testing.T) {
	cases := []struct{ addr, want string }{
		{"1.2.3.4:5678", "1.2.3.4"},
		{"[::1]:8080", "[::1]"},
		{"noport", "noport"},
	}
	for _, tc := range cases {
		got := remoteIP(tc.addr)
		if got != tc.want {
			t.Errorf("remoteIP(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

func TestRawEvent_Fields(t *testing.T) {
	now := time.Now().UTC()
	ev := RawEvent{
		Provider:   "jira",
		Headers:    map[string]string{"x-sig": "abc"},
		Body:       []byte("body"),
		SourceIP:   "1.2.3.4",
		ReceivedAt: now,
	}
	if ev.Provider != "jira" || ev.SourceIP != "1.2.3.4" || !ev.ReceivedAt.Equal(now) {
		t.Error("RawEvent fields not set correctly")
	}
}
