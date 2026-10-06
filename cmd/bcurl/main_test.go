package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binhttp/internal/frame"
	"binhttp/internal/message"
	"binhttp/internal/server"
)

func startServer(t *testing.T) *server.Server {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{"index.html": "<h1>hi</h1>\n", "hello.txt": "hello, world\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := server.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Root = root
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

// statusServer answers each request it receives with the next status in
// statuses, with no body.
func statusServer(t *testing.T, statuses ...uint16) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte(frame.Preface))
		buf := make([]byte, 0, 4096)
		chunk := make([]byte, 4096)
		for _, status := range statuses {
			for {
				rest := buf
				if len(rest) >= 4 && string(rest[:4]) == frame.Preface {
					rest = rest[4:]
				}
				if f, n, err := frame.Parse(rest); err == nil {
					buf = rest[n:]
					p, _ := message.Response{Status: status}.Append(nil)
					out, _ := frame.Append(nil, frame.TypeResponse, frame.EndStream, f.Stream, p)
					_, _ = c.Write(out)
					break
				}
				n, err := c.Read(chunk)
				if n == 0 && err != nil {
					return
				}
				buf = append(buf, chunk[:n]...)
			}
		}
		_, _ = c.Read(chunk) // wait for the client to hang up
	}()
	return ln.Addr().String()
}

func bcurl(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestBodyGoesToStdoutAndNothingElse(t *testing.T) {
	srv := startServer(t)
	code, out, errOut := bcurl(srv.Addr() + "/hello.txt")
	if code != 0 || out != "hello, world\n" || errOut != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestErrorStatusesExitNonZero(t *testing.T) {
	srv := startServer(t)
	code, out, errOut := bcurl(srv.Addr() + "/missing.html")
	if code != 4 {
		t.Fatalf("404: exit %d, want 4", code)
	}
	if !strings.Contains(out, "404 Not Found") || !strings.Contains(errOut, "404") {
		t.Fatalf("stdout %q, stderr %q", out, errOut)
	}
	if code, _, _ := bcurl(statusServer(t, 503) + "/x"); code != 5 {
		t.Fatalf("503: exit %d, want 5", code)
	}
}

// Several URLs share one connection, bodies are concatenated in order, and
// the worst status decides the exit code.
func TestManyURLsOneConnection(t *testing.T) {
	srv := startServer(t)
	a := srv.Addr()
	code, out, _ := bcurl(a+"/hello.txt", a+"/missing", a+"/index.html")
	if code != 4 {
		t.Fatalf("exit %d, want 4", code)
	}
	if !strings.HasPrefix(out, "hello, world\n") || !strings.HasSuffix(out, "<h1>hi</h1>\n") || !strings.Contains(out, "404") {
		t.Fatalf("stdout %q", out)
	}
	if n := srv.AcceptCount(); n != 1 {
		t.Fatalf("server accepted %d connections, want 1", n)
	}
	if code, _, _ := bcurl(func() []string {
		addr := statusServer(t, 404, 503, 200)
		return []string{addr + "/a", addr + "/b", addr + "/c"}
	}()...); code != 5 {
		t.Fatalf("404, 503, 200: exit %d, want 5", code)
	}
}

func TestDifferentServersAreRefusedBeforeConnecting(t *testing.T) {
	srv := startServer(t)
	code, _, errOut := bcurl(srv.Addr()+"/hello.txt", "127.0.0.1:1/x")
	if code != 2 || !strings.Contains(errOut, "second connection") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if n := srv.AcceptCount(); n != 0 {
		t.Fatalf("connected %d times before refusing", n)
	}
}

// Host names are not case-sensitive, so these two URLs name one server and
// share its one connection.
func TestServerNamesCompareWithoutCase(t *testing.T) {
	srv := startServer(t)
	_, port, _ := net.SplitHostPort(srv.Addr())
	code, out, errOut := bcurl("LOCALHOST:"+port+"/hello.txt", "localhost:"+port+"/hello.txt")
	if code != 0 || out != "hello, world\nhello, world\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if n := srv.AcceptCount(); n != 1 {
		t.Fatalf("server accepted %d connections, want 1", n)
	}
}

func TestUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"http://localhost:9000/x"},
		{"-nosuchflag", "localhost/x"},
		{"-timeout", "0", "localhost/x"},
		{"-timeout", "-1s", "localhost/x"},
		{"-timeout", "4s", "localhost/x"}, // SPEC.md section 6: at least 5 seconds per frame
	} {
		if code, _, _ := bcurl(args...); code != 2 {
			t.Errorf("bcurl %q: exit %d, want 2", args, code)
		}
	}
	if code, _, errOut := bcurl("-h"); code != 0 || !strings.Contains(errOut, "usage") {
		t.Errorf("-h: exit %d, stderr %q", code, errOut)
	}
}

func TestConnectionAndProtocolFailuresExit1(t *testing.T) {
	if code, _, errOut := bcurl("127.0.0.1:1/x"); code != 1 || errOut == "" {
		t.Errorf("refused: exit %d, stderr %q", code, errOut)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			time.Sleep(time.Second)
			c.Close()
		}
	}()
	if code, _, errOut := bcurl(ln.Addr().String() + "/x"); code != 1 || !strings.Contains(errOut, "preface") {
		t.Errorf("not BH/1: exit %d, stderr %q", code, errOut)
	}
}

// -v may come after the URL, writes its hexdump to stderr only, and leaves
// stdout byte-identical to a quiet run.
func TestVerboseWritesOnlyToStderr(t *testing.T) {
	srv := startServer(t)
	code, out, errOut := bcurl(srv.Addr()+"/index.html", "-v")
	if code != 0 || out != "<h1>hi</h1>\n" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	for _, want := range []string{"> 00000000  42 48 2f 31", "> REQUEST", "< RESPONSE", "< DATA"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q", want)
		}
	}
}
