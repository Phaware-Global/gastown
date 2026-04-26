package cmd

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/telegraph"
	"github.com/steveyegge/gastown/internal/telegraph/tlog"
)

// writeTelegraphTOML writes a telegraph.toml into <townRoot>/settings/ and returns the path.
func writeTelegraphTOML(t *testing.T, townRoot, content string) string {
	t.Helper()
	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir settings: %v", err)
	}
	path := filepath.Join(dir, "telegraph.toml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write telegraph.toml: %v", err)
	}
	return path
}

func TestTelegraphStart_ConfigMissing(t *testing.T) {
	townRoot := t.TempDir()
	cfgPath := telegraph.DefaultPath(townRoot) // settings/telegraph.toml does not exist

	_, _, err := telegraphSetup(cfgPath)
	if err == nil {
		t.Fatal("expected error for missing config, got nil")
	}
	if !strings.Contains(err.Error(), "telegraph config not found") {
		t.Errorf("want 'telegraph config not found' in error; got: %v", err)
	}
}

func TestTelegraphStart_ValidateFailure(t *testing.T) {
	townRoot := t.TempDir()
	// enabled=true with no secret_env violates Validate().
	writeTelegraphTOML(t, townRoot, `
[telegraph]
listen_addr = ":8765"
buffer_size = 256
nudge_window = "30s"
body_cap = 4096

[telegraph.providers.jira]
enabled = true
events = ["jira:issue_created"]
`)

	cfgPath := telegraph.DefaultPath(townRoot)
	_, _, err := telegraphSetup(cfgPath)
	if err == nil {
		t.Fatal("expected validate error for missing secret_env, got nil")
	}
}

func TestTelegraphStart_SecretMissing(t *testing.T) {
	const envVar = "GT_TELEGRAPH_JIRA_TEST_SECRET_MISSING"
	townRoot := t.TempDir()
	writeTelegraphTOML(t, townRoot, fmt.Sprintf(`
[telegraph]
listen_addr = ":8765"
buffer_size = 256
nudge_window = "30s"
body_cap = 4096

[telegraph.providers.jira]
enabled = true
secret_env = %q
events = ["jira:issue_created"]
`, envVar))

	t.Setenv(envVar, "") // explicitly empty → ResolveSecret returns error

	cfgPath := telegraph.DefaultPath(townRoot)
	_, _, err := telegraphSetup(cfgPath)
	if err == nil {
		t.Fatal("expected error for unset env var, got nil")
	}
	if !strings.Contains(err.Error(), "jira") {
		t.Errorf("error missing provider ID 'jira': %v", err)
	}
	if !strings.Contains(err.Error(), envVar) {
		t.Errorf("error missing env var name %q: %v", envVar, err)
	}
}

func TestTelegraphStart_HappyPath(t *testing.T) {
	const testSecret = "happy-path-secret-xyzzy"
	const envVar = "GT_TELEGRAPH_JIRA_TEST_HAPPY_SECRET"

	townRoot := t.TempDir()
	t.Setenv(envVar, testSecret)

	writeTelegraphTOML(t, townRoot, fmt.Sprintf(`
[telegraph]
listen_addr = ":0"
buffer_size = 64
nudge_window = "0s"
body_cap = 4096

[telegraph.providers.jira]
enabled = true
secret_env = %q
events = ["jira:issue_created"]
`, envVar))

	cfgPath := telegraph.DefaultPath(townRoot)
	cfg, resolved, err := telegraphSetup(cfgPath)
	if err != nil {
		t.Fatalf("telegraphSetup: %v", err)
	}

	// Pre-create a listener on :0 so the test knows the address.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()

	mr := mail.NewMemoryRouter()
	logger := tlog.New(io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deps := telegraphDeps{
		sender:   mr,
		listenFn: func(string) (net.Listener, error) { return ln, nil },
		log:      logger,
	}

	implErr := make(chan error, 1)
	go func() {
		implErr <- runTelegraphStartImpl(ctx, cfg, townRoot, resolved, deps)
	}()

	// Build and sign a Jira issue_created payload.
	payload := telegraphTestIssueCreated("PROJ-1", "alice", "Fix login", "Users get kicked out after 5 min")
	sig := telegraphSign([]byte(testSecret), payload)

	// Retry POST until the server is ready.
	var resp *http.Response
	for i := 0; i < 200; i++ {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/webhook/jira", bytes.NewReader(payload))
		req.Header.Set("X-Hub-Signature", sig)
		req.Header.Set("Content-Type", "application/json")
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("POST /webhook/jira failed after retries: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST: want 200, got %d", resp.StatusCode)
	}

	// Wait for mail to land in the MemoryRouter.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(mr.Messages()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(mr.Messages()); n != 1 {
		t.Fatalf("want 1 message in MemoryRouter, got %d", n)
	}

	if v := logger.Counters.Accept.Load(); v != 1 {
		t.Errorf("Counters.Accept = %d, want 1", v)
	}
	if v := logger.Counters.Deliver.Load(); v != 1 {
		t.Errorf("Counters.Deliver = %d, want 1", v)
	}

	// Trigger graceful shutdown.
	cancel()
	select {
	case err := <-implErr:
		if err != nil {
			t.Errorf("runTelegraphStartImpl returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("timed out waiting for runTelegraphStartImpl to exit")
	}
}

// telegraphTestIssueCreated builds a minimal Jira issue_created JSON payload.
func telegraphTestIssueCreated(key, actor, summary, description string) []byte {
	p := map[string]any{
		"timestamp":    time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC).UnixMilli(),
		"webhookEvent": "jira:issue_created",
		"user":         map[string]string{"name": actor},
		"issue": map[string]any{
			"key":  key,
			"self": "https://example.atlassian.net/browse/" + key,
			"fields": map[string]any{
				"summary":     summary,
				"description": description,
				"labels":      []string{},
			},
		},
	}
	b, _ := json.Marshal(p)
	return b
}

// telegraphSign returns the HMAC-SHA256 X-Hub-Signature header value for a payload.
func telegraphSign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
