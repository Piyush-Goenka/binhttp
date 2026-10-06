package server

import (
	"bufio"
	"errors"
	"net"
	"time"

	"binhttp/internal/frame"
)

// readChunk is how much one read asks for. Requests are small, so 4 KiB keeps
// an idle connection cheap; a larger frame simply takes several reads.
const readChunk = 4 << 10

// logSkips is how many skipped frames are logged one by one per connection;
// after that only a total is logged, at the end, so a peer cannot turn every
// 8 bytes it sends into a log line.
const logSkips = 10

// serveConn runs one connection to completion.
//
// The inner loop drains every complete frame already buffered before reading
// again, which is all pipelining needs. The writer is flushed only when the
// buffer holds no complete frame, immediately before the read that would
// block, so a pipelined batch is answered in one write and a sequential client
// still gets each answer before the server waits.
//
// Because frame.Parse can only succeed or ask for more, nothing a client sends
// can make this loop lose its place: a malformed REQUEST is answered with 400
// on its stream, and the next frame starts exactly Length bytes later. The
// only ways out are the connection errors of SPEC.md section 6.
//
// Two clocks decide when to give up on the peer. The idle clock runs from the
// connection's start or its last REQUEST, so only requests count as activity
// and a peer sending nothing but skippable frames is still idle. The frame
// clock runs from when the server starts waiting for the rest of a frame, so
// dribbled bytes cannot stretch it, and time the server spends writing an
// earlier response is never charged to the peer.
func (s *Server) serveConn(conn net.Conn, st *connState) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	w := bufio.NewWriter(conn)

	// Our preface goes first, without waiting for the client's (section 1).
	// It sits in the writer until the flush before the first read.
	_, _ = w.WriteString(frame.Preface)

	store := make([]byte, 0, readChunk)
	buf := store
	chunk := make([]byte, readChunk)
	prefaceSeen := false
	lastRequest := time.Now() // the idle clock
	var frameStart time.Time  // the frame clock; zero while it is not running
	skippedRun, skippedAll := 0, 0
	defer func() {
		if skippedAll > logSkips {
			s.logf("%s skipped %d frames in all (only the first %d are logged)", remote, skippedAll, logSkips)
		}
	}()

	for {
		for {
			if !prefaceSeen {
				n, err := frame.MatchPreface(buf)
				if errors.Is(err, frame.ErrNeedMore) {
					break
				}
				if err != nil {
					s.logf("%s closing: %v", remote, err)
					s.setWriteDeadline(conn)
					_ = w.Flush()
					return
				}
				buf = buf[n:]
				prefaceSeen = true
				frameStart = time.Time{}
				continue
			}

			f, n, err := frame.Parse(buf)
			if err != nil { // only ErrNeedMore is possible
				break
			}
			buf = buf[n:]
			frameStart = time.Time{} // whatever is left belongs to a new frame

			if f.Type != frame.TypeRequest {
				// The MUST-skip rule. Parse has already consumed the frame, so
				// skipping it is simply not acting on it.
				skippedRun++
				skippedAll++
				if skippedAll <= logSkips {
					s.logf("%s skipped a %v frame on stream %d (%d bytes)", remote, f.Type, f.Stream, f.Length)
				}
				if skippedRun > frame.MaxSkipped {
					s.logf("%s closing: %d frames in a row skipped without a REQUEST", remote, skippedRun)
					s.setWriteDeadline(conn)
					_ = w.Flush() // answers already given still go out
					return
				}
				// The idle clock is normally checked only when the buffer is
				// empty, and a peer can keep it from ever emptying by sending
				// the end of one frame with the start of the next. So skipped
				// frames are checked against it too.
				if time.Since(lastRequest) > s.cfg.IdleTimeout {
					s.logf("%s closing: no request for %v, only frames to skip", remote, s.cfg.IdleTimeout)
					s.setWriteDeadline(conn)
					_ = w.Flush()
					return
				}
				continue
			}
			skippedRun = 0
			if err := s.respond(w, conn, f); err != nil {
				s.logf("%s closing: %v", remote, err)
				return
			}
			lastRequest = time.Now()
		}

		if len(buf) == 0 {
			buf = store[:0] // reclaim the front of the backing array
		}
		s.setWriteDeadline(conn)
		if err := w.Flush(); err != nil {
			s.logf("%s closing: flush failed: %v", remote, err)
			return
		}

		// Which deadline applies depends on whether a frame is in flight.
		// With nothing buffered the server waits for a new frame, so the
		// generous idle timeout applies, counted from the last request. With
		// part of a frame buffered, the tight frame timeout applies, anchored
		// to when the server began waiting for it rather than to this read,
		// so dribbling cannot extend it. Bytes that arrived while the server
		// was busy writing were not late, so their clock starts only now.
		if len(buf) == 0 {
			if s.closing.Load() {
				return // between frames, so Shutdown loses nothing
			}
			st.idle.Store(true)
			_ = conn.SetReadDeadline(lastRequest.Add(s.cfg.IdleTimeout))
		} else {
			if frameStart.IsZero() {
				frameStart = time.Now()
			}
			_ = conn.SetReadDeadline(frameStart.Add(s.cfg.FrameTimeout))
		}

		n, err := conn.Read(chunk)
		st.idle.Store(false)
		if n > 0 {
			// Use what arrived before looking at err: a read can return the
			// last request and io.EOF together when the peer half-closes.
			if len(buf) == 0 {
				frameStart = time.Now()
			}
			buf = append(buf, chunk[:n]...)
			continue
		}
		if err != nil {
			var ne net.Error
			switch {
			case errors.As(err, &ne) && ne.Timeout() && len(buf) > 0:
				s.logf("%s closing: frame unfinished after %v (%d bytes buffered)", remote, s.cfg.FrameTimeout, len(buf))
			case errors.As(err, &ne) && ne.Timeout():
				s.logf("%s closing: no request for %v", remote, s.cfg.IdleTimeout)
			case len(buf) > 0:
				s.logf("%s closing: peer closed mid-frame (%d bytes buffered): %v", remote, len(buf), err)
			default:
				s.logf("%s closed the connection", remote)
			}
			return
		}
	}
}

// setWriteDeadline bounds how long one write may stall. Neither read deadline
// can see a goroutine blocked in Write, so without this a peer that stops
// reading pins a goroutine and a file descriptor until TCP gives up.
func (s *Server) setWriteDeadline(conn net.Conn) {
	if s.cfg.WriteTimeout > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
	}
}
