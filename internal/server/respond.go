package server

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"

	"binhttp/internal/frame"
	"binhttp/internal/message"
)

// dataChunk is the most bserve puts in one DATA frame. The protocol allows
// 65,535 bytes; 16 KiB, HTTP/2's default frame size, lets a client start
// writing a large body sooner for an overhead of 8 bytes in 16 KiB (0.05%).
const dataChunk = 16 << 10

// respond answers one REQUEST frame: a RESPONSE, then the body as DATA
// frames, every frame on the request's stream. A returned error means the
// connection must close.
func (s *Server) respond(w *bufio.Writer, conn net.Conn, f frame.Frame) error {
	r, what := s.handle(f)
	if r.file != nil {
		defer r.file.Close()
	}
	s.logf("%s stream %d %s -> %d (%d bytes)", conn.RemoteAddr(), f.Stream, what, r.status, r.size)

	head, err := message.Response{Status: uint16(r.status), Fields: r.fields}.Append(nil)
	if err != nil {
		return err
	}
	var flags frame.Flags
	if r.size == 0 {
		flags = frame.EndStream // no body, so the RESPONSE ends the message
	}
	if err := s.writeFrame(w, conn, frame.TypeResponse, flags, f.Stream, head); err != nil {
		return err
	}
	if r.size == 0 {
		return nil
	}
	var src io.Reader = bytes.NewReader(r.body)
	if r.file != nil {
		src = r.file
	}
	return s.sendBody(w, conn, f.Stream, src, r.size)
}

// sendBody sends exactly size bytes from src as DATA frames, setting
// END_STREAM on the last.
func (s *Server) sendBody(w *bufio.Writer, conn net.Conn, stream uint32, src io.Reader, size int64) error {
	chunk := make([]byte, dataChunk)
	for left := size; left > 0; {
		n := int(min(left, dataChunk))
		if _, err := io.ReadFull(src, chunk[:n]); err != nil {
			// The status is already on its way and cannot be taken back. The
			// connection must close without END_STREAM so the client detects
			// the incomplete body (SPEC.md section 6).
			return fmt.Errorf("reading the body failed after the response started: %w", err)
		}
		left -= int64(n)
		var flags frame.Flags
		if left == 0 {
			flags = frame.EndStream
		}
		if err := s.writeFrame(w, conn, frame.TypeData, flags, stream, chunk[:n]); err != nil {
			return err
		}
	}
	return nil
}

// writeFrame encodes one frame into the connection's writer. The write
// deadline is renewed per frame, so a large body may take as long as it needs
// while any single stall longer than WriteTimeout drops the peer.
func (s *Server) writeFrame(w *bufio.Writer, conn net.Conn, t frame.Type, flags frame.Flags, stream uint32, payload []byte) error {
	out, err := frame.Append(nil, t, flags, stream, payload)
	if err != nil {
		return err
	}
	s.setWriteDeadline(conn)
	if _, err := w.Write(out); err != nil {
		return fmt.Errorf("write failed: %w", err)
	}
	return nil
}
