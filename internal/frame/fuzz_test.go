package frame_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"binhttp/internal/frame"
)

// FuzzParse asserts what SPEC.md section 6 rests on: for any bytes at all,
// Parse either returns one whole frame, consuming exactly its header plus its
// declared Length, or asks for more and consumes nothing. There is no third
// outcome, so a receiver can never lose its place in the stream.
func FuzzParse(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("BH/1"))
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"))
	f.Add([]byte{0x00, 0x00, 0x01, 0x01, 0x00, 0x00, 0x00, 0x01})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00})

	f.Fuzz(func(t *testing.T, data []byte) {
		fr, n, err := frame.Parse(data)
		if err != nil {
			if !errors.Is(err, frame.ErrNeedMore) {
				t.Fatalf("err = %v; the only possible error is ErrNeedMore", err)
			}
			if n != 0 {
				t.Fatalf("ErrNeedMore with n = %d", n)
			}
			if len(data) >= 8 && len(data) >= 8+int(binary.BigEndian.Uint16(data)) {
				t.Fatalf("asked for more with a whole frame buffered (%d bytes)", len(data))
			}
			return
		}
		declared := int(binary.BigEndian.Uint16(data))
		if n != 8+declared {
			t.Fatalf("n = %d, want 8 + %d", n, declared)
		}
		if !bytes.Equal(fr.Payload, data[8:n]) {
			t.Fatal("payload does not match the bytes after the header")
		}
	})
}
