// Package client is bcurl's half of BH/1: it sends requests over a single
// connection and checks every response against the spec.
package client

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"binhttp/internal/frame"
	"binhttp/internal/message"
)

// Options configures a connection.
type Options struct {
	Timeout   time.Duration // for connecting and for each read or write
	UserAgent string        // sent in the user-agent field
	Verbose   io.Writer     // if set, every frame sent and received is hexdumped here
}

// Conn is one BH/1 connection. It has no way to redial: a client that must
// never open a second connection is simplest with no reconnect code at all.
type Conn struct {
	conn  net.Conn
	opts  Options
	host  string // the authority sent in the host field
	next  uint32 // stream ID of the next request
	buf   []byte
	chunk []byte

	sentPreface bool
	gotPreface  bool
}

// Result describes one response.
type Result struct {
	Status uint16
	Fields []message.Field
	Bytes  int64 // body bytes written to the caller
}

// ProtocolError means the server broke the spec. The connection's state is
// then unknown, so the caller must stop using it.
type ProtocolError struct {
	Msg string
}

func (e *ProtocolError) Error() string { return "protocol error: " + e.Msg }

func violation(format string, args ...any) error {
	return &ProtocolError{Msg: fmt.Sprintf(format, args...)}
}

// Dial opens the connection. host is the authority to send in the host field.
//
// Fast fallback (RFC 6555) is off: with it, a name with both an IPv6 and an
// IPv4 address can race two TCP handshakes, and bcurl never opens a second
// connection. Addresses are tried one after another instead.
func Dial(addr, host string, opts Options) (*Conn, error) {
	d := net.Dialer{Timeout: opts.Timeout, FallbackDelay: -1}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if opts.Verbose != nil {
		fmt.Fprintf(opts.Verbose, "* connected to %s; every request will use this one connection\n", conn.RemoteAddr())
	}
	return &Conn{conn: conn, opts: opts, host: host, next: 1, chunk: make([]byte, 64<<10)}, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.conn.Close() }

// Get requests path on a fresh stream and copies the body to out as each DATA
// frame arrives. A 4xx or 5xx is a Result like any other; an error means the
// connection or the protocol failed and the Conn must not be reused.
func (c *Conn) Get(path string, out io.Writer) (Result, error) {
	stream := c.next
	if stream == 0 { // the counter wrapped past 0xFFFFFFFF
		return Result{}, errors.New("every stream ID has been used on this connection, and they never wrap")
	}
	c.next++
	if err := c.sendRequest(stream, path); err != nil {
		return Result{}, err
	}
	if !c.gotPreface {
		if err := c.readPreface(); err != nil {
			return Result{}, err
		}
	}
	return c.readResponse(stream, out)
}

// sendRequest writes the preface (the first time) and one REQUEST frame,
// together in a single write.
func (c *Conn) sendRequest(stream uint32, path string) error {
	payload, err := message.Request{
		Method: message.MethodGET,
		Path:   path,
		Fields: []message.Field{
			{Name: "host", Value: c.host},
			{Name: "user-agent", Value: c.opts.UserAgent},
			{Name: "accept", Value: "*/*"},
		},
	}.Append(nil)
	if err != nil {
		return err
	}
	req, err := frame.Append(nil, frame.TypeRequest, frame.EndStream, stream, payload)
	if err != nil {
		return fmt.Errorf("the request head does not fit in one frame: %w", err)
	}

	var batch []byte
	if !c.sentPreface {
		batch = append(batch, frame.Preface...)
		c.dumpPreface(">", []byte(frame.Preface))
	}
	c.dumpFrame(">", req)
	batch = append(batch, req...)

	_ = c.conn.SetWriteDeadline(time.Now().Add(c.opts.Timeout))
	if _, err := c.conn.Write(batch); err != nil {
		return err
	}
	c.sentPreface = true
	return nil
}

func (c *Conn) readPreface() error {
	for {
		n, err := frame.MatchPreface(c.buf)
		if err == nil {
			c.dumpPreface("<", c.buf[:n])
			c.buf = c.buf[n:]
			c.gotPreface = true
			return nil
		}
		if errors.Is(err, frame.ErrBadPreface) {
			return violation("the server's first bytes are %q, not the BH/1 preface: is it a BH/1 server?", head(c.buf, 16))
		}
		if err := c.fill(); err != nil {
			return fmt.Errorf("waiting for the server's preface: %w", err)
		}
	}
}

// readResponse reads frames until the response to stream ends, skipping
// every frame type it does not use.
func (c *Conn) readResponse(stream uint32, out io.Writer) (Result, error) {
	var res Result
	gotHead := false
	declared := int64(-1) // content-length, if the server sent one
	skipped := 0          // frames skipped since the last one bcurl used

	for {
		f, err := c.readFrame()
		if err != nil {
			if gotHead {
				return res, fmt.Errorf("the connection ended after %d body bytes without END_STREAM, so the body is incomplete: %w", res.Bytes, err)
			}
			return res, fmt.Errorf("the connection ended before the response arrived: %w", err)
		}

		switch f.Type {
		case frame.TypeResponse:
			if gotHead {
				return res, violation("a second RESPONSE arrived on stream %d", f.Stream)
			}
			if f.Stream != stream {
				return res, violation("a RESPONSE arrived on stream %d while waiting for stream %d", f.Stream, stream)
			}
			resp, err := message.ParseResponse(f.Payload)
			if err != nil {
				return res, violation("the RESPONSE payload is %v", err)
			}
			if resp.Status < 200 || resp.Status > 599 {
				return res, violation("status %d is not a final HTTP status code (200 to 599; v1 has no interim responses)", resp.Status)
			}
			res.Status, res.Fields, gotHead = resp.Status, resp.Fields, true
			if v, ok := message.Get(resp.Fields, "content-length"); ok {
				n, err := strconv.ParseUint(v, 10, 63)
				if err != nil {
					return res, violation("content-length %q is not a decimal number", v)
				}
				declared = int64(n)
			}

		case frame.TypeData:
			if !gotHead {
				return res, violation("a DATA frame arrived on stream %d before its RESPONSE", f.Stream)
			}
			if f.Stream != stream {
				return res, violation("a DATA frame arrived on stream %d while reading stream %d", f.Stream, stream)
			}
			if declared >= 0 && res.Bytes+int64(len(f.Payload)) > declared {
				return res, violation("the DATA frames carry more than the content-length of %d", declared)
			}
			if _, err := out.Write(f.Payload); err != nil {
				return res, err
			}
			res.Bytes += int64(len(f.Payload))

		default:
			// The MUST-skip rule: readFrame has consumed the frame whole.
			if skipped++; skipped > frame.MaxSkipped {
				return res, violation("the server sent %d frames in a row that bcurl skipped; giving up", skipped)
			}
			continue
		}
		skipped = 0

		if f.Flags.Has(frame.EndStream) {
			if declared >= 0 && res.Bytes != declared {
				return res, violation("content-length is %d but the body was %d bytes", declared, res.Bytes)
			}
			return res, nil
		}
	}
}

// readFrame returns the next whole frame, hexdumping it under -v.
func (c *Conn) readFrame() (frame.Frame, error) {
	for {
		f, n, err := frame.Parse(c.buf)
		if err == nil {
			c.dumpFrame("<", c.buf[:n])
			c.buf = c.buf[n:]
			return f, nil
		}
		if err := c.fill(); err != nil {
			if len(c.buf) > 0 && errors.Is(err, io.EOF) {
				return frame.Frame{}, fmt.Errorf("server closed the connection mid-frame (%d bytes of it received)", len(c.buf))
			}
			if errors.Is(err, io.EOF) {
				return frame.Frame{}, errors.New("server closed the connection")
			}
			return frame.Frame{}, err
		}
	}
}

// fill reads whatever has arrived, waiting at most the timeout.
func (c *Conn) fill() error {
	_ = c.conn.SetReadDeadline(time.Now().Add(c.opts.Timeout))
	n, err := c.conn.Read(c.chunk)
	c.buf = append(c.buf, c.chunk[:n]...)
	if n > 0 {
		return nil
	}
	return err
}

// head returns at most n bytes from the front of b.
func head(b []byte, n int) []byte {
	return b[:min(len(b), n)]
}
