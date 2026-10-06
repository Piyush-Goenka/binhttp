// Package server is bserve: it serves the files under one root over BH/1. It
// owns every socket and file operation; the protocol itself lives in the
// I/O-free packages internal/frame and internal/message.
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
)

// Config holds the tunables. Timeouts are fields rather than constants so
// tests can inject 100ms values instead of sleeping.
type Config struct {
	Addr string // TCP address to listen on
	Root string // directory whose files are served

	// IdleTimeout bounds the wait for the first byte of a frame (or of the
	// preface) on a connection with nothing in flight.
	IdleTimeout time.Duration
	// FrameTimeout bounds how long a frame may take to arrive once its first
	// byte has, so a peer dribbling bytes cannot hold the connection open.
	FrameTimeout time.Duration
	// WriteTimeout bounds one blocked write to a peer that stopped reading.
	// Zero disables it.
	WriteTimeout time.Duration

	// MaxConns caps how many connections are served at once. Beyond it,
	// clients wait in the listen backlog until a slot frees up. Zero means no
	// cap.
	MaxConns int

	Logger *log.Logger // nil discards
}

// DefaultConfig returns the values bserve runs with.
func DefaultConfig() Config {
	return Config{
		Addr:         ":9000",
		IdleTimeout:  15 * time.Second,
		FrameTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second,
		MaxConns:     1024,
	}
}

// Server accepts connections and serves the root on each of them.
type Server struct {
	cfg     Config
	root    *os.Root
	ln      net.Listener
	wg      sync.WaitGroup
	accepts atomic.Int64
	closing atomic.Bool
	stop    chan struct{} // closed when Shutdown begins
	slots   chan struct{} // one token per live connection; nil means no cap

	mu    sync.Mutex
	conns map[net.Conn]*connState
}

// connState is the per-connection bookkeeping Shutdown needs. A connection is
// idle while it waits for a new frame with nothing buffered, the only state
// in which closing it cannot cut a request or response in half.
type connState struct {
	idle atomic.Bool
}

// New opens the root and builds a Server. Every file is opened through an
// os.Root, which refuses any path that leaves the root, including through
// "..", absolute symbolic links and symbolic links that climb out.
func New(cfg Config) (*Server, error) {
	fi, err := os.Stat(cfg.Root)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", cfg.Root)
	}
	root, err := os.OpenRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, root: root, stop: make(chan struct{}), conns: make(map[net.Conn]*connState)}
	if cfg.MaxConns > 0 {
		s.slots = make(chan struct{}, cfg.MaxConns)
	}
	return s, nil
}

// Listen binds the configured address. It is separate from Serve so a test
// can bind port 0 and then read the port it got from Addr.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.logf("serving %s on %s", s.cfg.Root, ln.Addr())
	return nil
}

// Addr reports the bound address, or "" before Listen.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// AcceptCount reports how many TCP connections have been accepted. It is what
// proves that many requests travelled over one connection.
func (s *Server) AcceptCount() int64 { return s.accepts.Load() }

// LiveConns reports how many connections are being served right now.
func (s *Server) LiveConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// Serve accepts connections until Shutdown closes the listener.
//
// It takes a connection slot before each Accept, so at the cap further
// clients wait in the kernel's listen backlog rather than being accepted and
// starved. An Accept that fails because the process or kernel is short of
// something is retried after a pause that doubles up to a second, as net/http
// does: running out of file descriptors must not stop the server for good.
func (s *Server) Serve() error {
	var pause time.Duration
	for {
		if !s.takeSlot() {
			return nil
		}
		conn, err := s.ln.Accept()
		if err != nil {
			s.giveSlot()
			if s.closing.Load() {
				return nil
			}
			if !retryable(err) {
				return err
			}
			pause = min(max(2*pause, 5*time.Millisecond), time.Second)
			s.logf("accept failed: %v; retrying in %v", err, pause)
			select {
			case <-time.After(pause):
			case <-s.stop:
				return nil
			}
			continue
		}
		pause = 0
		st, ok := s.track(conn)
		if !ok { // Shutdown began after Accept returned
			_ = conn.Close()
			s.giveSlot()
			return nil
		}
		n := s.accepts.Add(1)
		s.logf("%s connected (connection %d)", conn.RemoteAddr(), n)

		go func() {
			defer s.wg.Done()
			defer s.giveSlot()
			defer s.untrack(conn)
			s.serveConn(conn, st)
		}()
	}
}

// takeSlot waits for room under MaxConns. It reports false once Shutdown has
// begun.
func (s *Server) takeSlot() bool {
	if s.slots == nil {
		return true
	}
	select {
	case s.slots <- struct{}{}:
		return true
	case <-s.stop:
		return false
	}
}

func (s *Server) giveSlot() {
	if s.slots != nil {
		<-s.slots
	}
}

// retryable reports whether an Accept error will pass: the process or kernel
// ran short of descriptors, buffers or memory, or a client gave up during the
// handshake. Anything else means the listener itself is broken.
func retryable(err error) bool {
	for _, errno := range []syscall.Errno{
		syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM,
		syscall.ECONNABORTED, syscall.EINTR,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Shutdown stops accepting and waits for live connections to finish, forcing
// them closed if ctx expires first. It closes the root last.
func (s *Server) Shutdown(ctx context.Context) error {
	// Taking the lock orders this against track: a connection is either
	// added to the WaitGroup before Wait starts, or refused.
	s.mu.Lock()
	if s.closing.CompareAndSwap(false, true) {
		close(s.stop)
	}
	s.mu.Unlock()
	if s.ln != nil {
		_ = s.ln.Close()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	// An idle connection is parked in a read that may not return for the
	// whole idle timeout, and it has no work to lose, so close it at once.
	s.closeIdle()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return s.root.Close()
		case <-ticker.C:
			// Catch connections that went idle after the sweep above.
			s.closeIdle()
		case <-ctx.Done():
			s.mu.Lock()
			for c := range s.conns {
				_ = c.Close()
			}
			s.mu.Unlock()
			<-done
			_ = s.root.Close()
			return ctx.Err()
		}
	}
}

// closeIdle closes every connection that is waiting for a new frame.
func (s *Server) closeIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c, st := range s.conns {
		if st.idle.Load() {
			_ = c.Close()
		}
	}
}

// track registers a new connection and counts it in the WaitGroup Shutdown
// waits on. Both happen under the lock Shutdown takes before it starts
// waiting, so the WaitGroup's Add never races its Wait. Once Shutdown has
// begun, track refuses the connection.
func (s *Server) track(c net.Conn) (*connState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing.Load() {
		return nil, false
	}
	st := &connState{}
	s.conns[c] = st
	s.wg.Add(1)
	return st, true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// logf writes one log line. Paths, and errors quoting file names built from
// them, come off the network, so control bytes are escaped here, once, for
// every call site: a newline in a path cannot start a forged line.
func (s *Server) logf(format string, args ...any) {
	if s.cfg.Logger == nil {
		return
	}
	line := fmt.Sprintf(format, args...)
	if strings.ContainsFunc(line, unicode.IsControl) {
		var b strings.Builder
		for _, r := range line {
			if unicode.IsControl(r) {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				b.WriteRune(r)
			}
		}
		line = b.String()
	}
	s.cfg.Logger.Print(line)
}
