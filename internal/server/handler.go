package server

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"binhttp/internal/frame"
	"binhttp/internal/message"
)

const (
	serverName = "bserve/1.0"
	imfFixdate = "Mon, 02 Jan 2006 15:04:05 GMT" // RFC 9110 section 5.6.7
)

// reply is everything the server will send for one request.
type reply struct {
	status int
	fields []message.Field
	body   []byte   // a small body held in memory (the error explanations)
	file   *os.File // or a file streamed from disk; the caller closes it
	size   int64    // body size in bytes, in either case
}

// handle decides the reply to one REQUEST frame, checking in the order
// SPEC.md section 6 fixes: stream 0 or no method byte (400), then the method
// (501), then the rest of a GET (400), then the file (404, 403, 500). It also
// returns a short description for the log.
//
// The method is judged before the rest of the payload on purpose. A later
// version may add a method whose payload is laid out differently; this
// server cannot parse that payload, but it can still answer 501 rather than
// call it malformed.
func (s *Server) handle(f frame.Frame) (reply, string) {
	if f.Stream == 0 {
		return s.errorReply(400, "stream 0 is reserved for the connection itself; give each request a nonzero stream ID"), "request on stream 0"
	}
	if len(f.Payload) == 0 {
		return s.errorReply(400, "the REQUEST payload is empty"), "empty request"
	}
	if method := f.Payload[0]; method != message.MethodGET {
		r := s.errorReply(501, fmt.Sprintf("method 0x%02x is not implemented; v1 defines only GET (0x01)", method))
		r.fields = append(r.fields, message.Field{Name: "allow", Value: "GET"})
		return r, fmt.Sprintf("method 0x%02x", method)
	}

	req, err := message.ParseRequest(f.Payload)
	if err == nil {
		err = message.CheckPath(req.Path)
	}
	if err != nil {
		reason := err.Error()
		var me *message.MalformedError
		if errors.As(err, &me) {
			reason = me.Reason
		}
		return s.errorReply(400, reason), "malformed GET"
	}
	// The path came off the network, so it is quoted: a newline in it must not
	// be able to start a forged line in the log.
	return s.serveFile(req.Path), fmt.Sprintf("GET %q", req.Path)
}

// serveFile maps a checked path to a file under the root.
func (s *Server) serveFile(p string) reply {
	// CheckPath has already rejected "." and "..", so the only tidying left is
	// folding repeated slashes. A trailing slash is kept: "/hello.txt/" names
	// a directory, and opening a file as one fails, as it should.
	name := strings.TrimLeft(p, "/")
	for strings.Contains(name, "//") {
		name = strings.ReplaceAll(name, "//", "/")
	}
	if name == "" {
		name = "."
	}

	f, fi, err := s.open(name)
	if err != nil {
		return s.fileError(p, err)
	}
	if fi.IsDir() {
		_ = f.Close()
		if f, fi, err = s.open(path.Join(name, "index.html")); err != nil {
			return s.fileError(p, err)
		}
	}
	if !fi.Mode().IsRegular() {
		// A FIFO, socket or device, or an "index.html" that is a directory.
		_ = f.Close()
		return s.errorReply(404, "nothing to serve at "+p)
	}

	modified := fi.ModTime().UTC()
	return reply{
		status: 200,
		fields: []message.Field{
			{Name: "server", Value: serverName},
			{Name: "date", Value: time.Now().UTC().Format(imfFixdate)},
			{Name: "content-type", Value: contentType(fi.Name())},
			{Name: "content-length", Value: strconv.FormatInt(fi.Size(), 10)},
			{Name: "last-modified", Value: modified.Format(imfFixdate)},
			{Name: "etag", Value: fmt.Sprintf(`"%x-%x"`, modified.Unix(), fi.Size())},
		},
		file: f,
		size: fi.Size(),
	}
}

// open opens name under the root without ever blocking. O_NONBLOCK makes
// opening a FIFO return at once instead of waiting for a writer, and has no
// effect on reading a regular file. Type and size come from the open file
// itself, so they describe exactly the bytes that will be sent.
func (s *Server) open(name string) (*os.File, fs.FileInfo, error) {
	f, err := s.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}

// fileError turns a failure to open a path into a status.
func (s *Server) fileError(p string, err error) reply {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return s.errorReply(403, "cannot read "+p)
	case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE),
		errors.Is(err, syscall.EIO), errors.Is(err, syscall.ENOMEM):
		s.logf("opening %s: %v", p, err)
		return s.errorReply(500, "the server could not open "+p)
	default:
		// Missing, a file where a directory was expected, a name too long, a
		// symbolic link loop, or a link out of the root: in every case there
		// is nothing under the root to serve.
		return s.errorReply(404, "nothing to serve at "+p)
	}
}

// errorReply builds a reply whose text/plain body says what went wrong, so
// someone debugging a client learns which byte the server objected to.
func (s *Server) errorReply(status int, detail string) reply {
	body := []byte(fmt.Sprintf("%d %s: %s\n", status, statusText[status], detail))
	return reply{
		status: status,
		fields: []message.Field{
			{Name: "server", Value: serverName},
			{Name: "date", Value: time.Now().UTC().Format(imfFixdate)},
			{Name: "content-type", Value: "text/plain; charset=utf-8"},
			{Name: "content-length", Value: strconv.Itoa(len(body))},
		},
		body: body,
		size: int64(len(body)),
	}
}

var statusText = map[int]string{
	400: "Bad Request",
	403: "Forbidden",
	404: "Not Found",
	500: "Internal Server Error",
	501: "Not Implemented",
}

// contentTypes is deliberately a fixed table rather than the mime package,
// whose answers depend on the machine's mime.types file.
var contentTypes = map[string]string{
	".css":  "text/css; charset=utf-8",
	".gif":  "image/gif",
	".htm":  "text/html; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".ico":  "image/x-icon",
	".jpeg": "image/jpeg",
	".jpg":  "image/jpeg",
	".js":   "text/javascript; charset=utf-8",
	".json": "application/json",
	".md":   "text/markdown; charset=utf-8",
	".pdf":  "application/pdf",
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".txt":  "text/plain; charset=utf-8",
	".wasm": "application/wasm",
	".webp": "image/webp",
	".xml":  "application/xml",
}

func contentType(name string) string {
	if t, ok := contentTypes[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}
