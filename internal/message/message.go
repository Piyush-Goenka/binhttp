// Package message encodes and decodes the payloads of REQUEST and RESPONSE
// frames and their header fields (SPEC.md sections 3 and 5). Like package
// frame, it does no I/O.
package message

import (
	"encoding/binary"
	"strings"
)

// MethodGET is the only method v1 defines.
const MethodGET uint8 = 0x01

// Field is one header field.
type Field struct {
	Name  string
	Value string
}

// Request is a decoded REQUEST payload.
type Request struct {
	Method uint8
	Path   string
	Fields []Field
}

// Response is a decoded RESPONSE payload.
type Response struct {
	Status uint16
	Fields []Field
}

// MalformedError explains why a payload could not be decoded. The server
// sends Reason as the body of its 400 to identify the invalid field.
type MalformedError struct {
	Reason string
}

func (e *MalformedError) Error() string { return "malformed: " + e.Reason }

// Append appends the encoded request to dst: Method, Path Length, Path, then
// the header fields. It checks only that every length fits its 16-bit
// prefix; whether the path is acceptable is the receiver's call (CheckPath).
func (r Request) Append(dst []byte) ([]byte, error) {
	start := len(dst)
	dst = append(dst, r.Method)
	dst, err := appendString(dst, r.Path, "path")
	if err != nil {
		return dst[:start], err
	}
	if dst, err = appendFields(dst, r.Fields); err != nil {
		return dst[:start], err
	}
	return dst, nil
}

// Append appends the encoded response to dst: Status, then the header fields.
func (r Response) Append(dst []byte) ([]byte, error) {
	start := len(dst)
	dst = binary.BigEndian.AppendUint16(dst, r.Status)
	dst, err := appendFields(dst, r.Fields)
	if err != nil {
		return dst[:start], err
	}
	return dst, nil
}

// ParseRequest decodes a REQUEST payload. It reports structure only: an
// unknown method decodes fine (the server answers it with 501), and the path
// is not judged here (see CheckPath). Every failure is a *MalformedError.
func ParseRequest(p []byte) (Request, error) {
	if len(p) == 0 {
		return Request{}, &MalformedError{Reason: "the REQUEST payload is empty"}
	}
	method := p[0]
	path, rest, err := readString(p[1:], "path")
	if err != nil {
		return Request{}, err
	}
	fields, err := parseFields(rest)
	if err != nil {
		return Request{}, err
	}
	return Request{Method: method, Path: string(path), Fields: fields}, nil
}

// ParseResponse decodes a RESPONSE payload. Every failure is a
// *MalformedError.
func ParseResponse(p []byte) (Response, error) {
	if len(p) < 2 {
		return Response{}, &MalformedError{Reason: "the RESPONSE payload is shorter than its 2-byte status"}
	}
	fields, err := parseFields(p[2:])
	if err != nil {
		return Response{}, err
	}
	return Response{Status: binary.BigEndian.Uint16(p), Fields: fields}, nil
}

// CheckPath reports whether path obeys SPEC.md section 3: it starts with '/',
// holds no NUL byte, and has no "." or ".." segment. Rejecting dot segments
// outright, rather than resolving them, means the server never has to decide
// what "/a/../b" means. A NUL is refused because no file name contains one,
// and C's open() would stop at it and open a different file.
func CheckPath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return &MalformedError{Reason: "the path does not start with /"}
	}
	if strings.IndexByte(path, 0) >= 0 {
		return &MalformedError{Reason: "the path contains a NUL byte"}
	}
	for seg := range strings.SplitSeq(path[1:], "/") {
		if seg == "." || seg == ".." {
			return &MalformedError{Reason: "the path has a . or .. segment; clients resolve those before sending"}
		}
	}
	return nil
}
