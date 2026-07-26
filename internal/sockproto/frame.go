package sockproto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Exec-stream frame types (docs/design/remote-polecat-execution-socket.md
// §4.3). After the attach preamble the connection carries only these frames:
//
//	1 byte  frame type
//	4 bytes payload length (big-endian uint32)
//	N bytes payload
type FrameType byte

const (
	FrameStdin  FrameType = 0 // launcher → worker
	FrameStdout FrameType = 1 // worker → launcher
	FrameStderr FrameType = 2 // worker → launcher
	FrameResize FrameType = 3 // launcher → worker: {"cols":N,"rows":N}
	FrameExit   FrameType = 4 // worker → launcher: 1-byte exit code; ends the stream
	FrameSignal FrameType = 5 // launcher → worker: signal name, e.g. "SIGINT"
)

// String renders a frame type for diagnostics.
func (t FrameType) String() string {
	switch t {
	case FrameStdin:
		return "stdin"
	case FrameStdout:
		return "stdout"
	case FrameStderr:
		return "stderr"
	case FrameResize:
		return "resize"
	case FrameExit:
		return "exit"
	case FrameSignal:
		return "signal"
	default:
		return fmt.Sprintf("frame(%d)", byte(t))
	}
}

// MaxFramePayload bounds a single frame's payload so a misbehaving peer cannot
// make the reader allocate without limit. Agent output is chunked well below
// this; a larger claimed length is a protocol error, not a big write.
const MaxFramePayload = 1 << 20 // 1 MiB

// frameHeaderLen is the fixed header size (type + length).
const frameHeaderLen = 5

// Resize is the FrameResize payload.
type Resize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// WriteFrame writes one frame. Concurrent writers must serialize themselves —
// a frame's header and payload must not interleave with another's.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if len(payload) > MaxFramePayload {
		return fmt.Errorf("sockproto: frame payload %d exceeds %d", len(payload), MaxFramePayload)
	}
	var hdr [frameHeaderLen]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("sockproto: write frame header: %w", err)
	}
	if len(payload) == 0 {
		return nil
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("sockproto: write frame payload: %w", err)
	}
	return nil
}

// ReadFrame reads one frame. It returns io.EOF only when the stream ends
// cleanly at a frame boundary; a truncated frame is an error, so a caller can
// never mistake a cut connection for an orderly end of stream.
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if err == io.EOF {
			return 0, nil, io.EOF
		}
		if err == io.ErrUnexpectedEOF {
			return 0, nil, fmt.Errorf("sockproto: truncated frame header: %w", err)
		}
		return 0, nil, fmt.Errorf("sockproto: read frame header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFramePayload {
		return 0, nil, fmt.Errorf("sockproto: frame payload %d exceeds %d", n, MaxFramePayload)
	}
	if n == 0 {
		return FrameType(hdr[0]), nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, fmt.Errorf("sockproto: read frame payload (%d bytes): %w", n, err)
	}
	return FrameType(hdr[0]), buf, nil
}

// WriteExitFrame writes the terminal exit frame. The code is clamped into a
// single byte (the wire format's width); a negative code — a process killed by
// a signal, which has no exit status — is reported as 255.
func WriteExitFrame(w io.Writer, code int) error {
	b := byte(255)
	if code >= 0 && code <= 254 {
		b = byte(code)
	}
	return WriteFrame(w, FrameExit, []byte{b})
}

// ExitCodeFromFrame decodes an exit frame's payload.
func ExitCodeFromFrame(payload []byte) (int, error) {
	if len(payload) != 1 {
		return 0, fmt.Errorf("sockproto: exit frame payload must be 1 byte, got %d", len(payload))
	}
	return int(payload[0]), nil
}

// MarshalResize encodes a resize payload.
func MarshalResize(cols, rows int) ([]byte, error) {
	return json.Marshal(Resize{Cols: cols, Rows: rows})
}

// UnmarshalResize decodes a resize payload.
func UnmarshalResize(payload []byte) (Resize, error) {
	var r Resize
	if err := json.Unmarshal(payload, &r); err != nil {
		return r, fmt.Errorf("sockproto: decode resize: %w", err)
	}
	return r, nil
}
