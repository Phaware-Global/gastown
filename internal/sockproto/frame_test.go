package sockproto

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrame_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteFrame(&buf, FrameStdout, []byte("hello")))
	require.NoError(t, WriteFrame(&buf, FrameStderr, []byte("oops")))
	require.NoError(t, WriteFrame(&buf, FrameStdin, nil)) // empty payload is legal
	require.NoError(t, WriteExitFrame(&buf, 42))

	want := []struct {
		t       FrameType
		payload string
	}{
		{FrameStdout, "hello"},
		{FrameStderr, "oops"},
		{FrameStdin, ""},
	}
	for i, w := range want {
		ft, payload, err := ReadFrame(&buf)
		require.NoError(t, err, "frame %d", i)
		assert.Equal(t, w.t, ft)
		assert.Equal(t, w.payload, string(payload))
	}
	ft, payload, err := ReadFrame(&buf)
	require.NoError(t, err)
	require.Equal(t, FrameExit, ft)
	code, err := ExitCodeFromFrame(payload)
	require.NoError(t, err)
	assert.Equal(t, 42, code)

	// Clean end of stream at a frame boundary.
	_, _, err = ReadFrame(&buf)
	assert.ErrorIs(t, err, io.EOF)
}

func TestFrame_ExitCodeClamping(t *testing.T) {
	// A signal death has no exit status (-1) and must not wrap to a bogus
	// success; it is reported as 255.
	for _, tc := range []struct{ in, want int }{{0, 0}, {1, 1}, {254, 254}, {255, 255}, {-1, 255}, {300, 255}} {
		var buf bytes.Buffer
		require.NoError(t, WriteExitFrame(&buf, tc.in))
		_, payload, err := ReadFrame(&buf)
		require.NoError(t, err)
		got, err := ExitCodeFromFrame(payload)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "exit %d", tc.in)
	}
}

func TestFrame_TruncatedIsNotEOF(t *testing.T) {
	// A cut connection must never look like an orderly end of stream.
	t.Run("truncated header", func(t *testing.T) {
		_, _, err := ReadFrame(strings.NewReader("\x01\x00"))
		require.Error(t, err)
		assert.NotErrorIs(t, err, io.EOF)
		assert.Contains(t, err.Error(), "truncated")
	})

	t.Run("truncated payload", func(t *testing.T) {
		var buf bytes.Buffer
		buf.WriteByte(byte(FrameStdout))
		_ = binary.Write(&buf, binary.BigEndian, uint32(10))
		buf.WriteString("only4")
		_, _, err := ReadFrame(&buf)
		require.Error(t, err)
		assert.NotErrorIs(t, err, io.EOF)
	})
}

func TestFrame_OversizedRejected(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		err := WriteFrame(io.Discard, FrameStdout, make([]byte, MaxFramePayload+1))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds")
	})

	t.Run("read does not allocate a huge claim", func(t *testing.T) {
		var buf bytes.Buffer
		buf.WriteByte(byte(FrameStdout))
		_ = binary.Write(&buf, binary.BigEndian, uint32(1<<31)) // 2 GiB claim
		_, _, err := ReadFrame(&buf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds")
	})
}

func TestFrame_Resize(t *testing.T) {
	payload, err := MarshalResize(120, 40)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, WriteFrame(&buf, FrameResize, payload))
	ft, got, err := ReadFrame(&buf)
	require.NoError(t, err)
	require.Equal(t, FrameResize, ft)
	r, err := UnmarshalResize(got)
	require.NoError(t, err)
	assert.Equal(t, 120, r.Cols)
	assert.Equal(t, 40, r.Rows)
}

func TestExitCodeFromFrame_BadPayload(t *testing.T) {
	_, err := ExitCodeFromFrame(nil)
	assert.Error(t, err)
	_, err = ExitCodeFromFrame([]byte{1, 2})
	assert.Error(t, err)
}

func TestFrameType_String(t *testing.T) {
	assert.Equal(t, "stdout", FrameStdout.String())
	assert.Equal(t, "exit", FrameExit.String())
	assert.Contains(t, FrameType(99).String(), "99")
}

// TestCodecReader_PreservesBufferedFramesAfterPreamble pins the handoff that
// makes attach work: a JSON preamble line and the first frames often arrive in
// ONE read, so the frame reader must continue from the codec's buffer.
func TestCodecReader_PreservesBufferedFramesAfterPreamble(t *testing.T) {
	var wire bytes.Buffer
	c := NewCodec(&wire)
	require.NoError(t, c.Send(&Message{Type: TypeAttach, ID: "a", Session: "s"}))
	require.NoError(t, WriteFrame(&wire, FrameStdout, []byte("post-preamble")))

	// One reader over the whole byte stream: read the message, then the frame.
	rc := NewCodec(struct {
		io.Reader
		io.Writer
	}{bytes.NewReader(wire.Bytes()), io.Discard})
	m, err := rc.Recv()
	require.NoError(t, err)
	require.Equal(t, TypeAttach, m.Type)

	ft, payload, err := ReadFrame(rc.Reader())
	require.NoError(t, err, "the frame must survive the preamble read")
	assert.Equal(t, FrameStdout, ft)
	assert.Equal(t, "post-preamble", string(payload))
}
