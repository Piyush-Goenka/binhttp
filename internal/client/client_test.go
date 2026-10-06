package client_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"binhttp/internal/client"
	"binhttp/internal/frame"
	"binhttp/internal/message"
	"binhttp/internal/server"
)

func opts() client.Options {
	return client.Options{Timeout: 3 * time.Second, UserAgent: "bcurl/1.0"}
}

// startServer runs the real bserve code over a small root.
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

// The request on the wire, byte for byte as SPEC.md lays it out: preface,
// then a REQUEST with END_STREAM on stream 1 carrying GET, the path and the
// three fields bcurl sends, each as its table index.
func TestGetSendsTheSpecifiedBytes(t *testing.T) {
	f := startFake(t, script{preface: "BH/1", replies: [][]byte{responseFrame(t, 1, frame.EndStream, 204)}})
	c, err := client.Dial(f.addr, "h:9", opts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Get("/a", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x00, 0x1D, 0x01, 0x01, 0x00, 0x00, 0x00, 0x01, // Length 29, REQUEST, END_STREAM, stream 1
		0x01, 0x00, 0x02, '/', 'a', // GET, Path Length 2, "/a"
		0x01, 0x00, 0x03, 'h', ':', '9', // host
		0x02, 0x00, 0x09, 'b', 'c', 'u', 'r', 'l', '/', '1', '.', '0', // user-agent
		0x03, 0x00, 0x03, '*', '/', '*', // accept
	}
	if got := f.next(t); !bytes.Equal(got, want) {
		t.Fatalf("sent\n% x\nwant\n% x", got, want)
	}
}

func TestGetStreamsTheBodyAndReportsTheStatus(t *testing.T) {
	srv := startServer(t)
	c, err := client.Dial(srv.Addr(), "localhost", opts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var body bytes.Buffer
	res, err := c.Get("/hello.txt", &body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || body.String() != "hello, world\n" || res.Bytes != 13 {
		t.Fatalf("got %d, %d bytes, %q", res.Status, res.Bytes, body.String())
	}
	if ct, _ := message.Get(res.Fields, "content-type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("content-type %q", ct)
	}
}

// Error statuses are results, not failures: the body still arrives and the
// connection stays usable.
func TestManyRequestsShareOneConnection(t *testing.T) {
	srv := startServer(t)
	c, err := client.Dial(srv.Addr(), "localhost", opts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, tc := range []struct {
		path   string
		status uint16
	}{{"/hello.txt", 200}, {"/missing", 404}, {"/", 200}, {"/hello.txt", 200}} {
		var body bytes.Buffer
		res, err := c.Get(tc.path, &body)
		if err != nil || res.Status != tc.status {
			t.Fatalf("%s: %d %v, want %d", tc.path, res.Status, err, tc.status)
		}
	}
	if n := srv.AcceptCount(); n != 1 {
		t.Fatalf("server accepted %d connections, want 1", n)
	}
}

func TestStreamIDsIncrease(t *testing.T) {
	ok := func(stream uint32) []byte { return responseFrame(t, stream, frame.EndStream, 204) }
	f := startFake(t, script{preface: "BH/1", replies: [][]byte{ok(1), ok(2), ok(3)}})
	c, err := client.Dial(f.addr, "h", opts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for want := uint32(1); want <= 3; want++ {
		if _, err := c.Get("/", &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		got, _, _ := frame.Parse(f.next(t))
		if got.Stream != want || !got.Flags.Has(frame.EndStream) {
			t.Fatalf("request %d went out on stream %d (flags %#x)", want, got.Stream, got.Flags)
		}
	}
}

// Frames of types the client does not use are skipped wherever they appear,
// including between a RESPONSE and its DATA.
func TestUnknownFramesAreSkipped(t *testing.T) {
	reply := cat(
		mustFrame(t, frame.Type(0x42), 0, 1, []byte("from version 2")),
		responseFrame(t, 1, 0, 200, message.Field{Name: "content-length", Value: "2"}),
		mustFrame(t, frame.Type(0x99), 0xFF, 0, bytes.Repeat([]byte{7}, 300)),
		mustFrame(t, frame.TypeRequest, frame.EndStream, 1, []byte{0x01, 0x00, 0x01, '/'}),
		mustFrame(t, frame.TypeData, frame.EndStream, 1, []byte("ok")),
	)
	f := startFake(t, script{preface: "BH/1", replies: [][]byte{reply}})
	c, err := client.Dial(f.addr, "h", opts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var body bytes.Buffer
	res, err := c.Get("/", &body)
	if err != nil || res.Status != 200 || body.String() != "ok" {
		t.Fatalf("got %d %q %v", res.Status, body.String(), err)
	}
}

func TestEmptyBodyEndsWithTheResponse(t *testing.T) {
	f := startFake(t, script{preface: "BH/1", replies: [][]byte{
		responseFrame(t, 1, frame.EndStream, 200, message.Field{Name: "content-length", Value: "0"}),
	}})
	c, err := client.Dial(f.addr, "h", opts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.Get("/empty", &bytes.Buffer{})
	if err != nil || res.Status != 200 || res.Bytes != 0 {
		t.Fatalf("got %+v %v", res, err)
	}
}

// An empty body may also come as one empty DATA frame carrying END_STREAM.
func TestEmptyBodyAsAnEmptyDataFrame(t *testing.T) {
	f := startFake(t, script{preface: "BH/1", replies: [][]byte{cat(
		responseFrame(t, 1, 0, 200, message.Field{Name: "content-length", Value: "0"}),
		mustFrame(t, frame.TypeData, frame.EndStream, 1, nil))}})
	c, err := client.Dial(f.addr, "h", opts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if res, err := c.Get("/empty", &bytes.Buffer{}); err != nil || res.Status != 200 || res.Bytes != 0 {
		t.Fatalf("got %+v %v", res, err)
	}
}

// Up to 1,000 skippable frames in a row are skipped; the cap is on more.
func TestAThousandSkippedFramesAreFine(t *testing.T) {
	f := startFake(t, script{preface: "BH/1", replies: [][]byte{cat(
		bytes.Repeat(mustFrame(t, frame.Type(0x77), 0, 1, nil), 1000),
		responseFrame(t, 1, frame.EndStream, 204))}})
	c, err := client.Dial(f.addr, "h", opts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if res, err := c.Get("/", &bytes.Buffer{}); err != nil || res.Status != 204 {
		t.Fatalf("got %+v %v", res, err)
	}
}

// Every way a server can break the protocol is an error naming what broke,
// never a silently wrong body.
func TestProtocolViolationsAreErrors(t *testing.T) {
	cl := func(n string) message.Field { return message.Field{Name: "content-length", Value: n} }
	cases := []struct {
		name    string
		sc      script
		mention string
	}{
		{"not a BH/1 server", script{preface: "HTTP/1.1 400 Bad Request\r\n\r\n"}, "preface"},
		{"response on another stream", script{preface: "BH/1", replies: [][]byte{responseFrame(t, 9, frame.EndStream, 200)}}, "stream"},
		{"data before the response", script{preface: "BH/1", replies: [][]byte{mustFrame(t, frame.TypeData, frame.EndStream, 1, []byte("x"))}}, "before"},
		{"data on another stream", script{preface: "BH/1", replies: [][]byte{cat(
			responseFrame(t, 1, 0, 200),
			mustFrame(t, frame.TypeData, frame.EndStream, 7, []byte("x")))}}, "stream 7"},
		{"a second response", script{preface: "BH/1", replies: [][]byte{cat(
			responseFrame(t, 1, 0, 200),
			responseFrame(t, 1, frame.EndStream, 200))}}, "second RESPONSE"},
		{"body shorter than content-length", script{preface: "BH/1", replies: [][]byte{cat(
			responseFrame(t, 1, 0, 200, cl("10")),
			mustFrame(t, frame.TypeData, frame.EndStream, 1, []byte("hi")))}}, "content-length"},
		{"body longer than content-length", script{preface: "BH/1", replies: [][]byte{cat(
			responseFrame(t, 1, 0, 200, cl("1")),
			mustFrame(t, frame.TypeData, frame.EndStream, 1, []byte("hi")))}}, "content-length"},
		{"content-length not a number", script{preface: "BH/1", replies: [][]byte{responseFrame(t, 1, frame.EndStream, 200, cl("-1"))}}, "content-length"},
		{"status out of range", script{preface: "BH/1", replies: [][]byte{responseFrame(t, 1, frame.EndStream, 42)}}, "status"},
		{"interim 1xx status", script{preface: "BH/1", replies: [][]byte{responseFrame(t, 1, frame.EndStream, 103)}}, "status"},
		{"endless skippable frames", script{preface: "BH/1", replies: [][]byte{
			bytes.Repeat(mustFrame(t, frame.Type(0x77), 0, 1, nil), 1001)}}, "skipped"},
		{"malformed response payload", script{preface: "BH/1", replies: [][]byte{mustFrame(t, frame.TypeResponse, frame.EndStream, 1, []byte{0x00})}}, "malformed"},
		{"closed before END_STREAM", script{preface: "BH/1", hangUp: true, replies: [][]byte{cat(
			responseFrame(t, 1, 0, 200),
			mustFrame(t, frame.TypeData, 0, 1, []byte("part")))}}, "END_STREAM"},
		{"closed mid-frame", script{preface: "BH/1", hangUp: true, replies: [][]byte{responseFrame(t, 1, frame.EndStream, 200)[:5]}}, "closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := startFake(t, tc.sc)
			c, err := client.Dial(f.addr, "h", opts())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_, err = c.Get("/", &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.mention) {
				t.Fatalf("err = %v, want an error mentioning %q", err, tc.mention)
			}
		})
	}
}

func TestVerboseHexdumpsEveryFrame(t *testing.T) {
	srv := startServer(t)
	o := opts()
	var log bytes.Buffer
	o.Verbose = &log
	c, err := client.Dial(srv.Addr(), "localhost", o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var body bytes.Buffer
	if _, err := c.Get("/index.html", &body); err != nil {
		t.Fatal(err)
	}
	out := log.String()
	for _, want := range []string{
		"> 00000000  42 48 2f 31", // our preface, hexdumped
		"< 00000000  42 48 2f 31", // the server's
		"> REQUEST",
		"< RESPONSE",
		"< DATA",
		"01 01 00 00 00 01", // REQUEST header: type, END_STREAM, stream 1
	} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose output lacks %q:\n%s", want, out)
		}
	}
	hexLine := regexp.MustCompile(`(?m)^[<>] [0-9a-f]{8}  `)
	if n := len(hexLine.FindAllString(out, -1)); n < 5 {
		t.Errorf("only %d hexdump lines:\n%s", n, out)
	}
	if body.String() != "<h1>hi</h1>\n" {
		t.Errorf("the body changed under -v: %q", body.String())
	}
}

func TestDialFailsFast(t *testing.T) {
	if _, err := client.Dial("127.0.0.1:1", "h", opts()); err == nil {
		t.Fatal("dialled a closed port without error")
	}
}
