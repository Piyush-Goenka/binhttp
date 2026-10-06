package binhttp_test

import (
	"bytes"
	"encoding/hex"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"binhttp/internal/frame"
	"binhttp/internal/message"
)

// Decode the bytes in HEXDUMP.md and verify their annotations. The raw
// `bcurl -v` dump must contain the same bytes as the annotated sections.

func TestHexdumpAnnotationsMatchTheBytes(t *testing.T) {
	doc := readFile(t, "HEXDUMP.md")

	// Client to server: the preface, then exactly one REQUEST.
	rest := afterPreface(t, annotated(t, doc, "client"))
	req, n, err := frame.Parse(rest)
	if err != nil || n != len(rest) {
		t.Fatalf("client bytes are not one whole frame: n=%d of %d, %v", n, len(rest), err)
	}
	if req.Header != (frame.Header{Length: 49, Type: frame.TypeRequest, Flags: frame.EndStream, Stream: 1}) {
		t.Fatalf("REQUEST header %+v", req.Header)
	}
	gotReq, err := message.ParseRequest(req.Payload)
	if err != nil {
		t.Fatal(err)
	}
	wantReq := message.Request{Method: message.MethodGET, Path: "/index.html", Fields: []message.Field{
		{Name: "host", Value: "localhost:9000"},
		{Name: "user-agent", Value: "bcurl/1.0"},
		{Name: "accept", Value: "*/*"},
	}}
	if !reflect.DeepEqual(gotReq, wantReq) {
		t.Fatalf("REQUEST payload\n got %+v\nwant %+v", gotReq, wantReq)
	}

	// Server to client: the preface, a RESPONSE, then one DATA frame.
	rest = afterPreface(t, annotated(t, doc, "server"))
	head, n, err := frame.Parse(rest)
	if err != nil {
		t.Fatal(err)
	}
	rest = rest[n:]
	if head.Header != (frame.Header{Length: 127, Type: frame.TypeResponse, Flags: 0, Stream: 1}) {
		t.Fatalf("RESPONSE header %+v", head.Header)
	}
	gotResp, err := message.ParseResponse(head.Payload)
	if err != nil {
		t.Fatal(err)
	}
	wantResp := message.Response{Status: 200, Fields: []message.Field{
		{Name: "server", Value: "bserve/1.0"},
		{Name: "date", Value: "Sun, 04 Oct 2026 23:05:10 GMT"},
		{Name: "content-type", Value: "text/html; charset=utf-8"},
		{Name: "content-length", Value: "62"},
		{Name: "last-modified", Value: "Sun, 04 Oct 2026 23:03:50 GMT"},
		{Name: "etag", Value: `"6ac2db56-3e"`},
	}}
	if !reflect.DeepEqual(gotResp, wantResp) {
		t.Fatalf("RESPONSE payload\n got %+v\nwant %+v", gotResp, wantResp)
	}
	data, n, err := frame.Parse(rest)
	if err != nil || n != len(rest) {
		t.Fatalf("the rest is not one whole DATA frame: n=%d of %d, %v", n, len(rest), err)
	}
	if data.Header != (frame.Header{Length: 62, Type: frame.TypeData, Flags: frame.EndStream, Stream: 1}) {
		t.Fatalf("DATA header %+v", data.Header)
	}
	if page := readFile(t, "www/index.html"); string(data.Payload) != page {
		t.Fatalf("the DATA payload is not www/index.html as it is now; recapture HEXDUMP.md")
	}
}

func TestHexdumpRawDumpMatchesTheAnnotations(t *testing.T) {
	doc := readFile(t, "HEXDUMP.md")
	sent, received := rawDump(t, doc)
	if client := annotated(t, doc, "client"); !bytes.Equal(sent, client) {
		t.Errorf("the raw dump's sent bytes differ from section 2:\n% x\n% x", sent, client)
	}
	if server := annotated(t, doc, "server"); !bytes.Equal(received, server) {
		t.Errorf("the raw dump's received bytes differ from section 3:\n% x\n% x", received, server)
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func afterPreface(t *testing.T, b []byte) []byte {
	t.Helper()
	n, err := frame.MatchPreface(b)
	if err != nil {
		t.Fatalf("does not start with the preface: % x", b[:min(len(b), 8)])
	}
	return b[n:]
}

// fenced returns the body of the code block whose opening line is opener.
func fenced(t *testing.T, doc, opener string) []string {
	t.Helper()
	lines := strings.Split(doc, "\n")
	for i, l := range lines {
		if l != opener {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == "```" {
				return lines[i+1 : j]
			}
		}
	}
	t.Fatalf("no %q block in HEXDUMP.md", opener)
	return nil
}

// annotated returns the bytes of a field-by-field block: the hex pairs that
// begin each line, ahead of the annotation.
func annotated(t *testing.T, doc, dir string) []byte {
	t.Helper()
	pairs := regexp.MustCompile(`^\s*((?:[0-9a-f]{2} )*[0-9a-f]{2})(?:\s|$)`)
	var out []byte
	for _, line := range fenced(t, doc, "```hex "+dir) {
		m := pairs.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("a %s line holds no bytes: %q", dir, line)
		}
		b, err := hex.DecodeString(strings.ReplaceAll(m[1], " ", ""))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	return out
}

// rawDump returns the bytes in the `bcurl -v` block, sent and received.
func rawDump(t *testing.T, doc string) (sent, received []byte) {
	t.Helper()
	line := regexp.MustCompile(`^([<>]) [0-9a-f]{8}  (.*?)\s*\|`)
	for _, l := range fenced(t, doc, "```text") {
		m := line.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		b, err := hex.DecodeString(strings.ReplaceAll(m[2], " ", ""))
		if err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		if m[1] == ">" {
			sent = append(sent, b...)
		} else {
			received = append(received, b...)
		}
	}
	return sent, received
}

// Decode the two byte-level examples in SPEC.md and check their contents.
func TestSpecExamplesDecodeAsDescribed(t *testing.T) {
	spec := readFile(t, "SPEC.md")
	examples := []struct {
		inSpec string
		bytes  []byte
	}{
		{"`00 09 01 01 00 00 00 01` `01 00 02 2F 61 01 00 01 68`",
			[]byte{0x00, 0x09, 0x01, 0x01, 0x00, 0x00, 0x00, 0x01, 0x01, 0x00, 0x02, 0x2F, 0x61, 0x01, 0x00, 0x01, 0x68}},
		{"`00 02 02 01 00 00 00 01` `00 C8`",
			[]byte{0x00, 0x02, 0x02, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0xC8}},
	}
	for _, ex := range examples {
		if !strings.Contains(strings.Join(strings.Fields(spec), " "), ex.inSpec) {
			t.Fatalf("SPEC.md no longer shows %s", ex.inSpec)
		}
	}

	// "GET /a with host: h on stream 1"
	get, n, err := frame.Parse(examples[0].bytes)
	if err != nil || n != len(examples[0].bytes) {
		t.Fatalf("first example is not one whole frame: %v", err)
	}
	req, err := message.ParseRequest(get.Payload)
	if err != nil || get.Type != frame.TypeRequest || get.Stream != 1 || !get.Flags.Has(frame.EndStream) ||
		!reflect.DeepEqual(req, message.Request{Method: message.MethodGET, Path: "/a", Fields: []message.Field{{Name: "host", Value: "h"}}}) {
		t.Fatalf("first example decodes to %+v %+v %v", get.Header, req, err)
	}

	// "a 200 with no fields and no body"
	ok, n, err := frame.Parse(examples[1].bytes)
	if err != nil || n != len(examples[1].bytes) {
		t.Fatalf("second example is not one whole frame: %v", err)
	}
	resp, err := message.ParseResponse(ok.Payload)
	if err != nil || ok.Type != frame.TypeResponse || ok.Stream != 1 || !ok.Flags.Has(frame.EndStream) ||
		resp.Status != 200 || len(resp.Fields) != 0 {
		t.Fatalf("second example decodes to %+v %+v %v", ok.Header, resp, err)
	}
}
