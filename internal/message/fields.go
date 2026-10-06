package message

import (
	"encoding/binary"
	"fmt"
)

// table holds the names that travel as a one-byte index (SPEC.md section 5):
// exactly the ten names bcurl and bserve send. Index 0 is not a name; it
// introduces a literal one.
var table = [...]string{
	1:  "host",
	2:  "user-agent",
	3:  "accept",
	4:  "server",
	5:  "date",
	6:  "content-type",
	7:  "content-length",
	8:  "last-modified",
	9:  "etag",
	10: "allow",
}

// indexOf maps a name to its table index, built once from table so the two
// cannot disagree.
var indexOf = func() map[string]byte {
	m := make(map[string]byte, len(table))
	for i, name := range table {
		if name != "" {
			m[name] = byte(i)
		}
	}
	return m
}()

// maxString is the most a 16-bit length prefix can describe.
const maxString = 1<<16 - 1

// appendFields encodes fields: a known name as its index, any other as index
// 0 followed by the literal name, then the length-prefixed value.
func appendFields(dst []byte, fields []Field) ([]byte, error) {
	for _, f := range fields {
		if idx, ok := indexOf[f.Name]; ok {
			dst = append(dst, idx)
		} else {
			if f.Name == "" {
				return dst, fmt.Errorf("message: a field name cannot be empty")
			}
			var err error
			dst = append(dst, 0)
			if dst, err = appendString(dst, f.Name, "field name"); err != nil {
				return dst, err
			}
		}
		var err error
		if dst, err = appendString(dst, f.Value, "value of "+f.Name); err != nil {
			return dst, err
		}
	}
	return dst, nil
}

// appendString appends a 16-bit length and then s.
func appendString(dst []byte, s, what string) ([]byte, error) {
	if len(s) > maxString {
		return dst, fmt.Errorf("message: %s is %d bytes, more than a 16-bit length can describe", what, len(s))
	}
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(s)))
	return append(dst, s...), nil
}

// parseFields decodes header fields until p is exhausted. A field whose index
// is beyond the table is skipped by its value length, as the spec requires,
// so a later version can grow the table without breaking this one.
func parseFields(p []byte) ([]Field, error) {
	var fields []Field
	for len(p) > 0 {
		idx := p[0]
		p = p[1:]

		var name string
		switch {
		case idx == 0:
			b, rest, err := readString(p, "literal name")
			if err != nil {
				return nil, err
			}
			if len(b) == 0 {
				return nil, &MalformedError{Reason: "a literal name is empty"}
			}
			name, p = string(b), rest
		case int(idx) < len(table):
			name = table[idx]
		}

		value, rest, err := readString(p, "value")
		if err != nil {
			return nil, err
		}
		p = rest
		if name != "" {
			fields = append(fields, Field{Name: name, Value: string(value)})
		}
	}
	return fields, nil
}

// readString reads a 16-bit length and that many bytes from the front of p.
func readString(p []byte, what string) (s, rest []byte, err error) {
	if len(p) < 2 {
		return nil, nil, &MalformedError{Reason: what + " length is cut off by the end of the payload"}
	}
	n := int(binary.BigEndian.Uint16(p))
	p = p[2:]
	if n > len(p) {
		return nil, nil, &MalformedError{
			Reason: fmt.Sprintf("%s length %d runs %d bytes past the end of the payload", what, n, n-len(p)),
		}
	}
	return p[:n], p[n:], nil
}

// Get returns the value of the first field called name. The spec says a
// receiver uses the first when a name repeats.
func Get(fields []Field, name string) (string, bool) {
	for _, f := range fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return "", false
}
