# binhttp: HTTP in binary

A binary file-transfer protocol, BH/1, with a client and server written in Go
using raw TCP sockets. Uses the standard library and requires Go 1.24 or later.

| Submission file | Location |
| --- | --- |
| The spec, two pages | [`SPEC.md`](SPEC.md): two A4 pages at 10pt (`scripts/spec-pdf.sh` renders it and counts the pages) |
| The program | `cmd/bserve`, `cmd/bcurl`, `internal/` |
| Annotated hexdump of one request and response | [`HEXDUMP.md`](HEXDUMP.md) |

## Use

```
go build -o bserve ./cmd/bserve && go build -o bcurl ./cmd/bcurl

./bserve ./www 9000                       # serve ./www on port 9000
./bcurl -v localhost:9000/index.html      # body to stdout, every frame hexdumped to stderr
./bcurl localhost:9000/index.html localhost:9000/about.txt   # two requests, one connection
```

### bserve

`bserve [flags] ROOT PORT` keeps every connection open across requests. A
missing file is a `404` and a malformed frame a `400`, and neither closes the
connection. It serves every regular file under ROOT, dotfiles included, and
nothing outside it: a symbolic link that stays inside the root works, one that
leaves it is a `404`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-bind` | every interface | address to listen on |
| `-idle-timeout` | 15s | close a connection that has sent no request for this long |
| `-frame-timeout` | 5s | maximum time for an incomplete frame to arrive; must be at least 5s |
| `-write-timeout` | 10s | drop a client that stops reading for this long (`0` turns it off) |
| `-max-conns` | 1024 | serve at most this many connections; more wait their turn (`0`: no cap) |
| `-quiet` | off | no connection or request log |

Ctrl-C stops accepting, closes idle connections at once, and gives busy ones
up to 5 seconds to finish their current response.

### bcurl

`bcurl [-v] [-timeout 10s] host[:port]/path ...` fetches every URL over one
connection; `-timeout` (at least 5s) bounds connecting and each wait. URLs must
name the same server. The port defaults to 9000, and a `bh://` prefix is optional. The
path is percent-decoded, its query and fragment are dropped, and its dot
segments are resolved before sending, as the spec requires. Bodies go to stdout
in order. With `-v`, each frame is decoded and hexdumped to stderr, `>` for
sent and `<` for received.

| Exit status | Meaning |
| --- | --- |
| 0 | every response was 2xx or 3xx |
| 4 | the worst response was a 4xx |
| 5 | the worst response was a 5xx |
| 1 | the connection or the protocol failed |
| 2 | usage error |

## Protocol design

Every frame starts with an 8-byte header: Length (16 bits), Type (8 bits),
Flags (8 bits) and Stream ID (32 bits). The length field identifies the next
frame even when a request payload is malformed, so the server can return 400
and continue serving the connection. Unknown frame types and reserved header
indexes are skipped by length, allowing later versions to add optional
extensions. `SPEC.md` explains the field widths and compares them with
HTTP/2's 24/8/8/31.

## Test

```
go test ./... -race          # unit, integration, fuzz seeds and doc tests
scripts/e2e.sh               # command-line and end-to-end checks
scripts/interop.sh           # Go/Python interoperability checks
interop/selftest.sh          # Python client/server checks
scripts/conformance.py P...  # the same 19 edge cases to several servers, compared
scripts/spec-pdf.sh          # SPEC.md to SPEC.pdf, and its page count
```

Byte-layout tests compare encoded frames with explicit byte sequences from
the spec. The test suite covers:

- **Framing:** every prefix of a frame (a TCP read can stop anywhere), and
  fuzzing of every decoder.
- **Errors and skipping:**
  - a malformed request answered with `400` on a connection that keeps serving;
  - the order of the `400`, `501` and `404` checks;
  - unknown frame types and field indexes skipped;
  - the cap on frames skipped in a row.
- **Timeouts and load:**
  - incomplete frames timing out even when bytes continue to arrive;
  - a slow response that must not cost the next pipelined request its time;
  - a client that stops reading being dropped;
  - the connection cap;
  - `accept` errors retried rather than fatal.
- **Files:** symbolic links out of the root, FIFOs, unreadable files, and
  paths that try to forge log lines.
- **The documents:** the bytes in `HEXDUMP.md` and the two examples in
  `SPEC.md` are decoded and checked against what their text claims.

### Interoperability

`interop/` contains a Python implementation of BH/1 for cross-testing with
the Go programs. `scripts/interop.sh` runs:

- `bcurl` against the Python server;
- the Python client against `bserve`;
- `scripts/conformance.py`, which sends the same 19 edge-case frames to both
  servers. The cases cover error order, NUL bytes, trailing slashes, skipped
  frames and fields, and a missing END_STREAM. Both servers give the answer
  the spec requires in all 19.

`interop/selftest.sh` runs 119 checks against the Python programs, including
malformed frames, timeout handling, pipelining and shutdown. Implementation
details and command-line differences are listed in `interop/NOTES.md`.

## Layout

```
SPEC.md                the protocol (two pages)
HEXDUMP.md             one exchange, annotated byte by byte
cmd/bserve             flags, signals
cmd/bcurl              flags, URL handling, exit status
internal/frame         the 8-byte header and the preface. Pure: no I/O
internal/message       REQUEST/RESPONSE payloads and header fields. Pure
internal/server        every socket and file operation of bserve
internal/client        every socket operation of bcurl, and its -v dump
www/                   a small site to serve
interop/               Python client, server and interoperability tests
scripts/               end-to-end, interop, conformance and PDF scripts
```
