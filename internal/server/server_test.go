package server_test

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"binhttp/internal/frame"
	"binhttp/internal/message"
	"binhttp/internal/server"
)

const imfFixdate = "Mon, 02 Jan 2006 15:04:05 GMT"

func TestServesFileWithHeaders(t *testing.T) {
	srv, root := start(t, nil)
	p := open(t, srv)
	p.get(1, "/hello.txt")
	r := p.readResponse(1)

	if r.Status != 200 || string(r.body) != "hello, world\n" {
		t.Fatalf("got %d %q", r.Status, r.body)
	}
	fi, err := os.Stat(filepath.Join(root, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"server":         "bserve/1.0",
		"content-type":   "text/plain; charset=utf-8",
		"content-length": "13",
		"last-modified":  fi.ModTime().UTC().Format(imfFixdate),
	}
	for name, v := range want {
		if got := r.header(name); got != v {
			t.Errorf("%s = %q, want %q", name, got, v)
		}
	}
	date, err := time.Parse(imfFixdate, r.header("date"))
	if err != nil || time.Since(date).Abs() > time.Minute {
		t.Errorf("date = %q (%v), want an IMF-fixdate close to now", r.header("date"), err)
	}
	if !regexp.MustCompile(`^"[0-9a-f]+-[0-9a-f]+"$`).MatchString(r.header("etag")) {
		t.Errorf("etag = %q, want a quoted validator", r.header("etag"))
	}
}

func TestContentTypeFollowsExtension(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	for i, tc := range []struct{ path, want string }{
		{"/index.html", "text/html; charset=utf-8"},
		{"/big.bin", "application/octet-stream"},
	} {
		p.get(uint32(i+1), tc.path)
		if got := p.readResponse(uint32(i + 1)).header("content-type"); got != tc.want {
			t.Errorf("%s: content-type %q, want %q", tc.path, got, tc.want)
		}
	}
}

// The full 32 bits are echoed, including the top bit HTTP/2 reserves.
func TestStreamIDIsEchoedOnEveryFrame(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	p.get(0xDEADBEEF, "/big.bin")
	r := p.readResponse(0xDEADBEEF) // fails on any frame with another stream ID
	if len(r.data) < 2 {
		t.Fatalf("got %d DATA frames, want several", len(r.data))
	}
}

// Error responses must leave the connection usable for later requests.
func TestConnectionSurvivesErrors(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)

	p.get(1, "/missing.html")
	p.send(frameBytes(t, frame.TypeRequest, frame.EndStream, 3, []byte{0x01, 0x00, 0x09, '/'}))
	p.send(frameBytes(t, frame.TypeRequest, frame.EndStream, 5, requestPayload(t, 0x07, "/hello.txt")))
	p.get(7, "/hello.txt")

	for _, want := range []struct {
		stream uint32
		status uint16
	}{{1, 404}, {3, 400}, {5, 501}, {7, 200}} {
		if r := p.readResponse(want.stream); r.Status != want.status {
			t.Fatalf("stream %d: status %d, want %d (body %q)", want.stream, r.Status, want.status, r.body)
		}
	}
	if n := srv.AcceptCount(); n != 1 {
		t.Fatalf("accepted %d connections, want 1", n)
	}
}

func TestMalformedRequestsGet400(t *testing.T) {
	cases := []struct {
		name    string
		stream  uint32
		payload []byte
		explain string // the error body must identify the invalid field
	}{
		{"empty payload", 1, []byte{}, "empty"},
		{"path length overruns", 1, []byte{0x01, 0x00, 0x09, '/'}, "path length"},
		{"value length overruns", 1, []byte{0x01, 0x00, 0x01, '/', 0x01, 0x00, 0x04, 'h'}, "value length"},
		{"empty literal name", 1, []byte{0x01, 0x00, 0x01, '/', 0x00, 0x00, 0x00, 0x00, 0x00}, "literal name"},
		{"no leading slash", 1, requestPayload(t, message.MethodGET, "hello.txt"), "start with /"},
		{"dot segment", 1, requestPayload(t, message.MethodGET, "/sub/../hello.txt"), ".."},
		{"stream 0", 0, requestPayload(t, message.MethodGET, "/hello.txt"), "stream 0"},
	}
	srv, _ := start(t, nil)
	p := open(t, srv)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p.t = t
			p.send(frameBytes(t, frame.TypeRequest, frame.EndStream, tc.stream, tc.payload))
			r := p.readResponse(tc.stream)
			if r.Status != 400 || !strings.Contains(string(r.body), tc.explain) {
				t.Fatalf("got %d %q, want 400 mentioning %q", r.Status, r.body, tc.explain)
			}
			if r.header("content-type") != "text/plain; charset=utf-8" || r.header("content-length") != strconv.Itoa(len(r.body)) {
				t.Fatalf("error headers %+v", r.Fields)
			}
		})
	}
	p.t = t
	p.get(9, "/hello.txt") // still open after all of them
	if r := p.readResponse(9); r.Status != 200 {
		t.Fatalf("after the errors: status %d", r.Status)
	}
}

func TestUnknownMethodGets501WithAllow(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	p.send(frameBytes(t, frame.TypeRequest, frame.EndStream, 1, requestPayload(t, 0x02, "/hello.txt")))
	r := p.readResponse(1)
	if r.Status != 501 || r.header("allow") != "GET" {
		t.Fatalf("got %d allow=%q, want 501 allow=GET", r.Status, r.header("allow"))
	}
}

// The spec fixes the order of checks. The method is judged before the rest of
// the payload, because a later method may lay its payload out differently and
// must still get a 501 from a v1 server, not a 400.
func TestChecksRunInSpecOrder(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	cases := []struct {
		name    string
		stream  uint32
		payload []byte
		status  uint16
	}{
		{"stream 0 beats an unknown method", 0, requestPayload(t, 0x09, "/hello.txt"), 400},
		{"unknown method, payload not laid out like GET", 1, []byte{0x09, 0xFF, 0xFF, 0x42}, 501},
		{"unknown method, path breaking GET's rules", 2, requestPayload(t, 0x09, "no-slash"), 501},
		{"unknown method, missing file", 3, requestPayload(t, 0x09, "/missing"), 501},
		{"GET, malformed", 4, []byte{0x01, 0xFF, 0xFF, 0x42}, 400},
		{"GET, missing file", 5, requestPayload(t, message.MethodGET, "/missing"), 404},
	}
	for _, tc := range cases {
		p.send(frameBytes(t, frame.TypeRequest, frame.EndStream, tc.stream, tc.payload))
		if r := p.readResponse(tc.stream); r.Status != tc.status {
			t.Errorf("%s: %d, want %d", tc.name, r.Status, tc.status)
		}
	}
}

// A NUL cannot be part of a file name, and C's open() would stop at it and
// open a different file, so the protocol rules it out as malformed.
func TestNULInPathIs400(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	p.get(1, "/hello.txt\x00.html")
	if r := p.readResponse(1); r.Status != 400 || !strings.Contains(string(r.body), "NUL") {
		t.Fatalf("got %d %q, want 400 mentioning NUL", r.Status, r.body)
	}
}

// Skipping is not free, so a peer may not make the server skip forever (the
// HTTP/2 empty-frames flood, CVE-2019-9518). Up to 1,000 skippable frames in a
// row are fine; one more closes the connection.
func TestEndlessSkippableFramesAreCutOff(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	junk := frameBytes(t, frame.Type(0x77), 0, 1, nil)
	p.send(bytes.Repeat(junk, 1000))
	p.get(1, "/hello.txt")
	if r := p.readResponse(1); r.Status != 200 {
		t.Fatalf("after 1,000 skipped frames: %d", r.Status)
	}
	p.send(bytes.Repeat(junk, 1001)) // the count restarted at the REQUEST
	p.expectClosed()
}

// A client sending skippable frames must not be able to make the server write
// one log line per 8 bytes it sends.
func TestSkippedFramesDoNotFloodTheLog(t *testing.T) {
	var logged syncBuffer
	srv, _ := start(t, func(c *server.Config) { c.Logger = log.New(&logged, "", 0) })
	p := open(t, srv)
	p.send(bytes.Repeat(frameBytes(t, frame.Type(0x77), 0, 1, nil), 500))
	p.get(1, "/hello.txt")
	p.readResponse(1)
	p.conn.Close()
	for deadline := time.Now().Add(3 * time.Second); srv.LiveConns() > 0; {
		if time.Now().After(deadline) {
			t.Fatal("the connection never ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	text := logged.String()
	if n := strings.Count(text, "skipped a "); n > 10 {
		t.Fatalf("%d lines for skipped frames:\n%s", n, text)
	}
	if !strings.Contains(text, "skipped 500 frames") {
		t.Fatalf("no total for the skipped frames:\n%s", text)
	}
}

// The MUST-skip rule: frame types the server does not act on, including
// known ones that mean nothing to it, are consumed by Length and ignored.
func TestFramesTheServerDoesNotActOnAreSkipped(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	var batch []byte
	batch = append(batch, frameBytes(t, frame.Type(0x7F), 0, 1, bytes.Repeat([]byte{0xEE}, 1000))...)
	batch = append(batch, frameBytes(t, frame.Type(0x00), 0xFF, 0, nil)...)
	batch = append(batch, frameBytes(t, frame.TypeData, frame.EndStream, 1, []byte("ignored body"))...)
	batch = append(batch, frameBytes(t, frame.TypeResponse, frame.EndStream, 1, []byte{0x00, 0xC8})...)
	p.send(batch)
	p.get(2, "/hello.txt")
	r := p.readResponse(2) // the first thing back must be the answer to stream 2
	if r.Status != 200 {
		t.Fatalf("status %d", r.Status)
	}
}

func TestUnknownFlagsAndFieldsAreIgnored(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	payload := requestPayload(t, message.MethodGET, "/hello.txt", message.Field{Name: "x-later", Value: "1"})
	payload = append(payload, 0xC8, 0x00, 0x02, 'v', '2') // field index 200: reserved, skipped
	p.send(frameBytes(t, frame.TypeRequest, 0xFF, 1, payload))
	if r := p.readResponse(1); r.Status != 200 || string(r.body) != "hello, world\n" {
		t.Fatalf("all flags set: %d %q", r.Status, r.body)
	}
	// A REQUEST without END_STREAM is still answered at once (spec section 4).
	p.send(frameBytes(t, frame.TypeRequest, 0, 2, requestPayload(t, message.MethodGET, "/hello.txt")))
	if r := p.readResponse(2); r.Status != 200 {
		t.Fatalf("no END_STREAM: %d", r.Status)
	}
}

func TestDirectoriesServeTheirIndex(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	cases := []struct {
		path   string
		status uint16
		body   string
	}{
		{"/", 200, "<h1>home</h1>\n"},
		{"/sub", 200, "<p>sub</p>\n"},
		{"/sub/", 200, "<p>sub</p>\n"},
		{"//sub//", 200, "<p>sub</p>\n"},
		{"/noindex/", 404, ""},
		{"/hello.txt/", 404, ""},
		{"/hello.txt/x", 404, ""},
	}
	for i, tc := range cases {
		stream := uint32(i + 1)
		p.get(stream, tc.path)
		r := p.readResponse(stream)
		if r.Status != tc.status || (tc.status == 200 && string(r.body) != tc.body) {
			t.Errorf("%s: %d %q, want %d %q", tc.path, r.Status, r.body, tc.status, tc.body)
		}
	}
}

// An empty body is a RESPONSE carrying END_STREAM and no DATA at all: the
// next frame on the wire belongs to the next request.
func TestEmptyFileIsAResponseWithEndStream(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	p.get(1, "/empty.txt")
	p.get(2, "/hello.txt")
	head := p.readFrame()
	if head.Type != frame.TypeResponse || !head.Flags.Has(frame.EndStream) {
		t.Fatalf("got %v flags %#x, want RESPONSE with END_STREAM", head.Type, head.Flags)
	}
	parsed, _ := message.ParseResponse(head.Payload)
	if cl, _ := message.Get(parsed.Fields, "content-length"); cl != "0" || parsed.Status != 200 {
		t.Fatalf("status %d content-length %q", parsed.Status, cl)
	}
	if next := p.readFrame(); next.Type != frame.TypeResponse || next.Stream != 2 {
		t.Fatalf("next frame is %v on stream %d, want stream 2's RESPONSE", next.Type, next.Stream)
	}
}

func TestLargeFileSpansDataFrames(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	p.get(1, "/big.bin")
	r := p.readResponse(1)
	if !bytes.Equal(r.body, bigBody) {
		t.Fatalf("body differs: got %d bytes, want %d", len(r.body), len(bigBody))
	}
	if r.header("content-length") != "100000" {
		t.Fatalf("content-length %q", r.header("content-length"))
	}
	for i, f := range r.data {
		if len(f.Payload) > 16<<10 {
			t.Errorf("DATA %d carries %d bytes, more than bserve's 16 KiB", i, len(f.Payload))
		}
		if last := i == len(r.data)-1; f.Flags.Has(frame.EndStream) != last {
			t.Errorf("DATA %d of %d: END_STREAM = %v", i+1, len(r.data), !last)
		}
	}
}

func TestPipelinedRequestsAreAnsweredInOrder(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	var batch []byte
	for i, path := range []string{"/hello.txt", "/missing", "/index.html"} {
		batch = append(batch, frameBytes(t, frame.TypeRequest, frame.EndStream, uint32(10+i), requestPayload(t, message.MethodGET, path))...)
	}
	p.send(batch) // one write, three requests
	for i, want := range []uint16{200, 404, 200} {
		if r := p.readResponse(uint32(10 + i)); r.Status != want {
			t.Fatalf("response %d: %d, want %d", i, r.Status, want)
		}
	}
}

func TestServerSendsItsPrefaceWithoutWaiting(t *testing.T) {
	srv, _ := start(t, nil)
	p := connect(t, srv) // sends nothing
	p.expectPreface()
}

func TestWrongPrefaceClosesWithoutAnswer(t *testing.T) {
	srv, _ := start(t, nil)
	p := connect(t, srv)
	p.send([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	p.expectPreface() // sent before the server read anything
	p.expectClosed()
}

func TestNothingOutsideTheRootIsServed(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	p.get(1, "/leak.txt") // symlink to ../outside/secret.txt
	if r := p.readResponse(1); r.Status != 404 || bytes.Contains(r.body, []byte("secret")) {
		t.Fatalf("symlink out of the root: %d %q", r.Status, r.body)
	}
	p.get(2, "/alias.txt") // symlink that stays inside
	if r := p.readResponse(2); r.Status != 200 || string(r.body) != "hello, world\n" {
		t.Fatalf("symlink inside the root: %d %q", r.Status, r.body)
	}
}

func TestUnreadableFileIs403(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files whatever their mode")
	}
	srv, _ := start(t, nil)
	p := open(t, srv)
	p.get(1, "/locked.txt")
	if r := p.readResponse(1); r.Status != 403 {
		t.Fatalf("status %d, want 403", r.Status)
	}
}

// Opening a FIFO for reading blocks until a writer appears. The server must
// answer 404 at once instead of hanging the connection.
func TestFIFOIs404WithoutBlocking(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	p.get(1, "/pipe")
	if r := p.readResponse(1); r.Status != 404 {
		t.Fatalf("status %d, want 404", r.Status)
	}
}

// A client may send its requests and then half-close; complete requests
// already sent must still be answered before the server closes.
func TestHalfClosedClientStillGetsAnswers(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	p.get(1, "/hello.txt")
	p.get(2, "/index.html")
	if err := p.conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if r := p.readResponse(1); r.Status != 200 {
		t.Fatalf("stream 1: %d", r.Status)
	}
	if r := p.readResponse(2); r.Status != 200 {
		t.Fatalf("stream 2: %d", r.Status)
	}
	p.expectClosed()
}

func TestNewRejectsARootThatIsNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Join(t.TempDir(), "missing"), file} {
		cfg := server.DefaultConfig()
		cfg.Root = root
		if _, err := server.New(cfg); err == nil {
			t.Errorf("New accepted root %s", root)
		}
	}
}

// Paths come from the network, so they are logged quoted: a newline in a path
// must not let a client write a forged line into the server's log.
func TestPathsCannotForgeLogLines(t *testing.T) {
	var logged syncBuffer
	srv, _ := start(t, func(c *server.Config) { c.Logger = log.New(&logged, "", 0) })
	p := open(t, srv)
	p.get(1, "/x\n127.0.0.1:1 stream 9 GET /admin -> 200 (999 bytes)")
	p.readResponse(1)
	if strings.Contains(logged.String(), "\n127.0.0.1:1 stream 9") {
		t.Fatalf("a path started a new log line:\n%s", logged.String())
	}
}

// A frame bigger than one read is reassembled across reads: here a 60 KB path.
func TestFrameLargerThanOneRead(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	p.get(1, "/"+strings.Repeat("a", 60_000))
	if r := p.readResponse(1); r.Status != 404 {
		t.Fatalf("status %d, want 404", r.Status)
	}
	p.get(2, "/hello.txt")
	if r := p.readResponse(2); r.Status != 200 {
		t.Fatalf("after the big frame: %d", r.Status)
	}
}

// A client that half-closes in the middle of a frame has broken the stream:
// the server closes without answering the partial frame.
func TestHalfCloseMidFrameIsClosedUnanswered(t *testing.T) {
	srv, _ := start(t, nil)
	p := open(t, srv)
	whole := frameBytes(t, frame.TypeRequest, frame.EndStream, 1, requestPayload(t, message.MethodGET, "/hello.txt"))
	p.send(whole[:10])
	if err := p.conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	p.expectClosed()
}
