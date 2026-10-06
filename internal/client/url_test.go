package client_test

import (
	"testing"

	"binhttp/internal/client"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in   string
		want client.Target
	}{
		{"localhost:9000/index.html", client.Target{Addr: "localhost:9000", Host: "localhost:9000", Path: "/index.html"}},
		{"localhost/x", client.Target{Addr: "localhost:9000", Host: "localhost", Path: "/x"}},
		{"bh://127.0.0.1:8080", client.Target{Addr: "127.0.0.1:8080", Host: "127.0.0.1:8080", Path: "/"}},
		{"localhost:9000/a%20b.txt", client.Target{Addr: "localhost:9000", Host: "localhost:9000", Path: "/a b.txt"}},
		{"localhost:9000/x?q=1#frag", client.Target{Addr: "localhost:9000", Host: "localhost:9000", Path: "/x"}},
		{"[::1]:9001/x", client.Target{Addr: "[::1]:9001", Host: "[::1]:9001", Path: "/x"}},
		{"[::1]/x", client.Target{Addr: "[::1]:9000", Host: "[::1]", Path: "/x"}},
		// Dot segments are resolved before sending, as SPEC.md section 3 requires.
		{"h:1/a/./b/../c", client.Target{Addr: "h:1", Host: "h:1", Path: "/a/c"}},
		{"h:1/../../etc/passwd", client.Target{Addr: "h:1", Host: "h:1", Path: "/etc/passwd"}},
		{"h:1/%2e%2e/x", client.Target{Addr: "h:1", Host: "h:1", Path: "/x"}},
		{"h:1/a/..", client.Target{Addr: "h:1", Host: "h:1", Path: "/"}},
		{"h:1/a/.", client.Target{Addr: "h:1", Host: "h:1", Path: "/a/"}},
		// Everything else is sent as written, trailing slash included.
		{"h:1/sub/", client.Target{Addr: "h:1", Host: "h:1", Path: "/sub/"}},
		{"h:1/a//b", client.Target{Addr: "h:1", Host: "h:1", Path: "/a//b"}},
		{"h:1/...", client.Target{Addr: "h:1", Host: "h:1", Path: "/..."}},
	}
	for _, tt := range tests {
		got, err := client.ParseTarget(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ParseTarget(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
}

func TestParseTargetRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"http://localhost:9000/x", // another protocol's URL
		"localhost:0/x",
		"localhost:99999/x",
		"localhost:abc/x",
		"localhost:9000/%zz",
		"localhost:9000/index.html%00.txt", // SPEC.md section 3: no NUL in a path
		"/just/a/path",
	} {
		if got, err := client.ParseTarget(in); err == nil {
			t.Errorf("ParseTarget(%q) = %+v, want an error", in, got)
		}
	}
}
