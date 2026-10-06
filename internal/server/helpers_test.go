package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"binhttp/internal/frame"
	"binhttp/internal/message"
	"binhttp/internal/server"
)

// syncBuffer is a bytes.Buffer safe to write from the server's goroutines
// while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// bigBody is larger than one 16 KiB DATA frame and not a multiple of it.
var bigBody = func() []byte {
	b := make([]byte, 100_000)
	for i := range b {
		b[i] = byte(i * 7 % 251)
	}
	return b
}()

// newRoot builds a document root holding one of every kind of entry the
// server has to handle, plus a file outside the root that must stay unreachable.
func newRoot(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "www")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, filepath.Join(root, "sub"), filepath.Join(root, "noindex"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{
		"www/index.html":       []byte("<h1>home</h1>\n"),
		"www/hello.txt":        []byte("hello, world\n"),
		"www/empty.txt":        {},
		"www/big.bin":          bigBody,
		"www/sub/index.html":   []byte("<p>sub</p>\n"),
		"www/noindex/note.txt": []byte("no index here\n"),
		"www/locked.txt":       []byte("locked\n"),
		"outside/secret.txt":   []byte("secret\n"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(base, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "locked.txt"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside/secret.txt", filepath.Join(root, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// start runs a server on an ephemeral port and shuts it down when the test
// ends, failing the test if either step goes wrong.
func start(t *testing.T, mutate func(*server.Config)) (*server.Server, string) {
	t.Helper()
	cfg := server.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Root = newRoot(t)
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return srv, cfg.Root
}

// peer is a raw BH/1 client that builds and checks frames by hand.
type peer struct {
	t    *testing.T
	conn *net.TCPConn
	buf  []byte
}

// connect opens a TCP connection without sending anything.
func connect(t *testing.T, srv *server.Server) *peer {
	t.Helper()
	c, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &peer{t: t, conn: c.(*net.TCPConn)}
}

// open connects and exchanges prefaces in both directions.
func open(t *testing.T, srv *server.Server) *peer {
	t.Helper()
	p := connect(t, srv)
	p.send([]byte(frame.Preface))
	p.expectPreface()
	return p
}

func (p *peer) send(b []byte) {
	p.t.Helper()
	_ = p.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := p.conn.Write(b); err != nil {
		p.t.Fatalf("write: %v", err)
	}
}

// frameBytes encodes one frame, failing the test if it cannot be encoded.
func frameBytes(t *testing.T, typ frame.Type, flags frame.Flags, stream uint32, payload []byte) []byte {
	t.Helper()
	b, err := frame.Append(nil, typ, flags, stream, payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// requestPayload encodes a REQUEST payload.
func requestPayload(t *testing.T, method uint8, path string, fields ...message.Field) []byte {
	t.Helper()
	b, err := message.Request{Method: method, Path: path, Fields: fields}.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// get sends a well-formed GET for path on stream.
func (p *peer) get(stream uint32, path string) {
	p.t.Helper()
	p.send(frameBytes(p.t, frame.TypeRequest, frame.EndStream, stream, requestPayload(p.t, message.MethodGET, path)))
}

func (p *peer) fill() error {
	chunk := make([]byte, 64<<10)
	_ = p.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := p.conn.Read(chunk)
	p.buf = append(p.buf, chunk[:n]...)
	if n > 0 {
		return nil
	}
	return err
}

func (p *peer) expectPreface() {
	p.t.Helper()
	for len(p.buf) < len(frame.Preface) {
		if err := p.fill(); err != nil {
			p.t.Fatalf("waiting for the server preface: %v (got % x)", err, p.buf)
		}
	}
	if got := string(p.buf[:4]); got != "BH/1" {
		p.t.Fatalf("server preface %q, want %q", got, "BH/1")
	}
	p.buf = p.buf[4:]
}

func (p *peer) readFrame() frame.Frame {
	p.t.Helper()
	for {
		f, n, err := frame.Parse(p.buf)
		if err == nil {
			p.buf = p.buf[n:]
			return f
		}
		if err := p.fill(); err != nil {
			p.t.Fatalf("reading a frame: %v (buffered % x)", err, p.buf)
		}
	}
}

// response is one complete response as the test client saw it.
type response struct {
	message.Response
	head frame.Frame
	data []frame.Frame
	body []byte
}

func (r response) header(name string) string {
	v, _ := message.Get(r.Fields, name)
	return v
}

// readResponse reads a RESPONSE and its DATA frames, checking that every one
// carries the expected stream ID and that END_STREAM ends the message.
func (p *peer) readResponse(stream uint32) response {
	p.t.Helper()
	head := p.readFrame()
	if head.Type != frame.TypeResponse || head.Stream != stream {
		p.t.Fatalf("got %v on stream %d, want RESPONSE on stream %d", head.Type, head.Stream, stream)
	}
	parsed, err := message.ParseResponse(head.Payload)
	if err != nil {
		p.t.Fatalf("RESPONSE payload: %v", err)
	}
	r := response{Response: parsed, head: head}
	if head.Flags.Has(frame.EndStream) {
		return r
	}
	for {
		f := p.readFrame()
		if f.Type != frame.TypeData || f.Stream != stream {
			p.t.Fatalf("got %v on stream %d, want DATA on stream %d", f.Type, f.Stream, stream)
		}
		r.data = append(r.data, f)
		r.body = append(r.body, f.Payload...)
		if f.Flags.Has(frame.EndStream) {
			return r
		}
	}
}

// expectClosed asserts that the server closed the connection and sent
// nothing further.
func (p *peer) expectClosed() {
	p.t.Helper()
	for {
		if len(p.buf) > 0 {
			p.t.Fatalf("expected the connection to close, but got bytes % x", p.buf)
		}
		err := p.fill()
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
			return
		}
		p.t.Fatalf("expected the connection to close, got %v", err)
	}
}
