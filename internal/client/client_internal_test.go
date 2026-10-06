package client

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"

	"binhttp/internal/frame"
	"binhttp/internal/message"
)

// Stream IDs never wrap (SPEC.md section 4): the last request on a connection
// goes out on 0xFFFFFFFF, and asking for another is an error rather than a
// request on stream 0. Reaching the end takes 2^32 requests, so the counter
// is set directly.
func TestStreamIDsNeverWrap(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		p, _ := message.Response{Status: 204}.Append(nil)
		resp, _ := frame.Append([]byte(frame.Preface), frame.TypeResponse, frame.EndStream, 0xFFFFFFFF, p)
		_, _ = c.Write(resp)
		_, _ = c.Read(make([]byte, 4096))
	}()

	c, err := Dial(ln.Addr().String(), "h", Options{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.next = 0xFFFFFFFF
	if res, err := c.Get("/", &bytes.Buffer{}); err != nil || res.Status != 204 {
		t.Fatalf("the last stream ID: %+v %v", res, err)
	}
	if _, err := c.Get("/", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "stream ID") {
		t.Fatalf("after the last stream ID: err = %v, want an error about stream IDs", err)
	}
}
