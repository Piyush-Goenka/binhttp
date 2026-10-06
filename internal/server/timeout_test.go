package server_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binhttp/internal/frame"
	"binhttp/internal/message"
	"binhttp/internal/server"
)

// Timeouts are injected as small values so these tests take milliseconds.

func TestIdleConnectionIsClosed(t *testing.T) {
	srv, _ := start(t, func(c *server.Config) { c.IdleTimeout = 150 * time.Millisecond })
	p := open(t, srv)
	began := time.Now()
	p.expectClosed()
	if waited := time.Since(began); waited < 100*time.Millisecond {
		t.Fatalf("closed after %v, before the idle timeout", waited)
	}
}

// Slowloris: a frame that dribbles in a byte at a time must not keep the
// connection alive. The deadline is anchored to the frame's first byte, so
// each new byte does not buy more time.
func TestDribbledFrameIsCutOff(t *testing.T) {
	srv, _ := start(t, func(c *server.Config) {
		c.IdleTimeout = 10 * time.Second
		c.FrameTimeout = 200 * time.Millisecond
	})
	p := open(t, srv)
	whole := frameBytes(t, frame.TypeRequest, frame.EndStream, 1, requestPayload(t, message.MethodGET, "/hello.txt"))

	// One byte every 100ms: always inside a 200ms window of the previous
	// byte, and about 2.4s for the whole frame. A deadline that restarted on
	// every read would never fire.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for _, b := range whole {
			if _, err := p.conn.Write([]byte{b}); err != nil {
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()

	began := time.Now()
	p.expectClosed()
	if waited := time.Since(began); waited > 800*time.Millisecond {
		t.Fatalf("a dribbling frame held the connection for %v; the frame clock must not restart", waited)
	}
}

func TestDribbledPrefaceIsCutOff(t *testing.T) {
	srv, _ := start(t, func(c *server.Config) {
		c.IdleTimeout = 10 * time.Second
		c.FrameTimeout = 150 * time.Millisecond
	})
	p := connect(t, srv)
	p.send([]byte("BH")) // half a preface, then nothing
	p.expectPreface()
	began := time.Now()
	p.expectClosed()
	if waited := time.Since(began); waited > 2*time.Second {
		t.Fatalf("half a preface held the connection for %v", waited)
	}
}

// A peer that stops reading cannot pin the server: once a write has been
// blocked for WriteTimeout, the connection is dropped.
func TestPeerThatStopsReadingIsDropped(t *testing.T) {
	srv, _ := start(t, func(c *server.Config) { c.WriteTimeout = 200 * time.Millisecond })
	p := open(t, srv)
	var batch []byte
	for i := range 400 { // 400 x 100 KB, far more than the socket buffers hold
		batch = append(batch, frameBytes(t, frame.TypeRequest, frame.EndStream, uint32(i+1), requestPayload(t, message.MethodGET, "/big.bin"))...)
	}
	p.send(batch)

	deadline := time.Now().Add(5 * time.Second)
	for srv.LiveConns() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the server is still writing to a peer that stopped reading")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Shutdown must not wait out the idle timeout of a parked keep-alive
// connection: it has nothing in flight, so it is closed at once.
func TestShutdownClosesIdleConnectionsPromptly(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Root = newRoot(t)
	cfg.IdleTimeout = time.Minute
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	p := open(t, srv)
	p.get(1, "/hello.txt")
	p.readResponse(1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	began := time.Now()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("shutdown took %v with only an idle connection", took)
	}
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	p.expectClosed()
}

// A frame's clock measures the peer, not the server. Time spent writing an
// earlier response must not count against the next pipelined frame, even when
// that frame's first bytes arrived while the response was being written.
func TestSlowResponseDoesNotTimeOutTheNextPipelinedFrame(t *testing.T) {
	srv, root := start(t, func(c *server.Config) { c.FrameTimeout = 300 * time.Millisecond })
	huge := make([]byte, 16<<20) // far more than the socket buffers hold
	if err := os.WriteFile(filepath.Join(root, "huge.bin"), huge, 0o644); err != nil {
		t.Fatal(err)
	}
	p := open(t, srv)
	second := frameBytes(t, frame.TypeRequest, frame.EndStream, 3, requestPayload(t, message.MethodGET, "/hello.txt"))
	p.send(append(frameBytes(t, frame.TypeRequest, frame.EndStream, 1, requestPayload(t, message.MethodGET, "/huge.bin")), second[:5]...))
	time.Sleep(time.Second) // not reading: the server is stuck writing for 3x FrameTimeout
	p.send(second[5:])

	if r := p.readResponse(1); len(r.body) != len(huge) {
		t.Fatalf("stream 1 body: %d bytes, want %d", len(r.body), len(huge))
	}
	if r := p.readResponse(3); r.Status != 200 {
		t.Fatalf("stream 3: %d, want 200", r.Status)
	}
}

// Only requests count as activity. A peer that sends nothing but frames the
// server skips is idle, and is closed after the idle timeout like any other.
func TestSkippableFramesDoNotKeepAConnectionAlive(t *testing.T) {
	srv, _ := start(t, func(c *server.Config) { c.IdleTimeout = 300 * time.Millisecond })
	p := open(t, srv)
	junk := frameBytes(t, frame.Type(0x77), 0, 1, nil)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			if _, err := p.conn.Write(junk); err != nil {
				return
			}
		}
	}()
	began := time.Now()
	p.expectClosed()
	if waited := time.Since(began); waited > 1500*time.Millisecond {
		t.Fatalf("frames every 100ms held an idle connection for %v", waited)
	}
}

// The connection cap holds further clients in the listen backlog instead of
// accepting them: the third client is not served until a slot frees up.
func TestConnectionCapQueuesExtraClients(t *testing.T) {
	srv, _ := start(t, func(c *server.Config) { c.MaxConns = 2 })
	first := open(t, srv)
	open(t, srv)

	third := connect(t, srv)
	_ = third.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _ := third.conn.Read(make([]byte, 4)); n > 0 {
		t.Fatal("a third client was served past a cap of 2")
	}
	first.conn.Close()
	third.expectPreface() // its turn now
	third.send([]byte(frame.Preface))
	third.get(1, "/hello.txt")
	if r := third.readResponse(1); r.Status != 200 {
		t.Fatalf("status %d", r.Status)
	}
}

// A peer can keep the read buffer from ever being empty by sending the end of
// one skippable frame together with the start of the next. That must not
// dodge the idle clock: only requests count as activity.
func TestBackToBackSkippableFramesDoNotDodgeTheIdleClock(t *testing.T) {
	srv, _ := start(t, func(c *server.Config) { c.IdleTimeout = 300 * time.Millisecond })
	p := open(t, srv)
	junk := frameBytes(t, frame.Type(0x77), 0, 1, nil) // 8 bytes
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		if _, err := p.conn.Write(junk[:1]); err != nil {
			return
		}
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			// The rest of this frame and the first byte of the next.
			if _, err := p.conn.Write(append(append([]byte{}, junk[1:]...), junk[0])); err != nil {
				return
			}
		}
	}()
	began := time.Now()
	p.expectClosed()
	if waited := time.Since(began); waited > 1500*time.Millisecond {
		t.Fatalf("frames that never empty the buffer held an idle connection for %v", waited)
	}
}

// The other side of the pipelining fix: a partial frame left over after a
// response still has to finish within the frame timeout.
func TestPartialFrameLeftAfterAResponseStillTimesOut(t *testing.T) {
	srv, _ := start(t, func(c *server.Config) {
		c.IdleTimeout = 10 * time.Second
		c.FrameTimeout = 300 * time.Millisecond
	})
	p := open(t, srv)
	second := frameBytes(t, frame.TypeRequest, frame.EndStream, 3, requestPayload(t, message.MethodGET, "/hello.txt"))
	p.send(append(frameBytes(t, frame.TypeRequest, frame.EndStream, 1, requestPayload(t, message.MethodGET, "/hello.txt")), second[:5]...))
	p.readResponse(1)
	began := time.Now()
	p.expectClosed() // the rest of stream 3 never comes
	if waited := time.Since(began); waited > 1500*time.Millisecond {
		t.Fatalf("an unfinished frame held the connection for %v after the response", waited)
	}
}

// Shutdown while clients keep connecting. Serve adds each new connection to
// the WaitGroup that Shutdown waits on; that Add must never race the Wait.
// Run with -race.
func TestShutdownWhileClientsConnect(t *testing.T) {
	for range 20 {
		cfg := server.DefaultConfig()
		cfg.Addr = "127.0.0.1:0"
		cfg.Root = t.TempDir()
		srv, err := server.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Listen(); err != nil {
			t.Fatal(err)
		}
		served := make(chan error, 1)
		go func() { served <- srv.Serve() }()
		stop := make(chan struct{})
		done := make(chan struct{})
		for range 4 {
			go func() {
				defer func() { done <- struct{}{} }()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if c, err := net.DialTimeout("tcp", srv.Addr(), 200*time.Millisecond); err == nil {
						c.Close()
					}
				}
			}()
		}
		time.Sleep(20 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		cancel()
		close(stop)
		for range 4 {
			<-done
		}
		if err := <-served; err != nil {
			t.Fatalf("serve: %v", err)
		}
	}
}
