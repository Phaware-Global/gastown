package socket

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/steveyegge/gastown/internal/sockproto"
)

// ExecStreamOptions configures an exec-stream dial (§4.3). It mirrors what a
// launcher can know from its argv/env: the worker address, plus whichever
// credential the transport needs — a pre-shared token for a unix socket, or
// the enrolled worker name to pin for TCP mTLS (material comes from the
// enrollment-managed CA dir).
type ExecStreamOptions struct {
	Address    string
	Token      string // unix-socket mode (§3.3)
	WorkerName string // TCP: enrolled name to pin
}

// DialExecStream opens a connection to a worker and completes the §3 handshake,
// leaving it ready for an attach preamble. It returns the raw conn (for frame
// WRITES) and the codec (whose buffered reader must be used for the ack and all
// frame READS — bytes past the preamble may already be buffered).
//
// This is exported for gt-worker-attach, which is a separate binary from the
// daemon and so cannot reach the package-internal dialer.
func DialExecStream(ctx context.Context, opts ExecStreamOptions) (net.Conn, *sockproto.Codec, error) {
	if opts.Address == "" {
		return nil, nil, fmt.Errorf("socket: exec stream requires an address")
	}
	s := &Settings{Address: opts.Address, Token: opts.Token}
	if strings.HasPrefix(opts.Address, "unix://") {
		// A unix worker authenticates with the pre-shared token (§3.3).
		s.TLS = TLSConfig{Mode: tlsModeNone}
		if opts.Token == "" {
			return nil, nil, fmt.Errorf("socket: a unix worker requires a token (set GT_WORKER_TOKEN)")
		}
	} else {
		// TCP: mutual TLS with the enrollment-managed material, pinned to the
		// enrolled worker name.
		if opts.WorkerName == "" {
			return nil, nil, fmt.Errorf("socket: a TCP worker requires the enrolled worker name to pin (set GT_WORKER_NAME)")
		}
		s.TLS = TLSConfig{Mode: tlsModeAuto, WorkerName: opts.WorkerName}
	}
	if err := s.validate(); err != nil {
		return nil, nil, err
	}

	c, err := dial(ctx, s, "gt-worker-attach", "")
	if err != nil {
		return nil, nil, err
	}
	return c.nc, c.codec, nil
}
