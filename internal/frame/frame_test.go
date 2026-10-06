package frame_test

import (
	"bytes"
	"errors"
	"testing"

	"binhttp/internal/frame"
)

// The expected bytes in this file are written out by hand from the diagram in
// SPEC.md section 2, never produced by the encoder under test.

func TestAppendEncodesHeaderBigEndian(t *testing.T) {
	got, err := frame.Append(nil, frame.TypeRequest, frame.EndStream, 7, []byte{0xAA, 0xBB, 0xCC})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x00, 0x03, // Length 3
		0x01,                   // Type REQUEST
		0x01,                   // Flags END_STREAM
		0x00, 0x00, 0x00, 0x07, // Stream ID 7
		0xAA, 0xBB, 0xCC, // payload
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got  % x\nwant % x", got, want)
	}
}

// Every bit of every field is carried: in particular the top bit of the stream
// ID, which HTTP/2 reserves and BH/1 does not.
func TestAppendCarriesFullFieldWidths(t *testing.T) {
	payload := make([]byte, 65535)
	got, err := frame.Append([]byte{0x99}, frame.Type(0xFE), frame.Flags(0x80), 0xDEADBEEF, payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1+8+65535 {
		t.Fatalf("len %d, want %d", len(got), 1+8+65535)
	}
	wantHead := []byte{0x99, 0xFF, 0xFF, 0xFE, 0x80, 0xDE, 0xAD, 0xBE, 0xEF}
	if !bytes.Equal(got[:9], wantHead) {
		t.Fatalf("head % x, want % x", got[:9], wantHead)
	}
}

func TestAppendRejectsPayloadTheLengthFieldCannotHold(t *testing.T) {
	dst := []byte{1, 2, 3}
	got, err := frame.Append(dst, frame.TypeData, 0, 1, make([]byte, 65536))
	if !errors.Is(err, frame.ErrPayloadTooLarge) {
		t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
	}
	if !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("dst changed to % x", got)
	}
}

func TestParseDecodesHandWrittenFrame(t *testing.T) {
	buf := []byte{
		0x00, 0x02, 0x02, 0x01, 0x01, 0x02, 0x03, 0x04, 'h', 'i', // RESPONSE, END_STREAM, stream 0x01020304, "hi"
		0x00, 0x00, 0x03, // the start of the next frame, which must stay unread
	}
	f, n, err := frame.Parse(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("n = %d, want 10", n)
	}
	want := frame.Header{Length: 2, Type: frame.TypeResponse, Flags: frame.EndStream, Stream: 0x01020304}
	if f.Header != want {
		t.Fatalf("header %+v, want %+v", f.Header, want)
	}
	if string(f.Payload) != "hi" {
		t.Fatalf("payload %q, want %q", f.Payload, "hi")
	}
}

// A TCP read can stop anywhere, so every strict prefix of a frame must ask for
// more bytes and consume none.
func TestParseNeedsMoreAtEveryPrefix(t *testing.T) {
	whole := []byte{0x00, 0x05, 0x03, 0x00, 0x00, 0x00, 0x00, 0x09, 'h', 'e', 'l', 'l', 'o'}
	for i := 0; i < len(whole); i++ {
		_, n, err := frame.Parse(whole[:i])
		if !errors.Is(err, frame.ErrNeedMore) || n != 0 {
			t.Fatalf("prefix %d: n=%d err=%v, want 0 and ErrNeedMore", i, n, err)
		}
	}
	if _, n, err := frame.Parse(whole); err != nil || n != len(whole) {
		t.Fatalf("whole frame: n=%d err=%v", n, err)
	}
}

// Every 8-byte header is valid: the skip rule needs nothing but Length, so an
// unknown type, every flag bit and stream 0 still parse.
func TestParseAcceptsAnyHeader(t *testing.T) {
	buf := []byte{0x00, 0x01, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00, 0x42}
	f, n, err := frame.Parse(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 9 || f.Type != 0xFF || f.Flags != 0xFF || f.Stream != 0 || !bytes.Equal(f.Payload, []byte{0x42}) {
		t.Fatalf("got %+v n=%d", f, n)
	}
}

func TestParseZeroLengthFrame(t *testing.T) {
	f, n, err := frame.Parse([]byte{0x00, 0x00, 0x03, 0x01, 0x00, 0x00, 0x00, 0x01})
	if err != nil || n != 8 || len(f.Payload) != 0 || !f.Flags.Has(frame.EndStream) {
		t.Fatalf("got %+v n=%d err=%v", f, n, err)
	}
}

// The connection loops reuse their read buffers, so a payload that aliased the
// buffer would change under the caller.
func TestParsePayloadIsACopy(t *testing.T) {
	buf := []byte{0x00, 0x02, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01, 'o', 'k'}
	f, _, err := frame.Parse(buf)
	if err != nil {
		t.Fatal(err)
	}
	buf[8], buf[9] = 'X', 'X'
	if string(f.Payload) != "ok" {
		t.Fatalf("payload changed to %q", f.Payload)
	}
}

func TestMatchPreface(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		wantN int
		want  error
	}{
		{"exact", "BH/1", 4, nil},
		{"followed by a frame", "BH/1\x00\x00\x03", 4, nil},
		{"empty", "", 0, frame.ErrNeedMore},
		{"partial", "BH", 0, frame.ErrNeedMore},
		{"wrong first byte fails at once", "G", 0, frame.ErrBadPreface},
		{"wrong version", "BH/2", 0, frame.ErrBadPreface},
		{"http/1.1 request", "GET / HTTP/1.1\r\n", 0, frame.ErrBadPreface},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := frame.MatchPreface([]byte(tt.in))
			if n != tt.wantN || !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Fatalf("MatchPreface(%q) = %d, %v; want %d, %v", tt.in, n, err, tt.wantN, tt.want)
			}
		})
	}
}
