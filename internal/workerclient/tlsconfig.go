package workerclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// OrchestratorCN is the CN a worker requires on the orchestrator's client
// cert. Duplicated from workerca (rather than imported) to keep the
// orchestrator-side package out of the worker binary; the enrollment tests
// assert the two agree.
const OrchestratorCN = "gt-orchestrator"

// ServerTLSConfig builds the worker's control-listener TLS config from
// enrolled material: present the machine cert, require a client cert chaining
// to the client CA, AND pin that client cert's CN to the orchestrator
// (docs/design/remote-polecat-execution-socket.md §3).
//
// The CN pin is load-bearing, not cosmetic. Without it, ANY certificate the
// client CA has signed would be accepted as the orchestrator — including
// another worker's machine cert, which would turn one compromised worker into
// fleet-wide lateral access. (Machine certs are also minted ServerAuth-only so
// they cannot satisfy a client-auth requirement at all; this is the second,
// independent barrier.)
func ServerTLSConfig(machineCertFile, machineKeyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(machineCertFile, machineKeyFile)
	if err != nil {
		return nil, fmt.Errorf("workerclient: load machine cert (run `gt-worker-client enroll`?): %w", err)
	}
	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("workerclient: read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("workerclient: client CA file %s has no valid certificates", clientCAFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("no client certificate presented")
			}
			if cn := cs.PeerCertificates[0].Subject.CommonName; cn != OrchestratorCN {
				return fmt.Errorf("client certificate CN %q is not the orchestrator (%q): only the town daemon may drive a worker", cn, OrchestratorCN)
			}
			return nil
		},
	}, nil
}
