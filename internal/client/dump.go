package client

import (
	"encoding/hex"
	"fmt"
	"strings"

	"binhttp/internal/frame"
	"binhttp/internal/message"
)

// dumpPreface writes the preface under -v.
func (c *Conn) dumpPreface(dir string, raw []byte) {
	if c.opts.Verbose == nil {
		return
	}
	fmt.Fprintf(c.opts.Verbose, "%s preface %q\n", dir, raw)
	c.hexdump(dir, raw)
}

// dumpFrame writes one whole frame under -v: a line naming every header
// field, the decoded payload where there is one, and then every byte in
// `hexdump -C` layout, so the frame header is the first 8-byte group.
func (c *Conn) dumpFrame(dir string, raw []byte) {
	if c.opts.Verbose == nil {
		return
	}
	f, _, err := frame.Parse(raw)
	if err != nil {
		return // raw is always one whole frame
	}
	w := c.opts.Verbose
	fmt.Fprintf(w, "%s %v length=%d flags=%s stream=%d\n", dir, f.Type, f.Length, flagNames(f.Flags), f.Stream)
	switch f.Type {
	case frame.TypeRequest:
		if req, err := message.ParseRequest(f.Payload); err == nil {
			method := fmt.Sprintf("method 0x%02x", req.Method)
			if req.Method == message.MethodGET {
				method = "GET"
			}
			fmt.Fprintf(w, "%s   %s %s\n", dir, method, req.Path)
			writeFields(dir, req.Fields, c)
		}
	case frame.TypeResponse:
		if resp, err := message.ParseResponse(f.Payload); err == nil {
			fmt.Fprintf(w, "%s   status %d\n", dir, resp.Status)
			writeFields(dir, resp.Fields, c)
		}
	case frame.TypeData:
	default:
		fmt.Fprintf(w, "%s   (a type bcurl does not use: skipped)\n", dir)
	}
	c.hexdump(dir, raw)
}

func writeFields(dir string, fields []message.Field, c *Conn) {
	for _, f := range fields {
		fmt.Fprintf(c.opts.Verbose, "%s   %s: %s\n", dir, f.Name, f.Value)
	}
}

// hexdump writes raw in `hexdump -C` layout, each line prefixed with the
// direction: "> " for bytes sent, "< " for bytes received.
func (c *Conn) hexdump(dir string, raw []byte) {
	dump := strings.TrimSuffix(hex.Dump(raw), "\n")
	for line := range strings.SplitSeq(dump, "\n") {
		fmt.Fprintf(c.opts.Verbose, "%s %s\n", dir, line)
	}
}

func flagNames(f frame.Flags) string {
	if f.Has(frame.EndStream) {
		return fmt.Sprintf("0x%02x(END_STREAM)", uint8(f))
	}
	return fmt.Sprintf("0x%02x", uint8(f))
}
