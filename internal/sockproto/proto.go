// Package sockproto defines the wire protocol between the orchestrator's
// socket execution backend and gt-worker-client
// (docs/design/remote-polecat-execution-socket.md §4): newline-delimited JSON
// messages on the control connection, one object per line, UTF-8. Requests
// carry an ID nonce; responses echo it.
//
// The §4.3 exec-stream framing (binary frames after an attach preamble)
// arrives with the exec-streaming increment and is not defined here yet.
package sockproto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ProtoVersion is the control-protocol version spoken by this build. A worker
// that cannot speak the version in hello refuses with an error message (§3.2).
const ProtoVersion = 1

// Message types (§4.1, §4.2).
const (
	TypeAuth     = "auth" // unix-socket pre-shared token mode only (§3.3)
	TypeHello    = "hello"
	TypeHelloAck = "hello_ack"
	TypeDiscover = "discover"
	TypeSessions = "sessions"
	TypePing     = "ping"
	TypePong     = "pong"
	TypeError    = "error"

	TypeSessionOpen      = "session_open"
	TypeCSR              = "csr"
	TypeCert             = "cert"
	TypeSessionReady     = "session_ready"
	TypeSessionError     = "session_error"
	TypeShutdown         = "shutdown"
	TypeShutdownComplete = "shutdown_complete"
	TypeTeardown         = "teardown"
	TypeTeardownComplete = "teardown_complete"
)

// Capabilities reports what a worker can run (§4.1 hello_ack).
type Capabilities struct {
	Docker      bool     `json:"docker"`
	ExecModes   []string `json:"exec_modes"`
	MaxSessions int      `json:"max_sessions"`
}

// SessionSummary describes one live session (§4.1 sessions / hello_ack).
type SessionSummary struct {
	Session   string    `json:"session"`
	Rig       string    `json:"rig"`
	Polecat   string    `json:"polecat"`
	State     string    `json:"state"` // "ready" | "orphaned"
	StartedAt time.Time `json:"started_at"`
}

// Message is the single control-connection message shape. Fields are
// populated per Type; unused fields stay zero and are omitted on the wire.
type Message struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`      // request nonce, echoed in the response
	Session string `json:"session,omitempty"` // session-scoped messages

	// auth (§3.3)
	Token string `json:"token,omitempty"`

	// hello / hello_ack (§3.2, §4.1)
	ProtoVersion   int              `json:"proto_version,omitempty"`
	GTVersion      string           `json:"gt_version,omitempty"`
	OrchestratorID string           `json:"orchestrator_id,omitempty"`
	WorkerID       string           `json:"worker_id,omitempty"`
	OS             string           `json:"os,omitempty"`
	Arch           string           `json:"arch,omitempty"`
	Capabilities   *Capabilities    `json:"capabilities,omitempty"`
	Sessions       []SessionSummary `json:"sessions,omitempty"`

	// discover filters (§4.1)
	Rig     string `json:"rig,omitempty"`
	Polecat string `json:"polecat,omitempty"`

	// session_open (§4.2)
	ExecMode           string            `json:"exec_mode,omitempty"`
	Image              string            `json:"image,omitempty"`
	NetworkMode        string            `json:"network_mode,omitempty"`
	ProxyURL           string            `json:"proxy_url,omitempty"`
	CheckpointInterval string            `json:"checkpoint_interval,omitempty"`
	MaxRuntime         string            `json:"max_runtime,omitempty"`
	Env                map[string]string `json:"env,omitempty"` // NON-secret only (core §7.4)

	// csr / cert (§4.2; core §7.2 over the socket)
	CSRPEM   string    `json:"csr_pem,omitempty"`
	CertPEM  string    `json:"cert_pem,omitempty"`
	CAPEM    string    `json:"ca_pem,omitempty"`
	NotAfter time.Time `json:"not_after,omitzero"`

	// session_ready (§4.2)
	RelayAddr string `json:"relay_addr,omitempty"`

	// shutdown / teardown (§4.2)
	Reason        string `json:"reason,omitempty"`
	GraceSeconds  int    `json:"grace_seconds,omitempty"`
	CleanWorktree *bool  `json:"clean_worktree,omitempty"` // nil = true (default)
	CheckpointRef string `json:"checkpoint_ref,omitempty"` // shutdown_complete

	// error / session_error
	Code string `json:"code,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// maxLine bounds a single control message. CSR/cert PEMs are a few KiB;
// session env maps stay small. 1 MiB is far above any legitimate message and
// keeps a misbehaving peer from ballooning memory.
const maxLine = 1 << 20

// Codec reads and writes newline-delimited JSON messages on a connection.
// Not safe for concurrent use on the same side (the control protocol is
// sequential request/response by design).
type Codec struct {
	r *bufio.Reader
	w io.Writer
}

// NewCodec wraps a stream in a message codec.
func NewCodec(rw io.ReadWriter) *Codec {
	return &Codec{r: bufio.NewReaderSize(rw, 64<<10), w: rw}
}

// Send writes one message as a single JSON line.
func (c *Codec) Send(m *Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("sockproto: marshal %s: %w", m.Type, err)
	}
	data = append(data, '\n')
	if _, err := c.w.Write(data); err != nil {
		return fmt.Errorf("sockproto: write %s: %w", m.Type, err)
	}
	return nil
}

// Recv reads the next message line.
func (c *Codec) Recv() (*Message, error) {
	line, err := c.r.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		// Drain oversized line up to maxLine, then fail decisively.
		total := len(line)
		for err == bufio.ErrBufferFull && total < maxLine {
			line, err = c.r.ReadSlice('\n')
			total += len(line)
		}
		return nil, fmt.Errorf("sockproto: message exceeds %d bytes", maxLine)
	}
	if err != nil {
		if err == io.EOF && len(line) == 0 {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("sockproto: read: %w", err)
	}
	var m Message
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, fmt.Errorf("sockproto: decode: %w", err)
	}
	if m.Type == "" {
		return nil, fmt.Errorf("sockproto: message missing type")
	}
	return &m, nil
}

// SendErr writes a typed error response echoing id.
func (c *Codec) SendErr(id, code, msg string) error {
	return c.Send(&Message{Type: TypeError, ID: id, Code: code, Msg: msg})
}
