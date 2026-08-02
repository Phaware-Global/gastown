package sockproto

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pipe is a bidirectional in-memory ReadWriter over two buffers.
type rw struct {
	io.Reader
	io.Writer
}

func TestCodec_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	c := NewCodec(rw{&buf, &buf})
	require.NoError(t, c.Send(&Message{Type: TypeHello, ID: "r1", ProtoVersion: ProtoVersion}))
	m, err := c.Recv()
	require.NoError(t, err)
	assert.Equal(t, TypeHello, m.Type)
	assert.Equal(t, "r1", m.ID)
	assert.Equal(t, ProtoVersion, m.ProtoVersion)
}

func TestCodec_DecodesLargeMessageUnderMaxLine(t *testing.T) {
	// A hello_ack / sessions response grows with session count; a message
	// well over the 64KiB bufio buffer but under maxLine must DECODE, not be
	// rejected (the round-1 codec defect).
	var sessions []SessionSummary
	for i := 0; i < 2000; i++ { // ~2000 * ~120 bytes ≈ 240KiB, > 64KiB, < 1MiB
		sessions = append(sessions, SessionSummary{
			Session: "gt-rig-polecat-" + strings.Repeat("x", 20),
			Rig:     "rig", Polecat: "polecat", State: "ready",
		})
	}
	msg := &Message{Type: TypeSessions, ID: "r1", Sessions: sessions}

	var buf bytes.Buffer
	send := NewCodec(rw{nil, &buf})
	require.NoError(t, send.Send(msg))
	require.Greater(t, buf.Len(), 64<<10, "test message must exceed the bufio buffer")
	require.Less(t, buf.Len(), maxLine, "and stay under maxLine")

	recv := NewCodec(rw{&buf, io.Discard})
	got, err := recv.Recv()
	require.NoError(t, err)
	assert.Equal(t, TypeSessions, got.Type)
	assert.Len(t, got.Sessions, 2000)
}

func TestCodec_RejectsOverMaxLine(t *testing.T) {
	// A single line beyond maxLine is rejected decisively.
	huge := `{"type":"x","msg":"` + strings.Repeat("A", maxLine) + `"}` + "\n"
	recv := NewCodec(rw{strings.NewReader(huge), io.Discard})
	_, err := recv.Recv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

func TestCodec_EOF(t *testing.T) {
	recv := NewCodec(rw{strings.NewReader(""), io.Discard})
	_, err := recv.Recv()
	assert.ErrorIs(t, err, io.EOF)
}

func TestCodec_MissingType(t *testing.T) {
	recv := NewCodec(rw{strings.NewReader(`{"id":"r1"}` + "\n"), io.Discard})
	_, err := recv.Recv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing type")
}
