// Package frame encodes and decodes BH/1 frames (SPEC.md sections 1 and 2).
// It does no I/O: the socket code in internal/server and internal/client
// feeds it bytes.
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// HeaderLen is the size of every frame header.
const HeaderLen = 8

// MaxPayload is the largest payload a frame can carry. It is the width of the
// Length field rather than a negotiated limit, so no larger frame can even be
// expressed and no receiver ever needs a "frame too large" check.
const MaxPayload = 1<<16 - 1

// Preface is the four bytes each side sends before anything else. It is a
// constant string so no caller can modify it.
const Preface = "BH/1"

// MinFrameTimeout is the least time a receiver may give a frame to arrive,
// counted from when it starts waiting for the rest (SPEC.md section 6). A
// receiver that gave up sooner could cut off a slow but correct peer.
const MinFrameTimeout = 5 * time.Second

// MaxSkipped is how many frames in a row bserve and bcurl will skip before
// giving up on the peer. The MUST-skip rule makes skipping cheap but not
// free: without a cap a peer could keep a connection busy forever with frames
// that ask for nothing, as in HTTP/2's empty-frames flood (CVE-2019-9518).
// Genuine extension frames come a few per message.
const MaxSkipped = 1000

// Type says what a frame's payload is.
type Type uint8

// The frame types v1 defines. Every other value is skipped by the receiver.
const (
	TypeRequest  Type = 0x01
	TypeResponse Type = 0x02
	TypeData     Type = 0x03
)

func (t Type) String() string {
	switch t {
	case TypeRequest:
		return "REQUEST"
	case TypeResponse:
		return "RESPONSE"
	case TypeData:
		return "DATA"
	}
	return fmt.Sprintf("type 0x%02x", uint8(t))
}

// Flags is a frame's flag byte.
type Flags uint8

// EndStream marks the last frame of a message. It is the only flag v1
// defines; receivers ignore the other seven bits.
const EndStream Flags = 0x01

// Has reports whether every bit of x is set in f.
func (f Flags) Has(x Flags) bool { return f&x == x }

// Header is a decoded frame header.
type Header struct {
	Length uint16
	Type   Type
	Flags  Flags
	Stream uint32
}

// Frame is one complete frame.
type Frame struct {
	Header
	Payload []byte
}

var (
	// ErrNeedMore reports that the buffer ends inside a frame (or inside the
	// preface). The caller reads more bytes and tries again with the longer
	// buffer. It is never a reason to close the connection.
	ErrNeedMore = errors.New("frame: incomplete frame")

	// ErrBadPreface reports that the peer's first bytes are not Preface. The
	// caller closes the connection without sending anything more.
	ErrBadPreface = errors.New("frame: peer did not send the BH/1 preface")

	// ErrPayloadTooLarge reports a payload the Length field cannot express.
	ErrPayloadTooLarge = errors.New("frame: payload exceeds 65535 bytes")
)

// Append appends one frame, header then payload, to dst. A payload longer
// than MaxPayload returns dst unchanged and ErrPayloadTooLarge.
func Append(dst []byte, t Type, flags Flags, stream uint32, payload []byte) ([]byte, error) {
	if len(payload) > MaxPayload {
		return dst, ErrPayloadTooLarge
	}
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(payload)))
	dst = append(dst, byte(t), byte(flags))
	dst = binary.BigEndian.AppendUint32(dst, stream)
	return append(dst, payload...), nil
}

// Parse decodes one frame from the front of buf.
//
// On success it returns the frame and n, the exact number of bytes the frame
// occupied, so buf[n:] is the next frame, untouched. The only error is
// ErrNeedMore, with n = 0: every 8-byte header is valid, because every Length
// is legal and every Type is either handled or skipped by the caller. Parse
// therefore cannot lose its place in the stream.
//
// Parse never modifies buf, and the returned Payload is a copy.
func Parse(buf []byte) (Frame, int, error) {
	if len(buf) < HeaderLen {
		return Frame{}, 0, ErrNeedMore
	}
	h := Header{
		Length: binary.BigEndian.Uint16(buf[0:2]),
		Type:   Type(buf[2]),
		Flags:  Flags(buf[3]),
		Stream: binary.BigEndian.Uint32(buf[4:8]),
	}
	n := HeaderLen + int(h.Length)
	if len(buf) < n {
		return Frame{}, 0, ErrNeedMore
	}
	payload := make([]byte, h.Length)
	copy(payload, buf[HeaderLen:n])
	return Frame{Header: h, Payload: payload}, n, nil
}

// MatchPreface checks the front of buf against Preface. It returns the number
// of bytes the preface occupied, ErrNeedMore while buf is a correct but
// incomplete preface, or ErrBadPreface as soon as any byte differs, so a peer
// speaking another protocol is rejected on its first wrong byte.
func MatchPreface(buf []byte) (int, error) {
	n := min(len(buf), len(Preface))
	if string(buf[:n]) != Preface[:n] {
		return 0, ErrBadPreface
	}
	if n < len(Preface) {
		return 0, ErrNeedMore
	}
	return len(Preface), nil
}
