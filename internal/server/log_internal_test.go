package server

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// Anything a client controls can reach the log: paths, and errors that quote
// file names built from paths. logf escapes control bytes in every line, so
// no call site can let a client start a forged line.
func TestLogEscapesControlBytes(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{cfg: Config{Logger: log.New(&buf, "", 0)}}
	s.logf("opening %s: %v", "/x\n127.0.0.1:1 stream 9 GET \"/admin\" -> 200 (999 bytes)", "too many open files")
	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, `\x0a`) {
		t.Fatalf("log output %q, want one line with the newline escaped", out)
	}
}
