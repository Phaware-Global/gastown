package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AdminSigner signs CSRs by POSTing to the proxy's /v1/admin/sign-csr
// endpoint. The admin API binds to the orchestrator's loopback, so this
// signer is usable where the worker agent (or the provider channel's
// host-side end) runs on the orchestrator host: LocalBackend-style setups,
// tests, and provider backends that relay the CSR to the host themselves and
// then call the admin API on the worker's behalf. It is NOT a remote
// worker's direct path to the CA — that always rides the provider channel.
type AdminSigner struct {
	// AdminURL is the proxy admin base URL, e.g. http://127.0.0.1:9877.
	AdminURL string
	// Rig and Name identify the polecat; the server binds the cert CN to
	// gt-<rig>-<name> regardless of what the CSR requests.
	Rig  string
	Name string
	// TTL is the requested cert lifetime; empty uses the server default
	// (DefaultRemoteCertTTL). The server clamps to MaxRemoteCertTTL.
	TTL string
	// Client overrides the HTTP client; nil uses a 30s-timeout default.
	Client *http.Client
}

type signCSRRequest struct {
	Rig  string `json:"rig"`
	Name string `json:"name"`
	CSR  string `json:"csr"`
	TTL  string `json:"ttl,omitempty"`
}

type signCSRResponse struct {
	CN   string `json:"cn"`
	Cert string `json:"cert"`
	CA   string `json:"ca"`
}

// SignCSR implements Signer via the proxy admin API.
func (s *AdminSigner) SignCSR(ctx context.Context, csrPEM []byte) (certPEM, caPEM []byte, err error) {
	body, err := json.Marshal(signCSRRequest{Rig: s.Rig, Name: s.Name, CSR: string(csrPEM), TTL: s.TTL})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal sign-csr request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.AdminURL+"/v1/admin/sign-csr", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build sign-csr request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("sign-csr request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, nil, fmt.Errorf("sign-csr: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var result signCSRResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil, fmt.Errorf("decode sign-csr response: %w", err)
	}
	if result.Cert == "" || result.CA == "" {
		return nil, nil, fmt.Errorf("sign-csr response missing cert or ca")
	}
	return []byte(result.Cert), []byte(result.CA), nil
}
