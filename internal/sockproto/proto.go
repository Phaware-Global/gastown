// Package sockproto defines the wire protocol between the orchestrator's
// socket execution backend and gt-worker-client
// (docs/design/remote-polecat-execution-socket.md §4): newline-delimited JSON
// messages on the control connection, one object per line, UTF-8. Requests
// carry an ID nonce; responses echo it.
//
// The §4.3 exec-stream framing (binary frames after an attach preamble) lives
// in frame.go: a connection speaks JSON messages until attach/attach_ack, then
// switches to frames on the SAME connection.
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

	// Enrollment (§3.1), spoken only on a listener in enrollment mode.
	TypeEnroll         = "enroll"          // orch → worker: join token
	TypeEnrollCSR      = "enroll_csr"      // worker → orch: machine CSR
	TypeEnrollComplete = "enroll_complete" // orch → worker: machine cert + CAs
	TypeEnrollAck      = "enroll_ack"      // worker → orch: material persisted

	// Exec stream (§4.3): attach opens one, then the connection switches to
	// binary frames.
	TypeAttach    = "attach"
	TypeAttachAck = "attach_ack"

	// binary freshness (§4.1): the orchestrator streams the companion binaries
	// a worker runs — gt-proxy-client (which IS `gt` and `bd` there) and
	// gt-worker-client itself — when the worker reports a different gt_version.
	TypePushBinary    = "push_binaries"
	TypePushBinaryAck = "push_binaries_ack"

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
	// ContainerClient is the sha256 of the gt-proxy-client the worker can inject
	// into a work container, or "" if it has none.
	//
	// Version equality is NOT enough to decide a container push: a worker can be
	// perfectly up to date and still have never received the container
	// platform's binaries (fresh enrollment, wiped state dir), and a container
	// without them runs an agent with no gt/bd at all. Reporting the DIGEST
	// rather than a bool also stops an identical binary being re-streamed on
	// every provision.
	ContainerClient string `json:"container_client,omitempty"`
	// ContainerPlatform is "<goos>-<goarch>" of the worker's docker daemon,
	// when it has one. The work container is a Linux container even on a macOS
	// worker, so the binaries injected into it are a DIFFERENT platform from
	// the ones the worker runs — the orchestrator needs this to pick the right
	// artifacts (§4.1).
	ContainerPlatform string `json:"container_platform,omitempty"`
}

// GetContainerPlatform reads the container platform from a possibly-nil
// capability block.
func (c *Capabilities) GetContainerPlatform() string {
	if c == nil {
		return ""
	}
	return c.ContainerPlatform
}

// GetContainerClient reads the injectable client's digest from a possibly-nil
// capability block. "" means the worker has none.
func (c *Capabilities) GetContainerClient() string {
	if c == nil {
		return ""
	}
	return c.ContainerClient
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

	// push_binaries (§4.1). Chunks carry Data; the final chunk sets EOF and the
	// whole-file SHA256, which the worker verifies before anything is installed.
	Name string `json:"name,omitempty"`
	// Platform tags binaries destined for somewhere other than the worker
	// itself — "<goos>-<goarch>" for the work container's Linux binaries.
	// Empty means the worker's own platform.
	Platform string `json:"platform,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Data     string `json:"data,omitempty"` // base64 chunk
	EOF      bool   `json:"eof,omitempty"`
	Applied  string `json:"applied,omitempty"` // ack: "installed" | "staged"

	// csr / cert (§4.2; core §7.2 over the socket)
	CSRPEM   string    `json:"csr_pem,omitempty"`
	CertPEM  string    `json:"cert_pem,omitempty"`
	CAPEM    string    `json:"ca_pem,omitempty"`
	NotAfter time.Time `json:"not_after,omitzero"`

	// enrollment (§3.1). WorkerName is the enrolled machine identity the
	// orchestrator assigns; ClientCAPEM is the CA that signs the
	// orchestrator's client cert, which the worker pins for future
	// connections.
	JoinToken   string `json:"join_token,omitempty"`
	WorkerName  string `json:"worker_name,omitempty"`
	ClientCAPEM string `json:"client_ca_pem,omitempty"`

	// session_ready (§4.2)
	RelayAddr string `json:"relay_addr,omitempty"`

	// attach (§4.3, §5): the agent command to exec worker-side, plus the
	// NON-SECRET session env it needs (core §7.4). Secret env is delivered
	// worker-side via the operator's agent env file (§8) and never rides
	// this payload.
	Argv []string `json:"argv,omitempty"`
	TTY  bool     `json:"tty,omitempty"`

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

// PushChunkBytes is the RAW payload per push_binaries chunk. Base64 inflates by
// 4/3, so this leaves ample room under maxLine once the JSON envelope is added —
// a chunk that overran the line limit would fail mid-transfer, after the worker
// had already written most of a binary.
const PushChunkBytes = 512 << 10

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
//
// A message may exceed the bufio buffer (hello_ack / sessions grow with the
// worker's session count), so fragments returned with bufio.ErrBufferFull are
// accumulated until the newline, bounded by maxLine. Any Recv error
// invalidates the connection — a partially-read oversized line leaves the
// stream unaligned, so callers must not reuse the codec after an error.
func (c *Codec) Recv() (*Message, error) {
	line, err := c.r.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		// ReadSlice returns a slice into the reader's buffer, invalidated by
		// the next read — copy each fragment as we accumulate.
		buf := make([]byte, len(line), len(line)*2)
		copy(buf, line)
		for err == bufio.ErrBufferFull {
			if len(buf) > maxLine {
				return nil, fmt.Errorf("sockproto: message exceeds %d bytes", maxLine)
			}
			line, err = c.r.ReadSlice('\n')
			buf = append(buf, line...)
		}
		if err != nil {
			return nil, fmt.Errorf("sockproto: read: %w", err)
		}
		if len(buf) > maxLine {
			return nil, fmt.Errorf("sockproto: message exceeds %d bytes", maxLine)
		}
		return decodeMessage(buf)
	}
	if err != nil {
		if err == io.EOF && len(line) == 0 {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("sockproto: read: %w", err)
	}
	return decodeMessage(line)
}

// decodeMessage unmarshals one JSON line into a Message and checks the
// required type field.
func decodeMessage(line []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, fmt.Errorf("sockproto: decode: %w", err)
	}
	if m.Type == "" {
		return nil, fmt.Errorf("sockproto: message missing type")
	}
	return &m, nil
}

// Reader returns the codec's buffered reader. After the attach preamble a
// connection switches to §4.3 frames on the SAME stream, so the frame reader
// MUST continue from this buffer — reading the raw conn instead would drop any
// bytes the codec already buffered past the preamble line.
func (c *Codec) Reader() io.Reader { return c.r }

// SendErr writes a typed error response echoing id.
func (c *Codec) SendErr(id, code, msg string) error {
	return c.Send(&Message{Type: TypeError, ID: id, Code: code, Msg: msg})
}
