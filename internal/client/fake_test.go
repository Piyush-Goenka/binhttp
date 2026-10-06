package client_test

import (
	"io"
	"net"
	"testing"
	"time"

	"binhttp/internal/frame"
	"binhttp/internal/message"
)

// script is what a fake server does on its one connection.
type script struct {
	preface string   // sent right after accepting ("BH/1" for a well-behaved server)
	replies [][]byte // one reply written after each complete frame received
	hangUp  bool     // close right after the last reply instead of waiting for the client
}

// fake is a scripted server for making the client face one misbehaviour at a time.
type fake struct {
	addr     string
	received chan []byte // every complete frame the client sent, as raw bytes
}

func startFake(t *testing.T, sc script) *fake {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fake{addr: ln.Addr().String(), received: make(chan []byte, 64)}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Write([]byte(sc.preface))

		var buf []byte
		chunk := make([]byte, 4096)
		fill := func() bool {
			n, err := c.Read(chunk)
			buf = append(buf, chunk[:n]...)
			return n > 0 || err == nil
		}
		for len(buf) < 4 {
			if !fill() {
				return
			}
		}
		buf = buf[4:] // the client's preface, checked by the tests that care
		for _, reply := range sc.replies {
			for {
				_, n, err := frame.Parse(buf)
				if err == nil {
					f.received <- append([]byte(nil), buf[:n]...)
					buf = buf[n:]
					break
				}
				if !fill() {
					return
				}
			}
			_, _ = c.Write(reply)
		}
		if !sc.hangUp {
			_, _ = io.Copy(io.Discard, c)
		}
	}()
	return f
}

// next returns the next frame the client sent to the fake.
func (f *fake) next(t *testing.T) []byte {
	t.Helper()
	select {
	case b := <-f.received:
		return b
	case <-time.After(3 * time.Second):
		t.Fatal("the fake server received no frame")
		return nil
	}
}

func mustFrame(t *testing.T, typ frame.Type, flags frame.Flags, stream uint32, payload []byte) []byte {
	t.Helper()
	b, err := frame.Append(nil, typ, flags, stream, payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func responseFrame(t *testing.T, stream uint32, flags frame.Flags, status uint16, fields ...message.Field) []byte {
	t.Helper()
	p, err := message.Response{Status: status, Fields: fields}.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	return mustFrame(t, frame.TypeResponse, flags, stream, p)
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
