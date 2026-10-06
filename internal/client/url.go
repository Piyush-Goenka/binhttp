package client

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// DefaultPort is the port bcurl assumes when a URL names none. BH/1 has no
// registered port; 9000 matches the course's examples.
const DefaultPort = "9000"

// Target is what bcurl makes of one URL.
type Target struct {
	Addr string // host:port to dial
	Host string // the authority as written, sent in the host field
	Path string // the path as SPEC.md section 3 wants it on the wire
}

// ParseTarget turns "localhost:9000/index.html" (an optional "bh://" in
// front) into a Target. The path is percent-decoded, because BH/1 carries the
// bytes of a file path rather than a URL, and its dot segments are resolved
// here, because a server must reject any that remain. The query and fragment
// are dropped: v1 has nowhere to put them.
func ParseTarget(raw string) (Target, error) {
	s := raw
	if !strings.Contains(s, "://") {
		s = "bh://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return Target{}, fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if u.Scheme != "bh" {
		return Target{}, fmt.Errorf("%q is a %s URL; bcurl speaks BH/1 (bh://)", raw, u.Scheme)
	}
	if u.Hostname() == "" {
		return Target{}, fmt.Errorf("%q names no host", raw)
	}
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return Target{}, fmt.Errorf("%q has an empty port", raw)
		}
		port = DefaultPort
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return Target{}, errors.New("the port in " + strconv.Quote(raw) + " is not between 1 and 65535")
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	if strings.IndexByte(path, 0) >= 0 {
		return Target{}, fmt.Errorf("%q decodes to a path with a NUL byte, which no file name has", raw)
	}
	return Target{
		Addr: net.JoinHostPort(u.Hostname(), port),
		Host: u.Host,
		Path: resolveDots(path),
	}, nil
}

// resolveDots removes "." and ".." segments as RFC 3986 section 5.2.4 does,
// leaving everything else as written, a trailing slash included. A ".." at
// the root stays at the root.
func resolveDots(p string) string {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	out := make([]string, 0, len(segs))
	for i, seg := range segs {
		last := i == len(segs)-1
		switch seg {
		case ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
			continue
		}
		if last {
			out = append(out, "") // "/a/." and "/a/b/.." both name a directory
		}
	}
	return "/" + strings.Join(out, "/")
}
