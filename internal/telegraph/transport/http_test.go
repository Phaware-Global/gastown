package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
)

// fakeTranslator is a test double for telegraph.Translator.
type fakeTranslator struct {
	provider     string
	authErr      error
	translated   *telegraph.NormalizedEvent
	translateErr error
}

func (f *fakeTranslator) Provider() string { return f.provider }
func (f *fakeTranslator) Authenticate(_ map[string]string, _ []byte) error {
	return f.authErr
}
func (f *fakeTranslator) Translate(_ []byte) (*telegraph.NormalizedEvent, error) {
	return f.translated, f.translateErr
}

// capturingTranslator records what was passed to Authenticate.
type capturingTranslator struct {
	provider string
	onAuth   func(map[string]string, []byte) error
}

func (c *capturingTranslator) Provider() string { return c.provider }
func (c *capturingTranslator) Authenticate(headers map[string]string, body []byte) error {
	if c.onAuth != nil {
		return c.onAuth(headers, body)
	}
	return nil
}
func (c *capturingTranslator) Translate(_ []byte) (*telegraph.NormalizedEvent, error) {
	return nil, telegraph.ErrUnknownEventType
}

func silentLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func newTestServer(t *testing.T, translators []telegraph.Translator, bufferSize int) *Server {
	t.Helper()
	return New(":0", translators, bufferSize, silentLogger())
}

func serveRequest(s *Server, method, path string, body []byte) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, bodyReader)
	rr := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(rr, req)
	return rr
}

func TestHandleWebhook_ValidRequest(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	s := newTestServer(t, []telegraph.Translator{tr}, 10)

	rr := serveRequest(s, http.MethodPost, "/webhook/jira", []byte(`{"test":1}`))

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}

	select {
	case evt := <-s.rawCh:
		if evt.Provider != "jira" {
			t.Errorf("want provider jira, got %q", evt.Provider)
		}
		if !bytes.Equal(evt.Body, []byte(`{"test":1}`)) {
			t.Errorf("body mismatch: %s", evt.Body)
		}
		if evt.ReceivedAt.IsZero() {
			t.Error("ReceivedAt should not be zero")
		}
	case <-time.After(time.Second):
		t.Fatal("event not enqueued within timeout")
	}
}

func TestHandleWebhook_AuthFailure(t *testing.T) {
	tr := &fakeTranslator{provider: "jira", authErr: errors.New("bad sig")}
	s := newTestServer(t, []telegraph.Translator{tr}, 10)

	rr := serveRequest(s, http.MethodPost, "/webhook/jira", []byte(`{}`))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
	if len(s.rawCh) != 0 {
		t.Error("channel should be empty after auth failure")
	}
}

func TestHandleWebhook_UnknownProvider(t *testing.T) {
	s := newTestServer(t, nil, 10)

	rr := serveRequest(s, http.MethodPost, "/webhook/github", []byte(`{}`))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestHandleWebhook_Backpressure(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	s := newTestServer(t, []telegraph.Translator{tr}, 1)

	rr1 := serveRequest(s, http.MethodPost, "/webhook/jira", []byte(`{}`))
	if rr1.Code != http.StatusOK {
		t.Fatalf("first request: want 200, got %d", rr1.Code)
	}

	// Channel full — second request should backpressure.
	rr2 := serveRequest(s, http.MethodPost, "/webhook/jira", []byte(`{}`))
	if rr2.Code != http.StatusServiceUnavailable {
		t.Fatalf("second request: want 503, got %d", rr2.Code)
	}
}

func TestHandleWebhook_MethodNotAllowed(t *testing.T) {
	tr := &fakeTranslator{provider: "jira"}
	s := newTestServer(t, []telegraph.Translator{tr}, 10)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rr := serveRequest(s, method, "/webhook/jira", nil)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: want 405, got %d", method, rr.Code)
		}
	}
}

func TestHandleWebhook_InvalidPath(t *testing.T) {
	s := newTestServer(t, nil, 10)

	for _, path := range []string{"/webhook/", "/webhook/jira/sub"} {
		rr := serveRequest(s, http.MethodPost, path, nil)
		if rr.Code != http.StatusNotFound {
			t.Errorf("path %q: want 404, got %d", path, rr.Code)
		}
	}
}

func TestHandleWebhook_HeadersLowercased(t *testing.T) {
	var capturedHeaders map[string]string
	capTr := &capturingTranslator{
		provider: "jira",
		onAuth: func(headers map[string]string, _ []byte) error {
			capturedHeaders = headers
			return nil
		},
	}
	s := newTestServer(t, []telegraph.Translator{capTr}, 10)

	req := httptest.NewRequest(http.MethodPost, "/webhook/jira", strings.NewReader(`{}`))
	req.Header.Set("X-Hub-Signature", "sha256=abc")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if capturedHeaders["x-hub-signature"] != "sha256=abc" {
		t.Errorf("want lowercased x-hub-signature, got %v", capturedHeaders)
	}
}

func TestHandleWebhook_SourceIPStripsPort(t *testing.T) {
	capTr := &capturingTranslator{provider: "jira"}
	s := newTestServer(t, []telegraph.Translator{capTr}, 10)

	req := httptest.NewRequest(http.MethodPost, "/webhook/jira", strings.NewReader(`{}`))
	req.RemoteAddr = "1.2.3.4:12345"
	rr := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}

	evt := <-s.rawCh
	if evt.SourceIP != "1.2.3.4" {
		t.Errorf("want IP without port, got %q", evt.SourceIP)
	}
}

func TestNew_MultipleProviders(t *testing.T) {
	jira := &fakeTranslator{provider: "jira"}
	gh := &fakeTranslator{provider: "github"}
	s := newTestServer(t, []telegraph.Translator{jira, gh}, 10)

	for _, provider := range []string{"jira", "github"} {
		rr := serveRequest(s, http.MethodPost, fmt.Sprintf("/webhook/%s", provider), []byte(`{}`))
		if rr.Code != http.StatusOK {
			t.Errorf("provider %s: want 200, got %d", provider, rr.Code)
		}
		<-s.rawCh // drain
	}
}

func TestRun_GracefulShutdown(t *testing.T) {
	// Find a free port via OS, then release it and use that addr.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	s := New(addr, nil, 10, silentLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.Run(ctx)
	}()

	// Allow server to bind.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}

	// Channel should be closed after Run returns.
	_, open := <-s.rawCh
	if open {
		t.Error("rawCh should be closed after Run returns")
	}
}
