package message_test

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"binhttp/internal/message"
)

// Expected bytes are written out by hand from SPEC.md sections 3 and 5.

func TestRequestAppendEncodesSpecLayout(t *testing.T) {
	req := message.Request{
		Method: message.MethodGET,
		Path:   "/a",
		Fields: []message.Field{{Name: "host", Value: "h"}, {Name: "x-custom", Value: "v"}},
	}
	got, err := req.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x01,                 // Method GET
		0x00, 0x02, '/', 'a', // Path Length 2, Path
		0x01, 0x00, 0x01, 'h', // index 1 (host), Value Length 1, Value
		0x00, 0x00, 0x08, 'x', '-', 'c', 'u', 's', 't', 'o', 'm', // index 0, Name Length 8, Name
		0x00, 0x01, 'v', // Value Length 1, Value
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got  % x\nwant % x", got, want)
	}
}

// Every name in the table goes out as its one-byte index, at the number the
// spec assigns it. A table shuffled by one place breaks interop with every
// other implementation, so each index is pinned to the spec's literal.
func TestStaticTableIndexes(t *testing.T) {
	spec := []string{1: "host", 2: "user-agent", 3: "accept", 4: "server", 5: "date",
		6: "content-type", 7: "content-length", 8: "last-modified", 9: "etag", 10: "allow"}
	for idx := 1; idx <= 10; idx++ {
		resp := message.Response{Status: 200, Fields: []message.Field{{Name: spec[idx], Value: ""}}}
		got, err := resp.Append(nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []byte{0x00, 0xC8, byte(idx), 0x00, 0x00}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: got % x, want % x", spec[idx], got, want)
		}
	}
}

func TestResponseAppendEncodesSpecLayout(t *testing.T) {
	resp := message.Response{Status: 404, Fields: []message.Field{{Name: "content-length", Value: "0"}}}
	got, err := resp.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x01, 0x94, 0x07, 0x00, 0x01, '0'} // 404 = 0x0194; index 7; "0"
	if !bytes.Equal(got, want) {
		t.Fatalf("got  % x\nwant % x", got, want)
	}
}

func TestParseRequestDecodesHandWrittenPayload(t *testing.T) {
	payload := []byte{
		0x01, 0x00, 0x0b, '/', 'i', 'n', 'd', 'e', 'x', '.', 'h', 't', 'm', 'l',
		0x02, 0x00, 0x03, 'b', 'c', '/',
		0x00, 0x00, 0x03, 'x', '-', 'a', 0x00, 0x00, // literal name, empty value
	}
	got, err := message.ParseRequest(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := message.Request{
		Method: 0x01,
		Path:   "/index.html",
		Fields: []message.Field{{Name: "user-agent", Value: "bc/"}, {Name: "x-a", Value: ""}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// The method byte is decoded, not judged: deciding that 0x07 is unknown is
// the server's 501, not a malformed payload.
func TestParseRequestKeepsUnknownMethod(t *testing.T) {
	got, err := message.ParseRequest([]byte{0x07, 0x00, 0x01, '/'})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != 0x07 || got.Path != "/" || len(got.Fields) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestParseResponseDecodesHandWrittenPayload(t *testing.T) {
	payload := []byte{0x01, 0xF5, 0x0A, 0x00, 0x03, 'G', 'E', 'T'} // 501, allow: GET
	got, err := message.ParseResponse(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := message.Response{Status: 501, Fields: []message.Field{{Name: "allow", Value: "GET"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// Index 11 to 255 is reserved: the field is skipped by its Value Length and
// the fields around it still decode.
func TestParseSkipsReservedIndexes(t *testing.T) {
	payload := []byte{
		0x00, 0xC8,
		0x0B, 0x00, 0x02, 'h', 'i', // index 11
		0x06, 0x00, 0x01, 'x', // index 6 content-type
		0xFF, 0x00, 0x00, // index 255, empty value
		0x04, 0x00, 0x01, 'y', // index 4 server
	}
	got, err := message.ParseResponse(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := []message.Field{{Name: "content-type", Value: "x"}, {Name: "server", Value: "y"}}
	if !reflect.DeepEqual(got.Fields, want) {
		t.Fatalf("fields %+v, want %+v", got.Fields, want)
	}
}

func TestParseRequestRejectsMalformed(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		reason  string // a word the explanation must contain
	}{
		{"empty payload", []byte{}, "empty"},
		{"no path length", []byte{0x01}, "path"},
		{"half a path length", []byte{0x01, 0x00}, "path"},
		{"path overruns", []byte{0x01, 0x00, 0x05, '/', 'a'}, "path"},
		{"index without value length", []byte{0x01, 0x00, 0x01, '/', 0x01}, "value"},
		{"value overruns", []byte{0x01, 0x00, 0x01, '/', 0x01, 0x00, 0x05, 'h'}, "value"},
		{"literal name length truncated", []byte{0x01, 0x00, 0x01, '/', 0x00, 0x00}, "name"},
		{"literal name overruns", []byte{0x01, 0x00, 0x01, '/', 0x00, 0x00, 0x09, 'a'}, "name"},
		{"empty literal name", []byte{0x01, 0x00, 0x01, '/', 0x00, 0x00, 0x00, 0x00, 0x00}, "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := message.ParseRequest(tt.payload)
			var me *message.MalformedError
			if !errors.As(err, &me) {
				t.Fatalf("err = %v, want *MalformedError", err)
			}
			if !strings.Contains(me.Reason, tt.reason) {
				t.Fatalf("reason %q does not mention %q", me.Reason, tt.reason)
			}
		})
	}
}

func TestParseResponseRejectsMalformed(t *testing.T) {
	for _, payload := range [][]byte{
		{},
		{0x00},
		{0x00, 0xC8, 0x07, 0x00},
		{0x00, 0xC8, 0x07, 0x00, 0x02, '1'},
	} {
		var me *message.MalformedError
		if _, err := message.ParseResponse(payload); !errors.As(err, &me) {
			t.Errorf("% x: err = %v, want *MalformedError", payload, err)
		}
	}
}

func TestAppendRejectsWhatCannotBeEncoded(t *testing.T) {
	long := strings.Repeat("a", 65536)
	cases := map[string]message.Request{
		"empty name":   {Method: 1, Path: "/", Fields: []message.Field{{Name: "", Value: "v"}}},
		"long value":   {Method: 1, Path: "/", Fields: []message.Field{{Name: "host", Value: long}}},
		"long literal": {Method: 1, Path: "/", Fields: []message.Field{{Name: long, Value: ""}}},
		"long path":    {Method: 1, Path: "/" + long},
	}
	for name, req := range cases {
		if _, err := req.Append(nil); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestCheckPath(t *testing.T) {
	ok := []string{"/", "/index.html", "/a/b/c.txt", "/a//b", "/dir/", "/.hidden", "/a/..b", "/...", "/a b"}
	bad := []string{"", "index.html", "/.", "/./a", "/..", "/a/../b", "/a/.", "/a/..", "//..", "/a\x00b", "/\x00"}
	for _, p := range ok {
		if err := message.CheckPath(p); err != nil {
			t.Errorf("CheckPath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range bad {
		var me *message.MalformedError
		if err := message.CheckPath(p); !errors.As(err, &me) {
			t.Errorf("CheckPath(%q) = %v, want *MalformedError", p, err)
		}
	}
}

func TestGetUsesTheFirstOfRepeatedNames(t *testing.T) {
	fields := []message.Field{{Name: "etag", Value: "a"}, {Name: "content-length", Value: "5"}, {Name: "content-length", Value: "9"}}
	if v, ok := message.Get(fields, "content-length"); !ok || v != "5" {
		t.Fatalf("Get = %q, %v; want \"5\", true", v, ok)
	}
	if _, ok := message.Get(fields, "date"); ok {
		t.Fatal("Get found a field that is not there")
	}
}
