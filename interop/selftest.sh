#!/usr/bin/env bash
# selftest.sh: end-to-end checks for python_server.py and python_client.py.
#
#  1. Builds a temporary root: index.html, a 300,007-byte random binary, a file
#     of exactly 2 x 65,535 bytes, a subdirectory with its own index.html, an
#     empty file, plus symlinks, an unreadable file, a search-only directory, a
#     FIFO and a socket file for the edge cases.
#  2. Starts the server on port 0, so the OS picks a free ephemeral port (never
#     9000), and reads the chosen port from the server's log.
#  3. Runs the client against it.
#  4. Sends hand-built, malformed and unusual frames to the server (inline Python).
#  5. Points the client at tiny fake servers that misbehave on purpose.
#  6. Starts a second server to check idle closing and Ctrl-C.
# Prints PASS or FAIL per check; exits 0 only if every check passed.
#
# Usage: ./selftest.sh    (needs Python 3.13 as python3, or set PYTHON=...)

set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
PY="${PYTHON:-python3}"
SERVER="$HERE/python_server.py"
CLIENT="$HERE/python_client.py"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/bh1-selftest.XXXXXX")"
ROOT="$WORK/root"
SERVER_PID=""
PASSED=0
FAILED=0

cleanup() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null      # only the server this script started
    wait "$SERVER_PID" 2>/dev/null
  fi
  chmod u+rwx "$ROOT/searchonly" 2>/dev/null
  chmod -R u+rwx "$WORK" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %s\n' "$1"; }
fail() {
  FAILED=$((FAILED + 1)); printf 'FAIL  %s\n' "$1"
  if [ -n "${2:-}" ]; then printf '      %s\n' "$2"; fi
}

# tally FILE STATUS NAME: show the PASS/FAIL lines an inline Python block wrote, and count them.
tally() {
  cat "$1"
  PASSED=$((PASSED + $(grep -c '^PASS' "$1")))
  FAILED=$((FAILED + $(grep -c '^FAIL' "$1")))
  if [ "$2" -ne 0 ]; then fail "$3 crashed (exit status $2)"; fi
}

# fetch WANT_EXIT EXPECTED_BODY_FILE NAME [client arguments...]
# Runs the client; checks its exit status and, if a file is given, its stdout.
fetch() {
  local want="$1" expected="$2" name="$3"
  shift 3
  "$PY" "$CLIENT" "$@" >"$WORK/out" 2>"$WORK/err"
  local got=$?
  if [ "$got" -ne "$want" ]; then
    fail "$name" "client exit $got, wanted $want: $(head -c 300 "$WORK/err" | tr '\n' ' ')"
  elif [ -n "$expected" ] && ! cmp -s "$WORK/out" "$expected"; then
    fail "$name" "stdout differs from $expected"
  else
    pass "$name"
  fi
}

# fetch_error STATUS NAME URL...: the client exits 1 and reports STATUS.
fetch_error() {
  local status="$1" name="$2"
  shift 2
  "$PY" "$CLIENT" "$@" >"$WORK/out" 2>"$WORK/err"
  local got=$?
  if [ "$got" -eq 1 ] && grep -q "status $status\$" "$WORK/err"; then
    pass "$name"
  else
    fail "$name" "client exit $got: $(head -c 300 "$WORK/err" | tr '\n' ' ')"
  fi
}

# ------------------------------------------------------------------ fixtures
mkdir -p "$ROOT/sub" "$ROOT/emptydir" "$ROOT/searchonly" "$WORK/outside_dir"
printf '<!doctype html>\n<title>BH/1 selftest</title>\n<p>root index</p>\n' >"$ROOT/index.html"
printf '<p>sub index</p>\n' >"$ROOT/sub/index.html"
printf 'the file the spec example asks for\n' >"$ROOT/a"
: >"$ROOT/empty.txt"
"$PY" -c 'import os, sys; open(sys.argv[1], "wb").write(os.urandom(300007))' "$ROOT/big.bin"
"$PY" -c 'import sys; open(sys.argv[1], "wb").write((bytes(range(256)) * 512)[:2 * 65535])' "$ROOT/exact.bin"
printf 'a file with a space in its name\n' >"$ROOT/with space.txt"
printf 'outside the root\n' >"$WORK/outside.txt"
printf '<p>outside index</p>\n' >"$WORK/outside_dir/index.html"
ln -s "$WORK/outside.txt" "$ROOT/escape.txt"       # absolute symlink to a file outside the root
ln -s "$WORK/outside_dir" "$ROOT/escape_dir"       # absolute symlink to a directory outside the root
ln -s ../../outside.txt "$ROOT/sub/escape_up.txt"  # relative symlink that climbs out of the root
ln -s index.html "$ROOT/alias.html"                # relative symlink inside the root
ln -s sub "$ROOT/subalias"                         # relative symlink to a directory inside the root
ln -s ../index.html "$ROOT/sub/up.html"            # relative symlink with '..' that stays inside
ln -s "$ROOT/index.html" "$ROOT/abs_alias.html"    # absolute symlink that points inside the root
"$PY" -c 'import sys; open(sys.argv[1], "wb").truncate(8 << 20)' "$ROOT/huge.bin"   # 8 MiB of zeros (sparse)
ln -s loop_b "$ROOT/loop_a"                        # a symlink loop
ln -s loop_a "$ROOT/loop_b"
printf 'unreadable\n' >"$ROOT/unreadable.txt"
chmod 000 "$ROOT/unreadable.txt"
printf '<p>search-only index</p>\n' >"$ROOT/searchonly/index.html"
printf 'inside a search-only directory\n' >"$ROOT/searchonly/file.txt"
chmod 100 "$ROOT/searchonly"                       # owner may search it, but not list it
mkfifo "$ROOT/fifo"
(cd "$ROOT" && "$PY" -c 'import socket; socket.socket(socket.AF_UNIX).bind("sock")')   # a socket file

# ------------------------------------------------------------- start server
# Section 6 gives a frame at least 5 seconds, the shortest timeout allowed.
BH1_FRAME_TIMEOUT=5 "$PY" "$SERVER" "$ROOT" 0 2>"$WORK/server.log" &
SERVER_PID=$!
PORT=""
for _ in $(seq 1 100); do
  PORT="$(sed -n 's/.* listening on .* port \([0-9][0-9]*\),.*/\1/p' "$WORK/server.log")"
  [ -n "$PORT" ] && break
  kill -0 "$SERVER_PID" 2>/dev/null || break
  sleep 0.1
done
if [ -z "$PORT" ] || [ "$PORT" = 9000 ]; then
  echo "selftest: the server did not start on an ephemeral port:"
  cat "$WORK/server.log"
  exit 1
fi
HP="127.0.0.1:$PORT"
echo "selftest: server pid $SERVER_PID listening on $HP"

# ------------------------------------------------------ client vs. server
echo
echo "== the client against the server"
fetch 0 "$ROOT/index.html" "client: normal file" "$HP/index.html"
fetch 0 "$ROOT/big.bin" "client: 300,007-byte binary file, bytes match" "$HP/big.bin"
fetch 0 "$ROOT/exact.bin" "client: file of exactly 2 x 65,535 bytes" "$HP/exact.bin"
fetch 0 "$ROOT/sub/index.html" "client: directory with a trailing slash -> its index.html" "$HP/sub/"
fetch 0 "$ROOT/sub/index.html" "client: directory without a trailing slash -> its index.html" "$HP/sub"
fetch 0 "$ROOT/index.html" "client: / -> the root index.html" "$HP/"
fetch 0 "$ROOT/empty.txt" "client: empty file" "$HP/empty.txt"
fetch_error 404 "client: missing file -> 404, exit status 1" "$HP/no-such-file.html"
if grep -q '^404 ' "$WORK/out"; then pass "client: the 404 body goes to stdout"; else fail "client: the 404 body goes to stdout"; fi
fetch 0 "$ROOT/with space.txt" "client: percent-encoded URL path is decoded" "$HP/with%20space.txt"
fetch 0 "$ROOT/index.html" "client: dot segments are resolved before sending" "$HP/sub/../index.html"
fetch 0 "$ROOT/index.html" "client: a query (with a warning) and a fragment are dropped" "$HP/index.html?x=1#top"
fetch 2 "" "client: a URL whose path decodes to NUL is refused (a Path MUST NOT contain NUL)" "$HP/index.html%00.txt"
fetch 0 "$ROOT/index.html" "client: symlink that stays inside the root is served" "$HP/alias.html"
fetch_error 404 "client: symlink to a file outside the root -> 404" "$HP/escape.txt"
fetch_error 404 "client: symlinked directory outside the root -> 404" "$HP/escape_dir/"
fetch_error 404 "client: directory without index.html -> 404" "$HP/emptydir/"
fetch_error 404 "client: a FIFO is not served (404, no hang)" "$HP/fifo"
if [ "$(id -u)" -ne 0 ]; then
  fetch_error 403 "client: existing but unreadable file -> 403" "$HP/unreadable.txt"
fi

# Several URLs on one connection. The server logs every request with the
# client's address:port, so one connection means one source port.
cat "$ROOT/index.html" "$ROOT/sub/index.html" "$ROOT/empty.txt" "$ROOT/big.bin" "$ROOT/alias.html" >"$WORK/expected_multi"
BEFORE=$(wc -l <"$WORK/server.log")
fetch 0 "$WORK/expected_multi" "client: five URLs -> five bodies, in order" \
  "$HP/index.html" "$HP/sub/" "$HP/empty.txt" "$HP/big.bin" "$HP/alias.html"
for _ in $(seq 1 50); do
  [ "$(tail -n +$((BEFORE + 1)) "$WORK/server.log" | grep -c ' stream=')" -ge 5 ] && break
  sleep 0.1
done
tail -n +$((BEFORE + 1)) "$WORK/server.log" | grep ' stream=' >"$WORK/multi.log"
PEERS=$(awk '{print $3}' "$WORK/multi.log" | sort -u | wc -l | tr -d ' ')
STREAMS=$(awk '{print $4}' "$WORK/multi.log" | tr '\n' ' ')
if [ "$PEERS" = 1 ] && [ "$STREAMS" = "stream=1 stream=2 stream=3 stream=4 stream=5 " ]; then
  pass "client: the five requests shared one connection, stream IDs 1..5"
else
  fail "client: the five requests shared one connection, stream IDs 1..5" "peers=$PEERS streams=$STREAMS"
fi

fetch_error 404 "client: a 404 among other URLs -> exit status 1" "$HP/index.html" "$HP/missing" "$HP/sub/"
N1=$(($(wc -c <"$ROOT/index.html")))
N2=$(($(wc -c <"$ROOT/sub/index.html")))
if head -c "$N1" "$WORK/out" | cmp -s - "$ROOT/index.html" && tail -c "$N2" "$WORK/out" | cmp -s - "$ROOT/sub/index.html"; then
  pass "client: the bodies around the 404 are still written"
else
  fail "client: the bodies around the 404 are still written"
fi

fetch 0 "$ROOT/index.html" "client -v: the body still goes to stdout" -v "$HP/index.html"
if grep -q '^> preface' "$WORK/err" && grep -q '^< preface' "$WORK/err" &&
  grep -q '^00000000  42 48 2f 31 ' "$WORK/err" && grep -q '^> REQUEST stream=1 flags=0x01 END_STREAM' "$WORK/err" &&
  grep -q '^< RESPONSE stream=1' "$WORK/err" && grep -q '^< DATA stream=1 flags=0x01 END_STREAM' "$WORK/err"; then
  pass "client -v: hexdumps of both prefaces and every frame on stderr"
else
  fail "client -v: hexdumps of both prefaces and every frame on stderr" "$(head -c 300 "$WORK/err")"
fi

fetch 2 "" "client: no URL -> usage error, exit status 2"
fetch 2 "" "client: URLs on two different host:port pairs -> exit status 2" "$HP/index.html" "127.0.0.1:1/index.html"
fetch 2 "" "client: URL without a port -> exit status 2" "localhost/index.html"
FREE_PORT=$("$PY" -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
fetch 3 "" "client: nothing listening -> exit status 3" "127.0.0.1:$FREE_PORT/"

# ------------------------------------------------- hand-built frames vs. server
echo
echo "== hand-built frames against the server"
"$PY" - "$PORT" "$ROOT" >"$WORK/probe.out" 2>&1 <<'PROBE'
import os, socket, struct, sys, time

PORT, ROOT = int(sys.argv[1]), sys.argv[2]
HEADER = struct.Struct(">HBBI")
REQUEST, RESPONSE, DATA, END_STREAM = 1, 2, 3, 1
TABLE = {1: "host", 2: "user-agent", 3: "accept", 4: "server", 5: "date", 6: "content-type",
         7: "content-length", 8: "last-modified", 9: "etag", 10: "allow"}

def check(name, ok, detail=""):
    print(("PASS  " if ok else "FAIL  ") + "server: " + name)
    if not ok:
        print("      " + str(detail)[:400])

def frame(ftype, flags, sid, payload=b""):
    return HEADER.pack(len(payload), ftype, flags, sid) + payload

def field(index, value, name=b""):
    if index == 0:
        return struct.pack(">BH", 0, len(name)) + name + struct.pack(">H", len(value)) + value
    return struct.pack(">BH", index, len(value)) + value

def request(path, method=1, fields=None):
    if fields is None:
        fields = field(1, b"127.0.0.1")
    return struct.pack(">BH", method, len(path)) + path + fields

def parse_fields(payload, offset):
    fields = {}
    while offset < len(payload):
        index = payload[offset]; offset += 1
        name = TABLE.get(index)
        if index == 0:
            (n,) = struct.unpack_from(">H", payload, offset); offset += 2
            name = payload[offset:offset + n].decode(); offset += n
        (n,) = struct.unpack_from(">H", payload, offset); offset += 2
        if name is not None:
            fields.setdefault(name, payload[offset:offset + n])
        offset += n
    return fields

def read(name):
    with open(os.path.join(ROOT, name), "rb") as f:
        return f.read()

class Peer:
    """One raw connection to the server under test."""

    def __init__(self, preface=b"BH/1"):
        self.sock = socket.create_connection(("127.0.0.1", PORT), timeout=5)
        self.buf = b""
        self.server_preface = self.recv_exact(4)   # must arrive before we send anything
        if preface:
            self.sock.sendall(preface)

    def recv_exact(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise EOFError(f"server closed the connection ({len(self.buf)} of {n} bytes)")
            self.buf += chunk
        data, self.buf = self.buf[:n], self.buf[n:]
        return data

    def send(self, data):
        self.sock.sendall(data)

    def read_frame(self):
        length, ftype, flags, sid = HEADER.unpack(self.recv_exact(8))
        return ftype, flags, sid, self.recv_exact(length)

    def response(self):
        """One whole response, read strictly: a RESPONSE, then DATA on the
        same stream until END_STREAM, with nothing else in between."""
        ftype, flags, sid, payload = self.read_frame()
        assert ftype == RESPONSE, f"expected a RESPONSE, got frame type {ftype}"
        r = dict(sid=sid, status=struct.unpack_from(">H", payload)[0],
                 fields=parse_fields(payload, 2), body=b"", sizes=[],
                 head_end_stream=bool(flags & END_STREAM))
        while not flags & END_STREAM:
            ftype, flags, dsid, chunk = self.read_frame()
            assert ftype == DATA and dsid == sid, f"expected DATA on stream {sid}, got type {ftype} on {dsid}"
            r["body"] += chunk
            r["sizes"].append(len(chunk))
        return r

    def get(self, sid, payload, flags=END_STREAM):
        self.send(frame(REQUEST, flags, sid, payload))
        return self.response()

    def closed_without_more(self, wait=5):
        """True if the server closes the connection without sending another byte."""
        self.sock.settimeout(wait)
        try:
            return self.buf == b"" and self.sock.recv(1) == b""
        except ConnectionResetError:
            return self.buf == b""
        except TimeoutError:
            return False

def error_ok(r, sid, status):
    """A stream error: right stream and status, short text/plain body, matching content-length."""
    return (r["sid"] == sid and r["status"] == status and len(r["body"]) > 0
            and r["fields"].get("content-type", b"").startswith(b"text/plain")
            and r["fields"].get("content-length") == str(len(r["body"])).encode())

INDEX, BIG = read("index.html"), read("big.bin")

# Everything below runs on ONE connection: each stream error must leave it open.
p = Peer()
check("sends its preface on accept, before the client sends anything", p.server_preface == b"BH/1", p.server_preface)
try:
    r = p.get(1, request(b"/index.html"))
    check("plain GET -> 200 on stream 1 with the file's bytes", (r["sid"], r["status"], r["body"]) == (1, 200, INDEX), r)
    check("200 carries server, date, content-type, content-length, last-modified, etag",
          all(k in r["fields"] for k in ("server", "date", "content-type", "last-modified", "etag"))
          and r["fields"].get("content-length") == str(len(INDEX)).encode(), r["fields"])

    # Section 3's own example, byte for byte: GET /a with host: h on stream 1.
    p.send(bytes.fromhex("00 09 01 01 00 00 00 01 01 00 02 2F 61 01 00 01 68"))
    r = p.response()
    check("the spec's example REQUEST bytes (GET /a, host: h) are answered on stream 1",
          (r["sid"], r["status"], r["body"]) == (1, 200, read("a")), r)

    p.send(frame(0x7E, 0, 2, b"an unknown frame type, to be skipped"))
    r = p.get(3, request(b"/index.html"))
    check("unknown frame type before a REQUEST is skipped", (r["sid"], r["status"], r["body"]) == (3, 200, INDEX), r)

    extra = field(1, b"127.0.0.1") + field(200, b"reserved index") + field(0, b"v", b"x-literal") + field(0, b"x", b"Host")
    r = p.get(4, request(b"/index.html", fields=extra))
    check("unknown field index (200) and literal names (even 'Host') are skipped", (r["sid"], r["status"]) == (4, 200), r)

    r = p.get(5, struct.pack(">BH", 1, 500) + b"/index.html")
    check("bad path length (the GET payload ends inside the path) -> 400 on the same stream", error_ok(r, 5, 400), r)

    r = p.get(0, request(b"/index.html"))
    check("stream ID 0 -> 400, answered on stream 0", error_ok(r, 0, 400), r)

    r = p.get(6, request(b"/index.html", method=2))
    check("unknown method 0x02 -> 501 with allow: GET", error_ok(r, 6, 501) and r["fields"].get("allow") == b"GET", r)

    r = p.get(7, request(b"index.html", method=2))
    check("unknown method AND a bad path -> 501 (step 2 comes before the path check)", error_ok(r, 7, 501), r)

    r = p.get(40, b"\x02\xff\xff\x00")
    check("unknown method with any bytes after it -> 501 (nothing after the method byte is examined)", error_ok(r, 40, 501), r)

    r = p.get(0, b"\x02")
    check("unknown method on stream 0 -> 400 (step 1 comes before step 2)", error_ok(r, 0, 400), r)

    r = p.get(41, b"\x01")
    check("a GET payload of only the method byte -> 400 (ends inside a field)", error_ok(r, 41, 400), r)

    for sid, path in ((8, b"/sub/../index.html"), (9, b"/./index.html"), (10, b"index.html"), (11, b""),
                      (30, b"/index.html\x00.txt")):
        r = p.get(sid, request(path))
        check(f"path {path!r} breaks section 3 -> 400", error_ok(r, sid, 400), r)

    r = p.get(12, request(b"/index.html", fields=field(0, b"value", b"")))
    check("empty literal name -> 400", error_ok(r, 12, 400), r)

    r = p.get(13, request(b"/index.html", fields=b"\x01"))
    check("header field cut off after its index byte -> 400", error_ok(r, 13, 400), r)

    r = p.get(14, request(b"/index.html", fields=struct.pack(">BH", 1, 100) + b"abc"))
    check("value length runs past the payload -> 400", error_ok(r, 14, 400), r)

    r = p.get(15, b"")
    check("empty REQUEST payload -> 400", error_ok(r, 15, 400), r)

    r = p.get(16, request(b"/index.html"), flags=0xFF)
    check("unknown flag bits on a REQUEST are ignored", (r["sid"], r["status"]) == (16, 200), r)

    r = p.get(17, request(b"/index.html"), flags=0x00)
    check("REQUEST without END_STREAM is still answered", (r["sid"], r["status"]) == (17, 200), r)

    p.send(frame(DATA, END_STREAM, 18, b"DATA means nothing to a server")
           + frame(RESPONSE, END_STREAM, 18, b"\x00\xc8")
           + frame(0xFF, END_STREAM, 0, b"type 0xFF on stream 0")
           + frame(0x00, 0, 0, b"type 0x00 is unused too"))
    r = p.get(19, request(b"/index.html"))
    check("DATA, RESPONSE, type 0xFF and type 0x00 sent to the server are skipped", (r["sid"], r["status"]) == (19, 200), r)

    p.send(frame(0x42, 0, 0) * 1000)
    r = p.get(42, request(b"/index.html"))
    check("1,000 skipped frames in a row are tolerated (the cap is 'more than' 1,000)", (r["sid"], r["status"]) == (42, 200), r)

    r = p.get(0xFFFFFFFF, request(b"/index.html"))
    check("the largest stream ID, 0xFFFFFFFF, is echoed", (r["sid"], r["status"]) == (0xFFFFFFFF, 200), r)

    r = p.get(2, request(b"/index.html"))
    check("a smaller stream ID afterwards is still answered (order is not checked)", (r["sid"], r["status"]) == (2, 200), r)

    p.send(frame(REQUEST, END_STREAM, 20, request(b"/index.html"))
           + frame(REQUEST, END_STREAM, 21, request(b"/big.bin"))
           + frame(REQUEST, END_STREAM, 22, request(b"/missing")))
    a, b, c = p.response(), p.response(), p.response()
    got = [(x["sid"], x["status"]) for x in (a, b, c)]
    check("three pipelined REQUESTs -> answered in order, never interleaved", got == [(20, 200), (21, 200), (22, 404)], got)
    check("big file: bytes match, every DATA <= 65,535 bytes, total == content-length",
          b["body"] == BIG and max(b["sizes"]) <= 65535
          and b["fields"].get("content-length") == str(len(BIG)).encode(), b["sizes"])

    r = p.get(23, request(b"/empty.txt"))
    check("empty file -> 200 as just a RESPONSE carrying END_STREAM, content-length 0",
          r["status"] == 200 and r["head_end_stream"] and r["body"] == b""
          and r["fields"].get("content-length") == b"0", r)

    r = p.get(24, request(b"/sub"))
    check("directory -> its index.html", (r["status"], r["body"]) == (200, read("sub/index.html")), r)
    r = p.get(25, request(b"/emptydir/"))
    check("directory without index.html -> 404", error_ok(r, 25, 404), r)
    r = p.get(26, request(b"/index.html/"))
    check("a file asked for with a trailing slash -> 404", error_ok(r, 26, 404), r)
    r = p.get(27, request(b"/with%20space.txt"))
    check("path bytes are literal: the server does not percent-decode", error_ok(r, 27, 404), r)
    r = p.get(28, request(b"/with space.txt"))
    check("a raw space in the path is served", (r["status"], r["body"]) == (200, read("with space.txt")), r)
    r = p.get(29, request(b"//index.html"))
    check("'//' means '/'", (r["status"], r["body"]) == (200, INDEX), r)

    # The one-step lookup (section 6, step 4), symlink by symlink.
    for sid, path, want in ((31, b"/escape.txt", 404), (32, b"/escape_dir/index.html", 404),
                            (33, b"/sub/escape_up.txt", 404), (34, b"/fifo", 404), (35, b"/sock", 404),
                            (36, b"/loop_a", 404)):
        r = p.get(sid, request(path))
        check(f"{path.decode()} -> 404", error_ok(r, sid, want), r)
    for sid, path, name in ((43, b"/alias.html", "index.html"), (44, b"/subalias/", "sub/index.html"),
                            (45, b"/sub/up.html", "index.html")):
        r = p.get(sid, request(path))
        check(f"{path.decode()} -> 200 with {name}", (r["sid"], r["status"], r["body"]) == (sid, 200, read(name)), r)
    # Section 6 lets a server "treat any absolute link as outside"; this one
    # does, so the answer cannot depend on how the temporary root is spelled.
    r = p.get(46, request(b"/abs_alias.html"))
    check("/abs_alias.html, an absolute link that points into the root -> 404 (any absolute link is outside)",
          error_ok(r, 46, 404), r)
    if os.geteuid() != 0:
        r = p.get(47, request(b"/unreadable.txt"))
        check("existing but unreadable file -> 403", error_ok(r, 47, 403), r)
        r = p.get(48, request(b"/searchonly/file.txt"))
        check("a file under a search-only directory is served (as by path)",
              (r["status"], r["body"]) == (200, b"inside a search-only directory\n"), r)

    whole = frame(REQUEST, END_STREAM, 37, request(b"/index.html"))
    for piece in (whole[:3], whole[3:8], whole[8:20], whole[20:]):
        p.send(piece)
        time.sleep(0.15)
    r = p.response()
    check("a frame trickled in four pieces is reassembled", (r["sid"], r["status"]) == (37, 200), r)

    r = p.get(38, request(b"/index.html", fields=field(7, b"12345") + field(10, b"PUT")))
    check("server-only field names in a REQUEST are ignored", (r["sid"], r["status"]) == (38, 200), r)

    r = p.get(39, request(b"/index.html"))
    check("after all of the above, the same connection still answers", (r["sid"], r["status"], r["body"]) == (39, 200, INDEX), r)
except Exception as error:
    check("probe sequence on one connection", False, f"{type(error).__name__}: {error}")

# Connection-level behaviour, each on its own connection.
try:
    q = Peer()
    q.send(frame(REQUEST, END_STREAM, 1, request(b"/index.html")) + frame(REQUEST, END_STREAM, 2, request(b"/sub/")))
    q.sock.shutdown(socket.SHUT_WR)   # like "printf ... | nc": the client is done sending
    a, b = q.response(), q.response()
    check("client half-closes after pipelining: both REQUESTs are still answered, then EOF",
          [(a["sid"], a["status"]), (b["sid"], b["status"])] == [(1, 200), (2, 200)] and q.closed_without_more(),
          (a, b))

    q = Peer()
    q.send(frame(0x42, 0, 0) * 1001 + frame(REQUEST, END_STREAM, 1, request(b"/index.html")))
    check("1,001 skipped frames in a row -> closed without answering", q.closed_without_more(), "")

    q = Peer(preface=b"GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
    check("wrong preface (an HTTP/1.1 request) -> closed without sending more", q.closed_without_more(), "")

    q = Peer(preface=b"X")
    start = time.monotonic()
    ok = q.closed_without_more()
    check("a wrong first preface byte is rejected at once", ok and time.monotonic() - start < 0.5, time.monotonic() - start)

    q = Peer(preface=b"BH")
    time.sleep(0.1)
    q.send(b"/1")
    r = q.get(1, request(b"/index.html"))
    check("a preface split across two writes is accepted", (r["sid"], r["status"]) == (1, 200), r)

    q = Peer()
    q.send(HEADER.pack(20, REQUEST, END_STREAM, 1) + b"/inde")
    start = time.monotonic()
    ok = q.closed_without_more(wait=15)
    elapsed = time.monotonic() - start
    check("a frame that never completes -> closed, unanswered, after the 5 s frame timeout",
          ok and 4.5 < elapsed < 12, f"closed={ok} after {elapsed:.2f}s")
except Exception as error:
    check("connection-level probes", False, f"{type(error).__name__}: {error}")
PROBE
tally "$WORK/probe.out" $? "server probe script"

# ----------------------------------------------- client vs. fake servers
echo
echo "== the client against misbehaving fake servers"
"$PY" - "$CLIENT" >"$WORK/fake.out" 2>&1 <<'FAKE'
import os, socket, struct, subprocess, sys, threading, time

CLIENT = sys.argv[1]
HEADER = struct.Struct(">HBBI")
REQUEST, RESPONSE, DATA, END_STREAM = 1, 2, 3, 1

def check(name, ok, detail=""):
    print(("PASS  " if ok else "FAIL  ") + "client: " + name)
    if not ok:
        print("      " + str(detail)[:400])

def frame(ftype, flags, sid, payload=b""):
    return HEADER.pack(len(payload), ftype, flags, sid) + payload

def field(index, value, name=b""):
    if index == 0:
        return struct.pack(">BH", 0, len(name)) + name + struct.pack(">H", len(value)) + value
    return struct.pack(">BH", index, len(value)) + value

def head(status, *fields):
    return struct.pack(">H", status) + b"".join(fields)

def recv_exact(conn, n):
    data = b""
    while len(data) < n:
        chunk = conn.recv(n - len(data))
        if not chunk:
            raise EOFError("the client closed the connection")
        data += chunk
    return data

def take_requests(conn, count, preface=True):
    """Read the client's preface (unless already read) and `count` frames."""
    if preface:
        assert recv_exact(conn, 4) == b"BH/1", "client preface"
    frames = []
    for _ in range(count):
        length, ftype, flags, sid = HEADER.unpack(recv_exact(conn, 8))
        frames.append((ftype, flags, sid, recv_exact(conn, length)))
    return frames

def run(behaviour, paths=("/x",), rcvbuf=None):
    """Run the client against a fake server that accepts one connection and
    hands it to behaviour(conn). Returns (exit status, stdout, stderr, seconds)."""
    listener = socket.socket()
    if rcvbuf:   # set before listen() so the accepted socket gets it too
        listener.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, rcvbuf)
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    listener.settimeout(10)
    port = listener.getsockname()[1]

    def serve():
        try:
            conn, _ = listener.accept()
        except OSError:
            return
        conn.settimeout(20)
        try:
            behaviour(conn)
            while conn.recv(65536):   # let the client read everything and hang up first
                pass
        except Exception:
            pass
        finally:
            conn.close()

    thread = threading.Thread(target=serve, daemon=True)
    thread.start()
    start = time.monotonic()
    try:
        proc = subprocess.run([sys.executable, CLIENT] + [f"127.0.0.1:{port}{p}" for p in paths],
                              capture_output=True, timeout=40, env=dict(os.environ, BH1_TIMEOUT="5"))
        result = proc.returncode, proc.stdout, proc.stderr.decode(errors="replace")
    except subprocess.TimeoutExpired:
        result = None, b"", "the client hung"
    thread.join(5)
    listener.close()
    return result + (time.monotonic() - start,)

def request_parts(payload):
    """(method, path, {index or literal name: first value}) of a REQUEST payload."""
    (n,) = struct.unpack_from(">H", payload, 1)
    fields, i = {}, 3 + n
    while i < len(payload):
        index = payload[i]; i += 1
        name = index
        if index == 0:
            (k,) = struct.unpack_from(">H", payload, i); i += 2
            name = payload[i:i + k]; i += k
        (k,) = struct.unpack_from(">H", payload, i); i += 2
        fields.setdefault(name, payload[i:i + k]); i += k
    return payload[0], payload[3:3 + n], fields

def answer(*frames, count=1):
    """A behaviour: read `count` requests, then send the preface and frames."""
    def behaviour(conn):
        take_requests(conn, count)
        conn.sendall(b"BH/1" + b"".join(frames))
    return behaviour

code, out, err, _ = run(lambda conn: conn.sendall(b"HTTP/1.1 200 OK\r\n\r\n"))
check("wrong server preface -> exit 3, nothing on stdout", code == 3 and out == b"" and "preface" in err, (code, out, err))

code, out, err, _ = run(answer(bytes.fromhex("00 02 02 01 00 00 00 01 00 C8")))
check("the spec's example '200 with no fields and no body' bytes -> exit 0, empty body", code == 0 and out == b"", (code, out, err))

code, out, err, _ = run(answer(frame(RESPONSE, 0, 1, head(200, field(7, b"10"))), frame(DATA, END_STREAM, 1, b"12345")))
check("content-length 10 but 5 DATA bytes -> response failed, exit 3", code == 3 and "content-length" in err, (code, err))

code, out, err, _ = run(answer(frame(RESPONSE, 0, 1, head(200, field(7, b"2"))), frame(DATA, END_STREAM, 1, b"12345")))
check("more DATA than content-length -> response failed, exit 3", code == 3 and "content-length" in err, (code, err))

def hang_up(conn):
    answer(frame(RESPONSE, 0, 1, head(200, field(7, b"10"))), frame(DATA, 0, 1, b"12345"))(conn)
    conn.shutdown(socket.SHUT_WR)
code, out, err, _ = run(hang_up)
check("server closes before END_STREAM -> exit 3", code == 3 and "closed" in err, (code, err))

def close_between(conn):
    answer(frame(RESPONSE, 0, 1, head(200, field(7, b"6"))), frame(DATA, END_STREAM, 1, b"first\n"), count=2)(conn)
    conn.shutdown(socket.SHUT_WR)    # "Either side MAY close between messages"
code, out, err, _ = run(close_between, paths=("/1", "/2"))
check("server closes between messages: the first body is kept, the second URL fails (no retry), exit 3",
      code == 3 and out == b"first\n" and "/2: failed" in err, (code, out, err))

code, out, err, _ = run(answer(frame(RESPONSE, END_STREAM, 2, head(200))))
check("RESPONSE on the wrong stream ID -> exit 3", code == 3 and "stream 2" in err, (code, err))

code, out, err, _ = run(answer(frame(DATA, END_STREAM, 1, b"orphan")))
check("DATA before any RESPONSE -> exit 3", code == 3, (code, err))

code, out, err, _ = run(answer(frame(RESPONSE, 0, 1, head(200)), frame(RESPONSE, END_STREAM, 1, head(200))))
check("a second RESPONSE on the same stream -> exit 3", code == 3 and "second RESPONSE" in err, (code, err))

code, out, err, _ = run(answer(frame(RESPONSE, END_STREAM, 1, head(0))))
check("status 0 (not a final status, 200 to 599) -> exit 3", code == 3 and "status 0" in err, (code, err))

code, out, err, _ = run(answer(frame(RESPONSE, END_STREAM, 1, head(103))))
check("status 103 (v1 has no interim responses) -> exit 3", code == 3 and "status 103" in err, (code, err))

code, out, err, elapsed = run(answer(HEADER.pack(40, RESPONSE, 0, 1) + b"\x00\xc8" + b"x" * 8))
check("a frame that never completes -> exit 3 after the 5 s frame timeout",
      code == 3 and "timed out" in err and 4.5 < elapsed < 15, (code, err, elapsed))

def skippables(conn):
    take_requests(conn, 1)
    conn.sendall(b"BH")
    time.sleep(0.05)
    conn.sendall(b"/1"
                 + frame(0x42, END_STREAM, 0, b"unknown type on stream 0, END_STREAM set")
                 + frame(RESPONSE, 0, 1, head(200, field(77, b"reserved index"), field(0, b"v", b"x-future"),
                                             field(0, b"99", b"Content-Length"),
                                             field(7, b"11"), field(0, b"999", b"content-length")))
                 + frame(0xFE, END_STREAM, 1, b"unknown type between RESPONSE and DATA, END_STREAM set")
                 + frame(0x7E, 0, 7, b"unknown type on a stream the client is not reading")
                 + frame(REQUEST, END_STREAM, 9, b"a REQUEST, on another stream, means nothing to a client")
                 + frame(DATA, 0xFE, 1, b"hello ")
                 + frame(DATA, 0, 1, b"")
                 + frame(DATA, 0x80 | END_STREAM, 1, b"world"))
code, out, err, _ = run(skippables)
check("skips unknown frames and REQUESTs on any stream, unknown fields, 'Content-Length' and flag bits; "
      "empty DATA mid-body; first content-length wins",
      code == 0 and out == b"hello world", (code, out, err))

code, out, err, _ = run(answer(frame(0x42, 0, 0) * 1000 + frame(RESPONSE, END_STREAM, 1, head(200))))
check("1,000 skipped frames in a row are tolerated", code == 0, (code, err))

code, out, err, _ = run(answer(frame(0x42, 0, 0) * 1001 + frame(RESPONSE, END_STREAM, 1, head(200))))
check("1,001 skipped frames in a row -> connection closed, exit 3", code == 3 and "skipped" in err, (code, err))

code, out, err, _ = run(answer(frame(RESPONSE, 0, 1, head(200, field(7, b"+5"))), frame(DATA, END_STREAM, 1, b"12345")))
check("content-length '+5' is not all digits -> response failed, exit 3", code == 3 and "digits" in err, (code, err))

code, out, err, _ = run(answer(frame(RESPONSE, END_STREAM, 1, head(200, field(7, b"")))))
check("an empty content-length -> response failed, exit 3",
      code == 3 and "digits" in err, (code, err))

code, out, err, _ = run(answer(frame(RESPONSE, END_STREAM, 1, head(200, field(7, b"0"))),
                               frame(RESPONSE, 0, 2, head(200, field(7, b"0"))), frame(DATA, END_STREAM, 2),
                               count=2), paths=("/a", "/b"))
check("an empty body as RESPONSE+END_STREAM or as one empty DATA: both accepted", code == 0 and out == b"", (code, out, err))

code, out, err, _ = run(answer(frame(RESPONSE, 0, 1, head(503, field(7, b"10"))), frame(DATA, END_STREAM, 1, b"try later\n")))
check("a 5xx -> its body on stdout, exit 1", code == 1 and out == b"try later\n", (code, out, err))

code, out, err, _ = run(answer(frame(RESPONSE, END_STREAM, 1, head(200) + b"\x07\x00\x09ab"),
                               frame(RESPONSE, 0, 2, head(200, field(7, b"2"))), frame(DATA, END_STREAM, 2, b"ok"),
                               count=2), paths=("/1", "/2"))
check("a malformed RESPONSE fails only that response; the next is still read (exit 3)",
      code == 3 and out == b"ok" and "malformed" in err, (code, out, err))

# Section 4: a pipelining client MUST keep reading while it writes, because a
# server need not read while it writes. This server reads one REQUEST, then
# writes an 8 MiB body without reading anything else, with a small receive
# buffer; only then does it read the other eleven ~50 KB REQUESTs.
BIG_BODY = 8 * 1024 * 1024
def busy_writer(conn):
    take_requests(conn, 1)
    conn.sendall(b"BH/1" + frame(RESPONSE, 0, 1, head(200, field(7, str(BIG_BODY).encode()))))
    chunk, left = b"z" * 65535, BIG_BODY
    while left:
        piece = chunk[:min(65535, left)]
        left -= len(piece)
        conn.sendall(frame(DATA, 0 if left else END_STREAM, 1, piece))
    for _, _, sid, _ in take_requests(conn, 11, preface=False):
        conn.sendall(frame(RESPONSE, END_STREAM, sid, head(200)))
long_paths = ["/" + "abcdefghijkl"[i] * 50000 for i in range(12)]
code, out, err, _ = run(busy_writer, paths=long_paths, rcvbuf=8192)
check("keeps reading while it writes: 12 x 50 KB REQUESTs against a server writing 8 MiB without reading",
      code == 0 and len(out) == BIG_BODY, (code, len(out), err))

seen = {}
def record(conn):
    seen["frames"] = take_requests(conn, 2)
    conn.sendall(b"BH/1" + frame(RESPONSE, END_STREAM, 1, head(204)) + frame(RESPONSE, END_STREAM, 2, head(204)))
code, out, err, _ = run(record, paths=("/a/../b%20c", "/d/"))
frames = seen.get("frames", [])
parts = [request_parts(f[3]) for f in frames]
check("sends GET REQUESTs with END_STREAM, stream IDs 1 and 2, resolved paths, a host field",
      code == 0 and [(f[0], f[1] & END_STREAM, f[2]) for f in frames] == [(REQUEST, 1, 1), (REQUEST, 1, 2)]
      and [(m, path) for m, path, _ in parts] == [(1, b"/b c"), (1, b"/d/")]
      and all(fields.get(1, b"").startswith(b"127.0.0.1:") for _, _, fields in parts),
      (code, frames, err))
FAKE
tally "$WORK/fake.out" $? "fake-server script"

# ------------------------------------------------------- a second server process
echo
echo "== a second server: idle closing, the frame-timeout floor, Ctrl-C with a drain"
"$PY" - "$SERVER" "$ROOT" >"$WORK/second.out" 2>&1 <<'SECOND'
import os, signal, socket, struct, subprocess, sys, time

HEADER = struct.Struct(">HBBI")

def check(name, ok, detail=""):
    print(("PASS  " if ok else "FAIL  ") + "server: " + name)
    if not ok:
        print("      " + str(detail)[:400])

def get(path, sid):
    payload = struct.pack(">BH", 1, len(path)) + path
    return HEADER.pack(len(payload), 1, 1, sid) + payload

class Peer:
    def __init__(self, port, rcvbuf=None):
        self.sock = socket.socket()
        if rcvbuf:   # a small receive window keeps the server blocked mid-response
            self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, rcvbuf)
        self.sock.settimeout(5)
        self.sock.connect(("127.0.0.1", port))
        self.buf = b""
        assert self.recv_exact(4) == b"BH/1"
        self.sock.sendall(b"BH/1")

    def recv_exact(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise EOFError("closed")
            self.buf += chunk
        data, self.buf = self.buf[:n], self.buf[n:]
        return data

    def status_of_next_response(self):
        """Read one RESPONSE and its DATA; return the status."""
        length, _, flags, _ = HEADER.unpack(self.recv_exact(8))
        status = struct.unpack(">H", self.recv_exact(length)[:2])[0]
        while not flags & 1:
            length, _, flags, _ = HEADER.unpack(self.recv_exact(8))
            self.recv_exact(length)
        return status

    def response_size(self):
        """Read one RESPONSE and its DATA; return (status, body size)."""
        length, _, flags, _ = HEADER.unpack(self.recv_exact(8))
        status = struct.unpack(">H", self.recv_exact(length)[:2])[0]
        size = 0
        while not flags & 1:
            length, _, flags, _ = HEADER.unpack(self.recv_exact(8))
            size += len(self.recv_exact(length))
        return status, size

    def is_open(self):
        """True if the server has not closed the connection."""
        self.sock.settimeout(0.2)
        try:
            return self.sock.recv(1) != b""
        except TimeoutError:
            return True
        except OSError:
            return False

    def closes_within(self, seconds):
        start = time.monotonic()
        while time.monotonic() - start < seconds:
            if not self.is_open():
                return True
        return False

# Started from Python, not with "&" in a shell script: a non-interactive
# shell makes background jobs ignore SIGINT, which would hide what Ctrl-C does.
env = dict(os.environ, BH1_IDLE_TIMEOUT="2", BH1_FRAME_TIMEOUT="1")
server = subprocess.Popen([sys.executable, sys.argv[1], sys.argv[2], "0"], stderr=subprocess.PIPE, text=True, env=env)
lines = [server.stderr.readline(), server.stderr.readline()]
check("asks for a 1 s frame timeout -> raised to the 5 s minimum (section 6)", "raised to 5 s" in lines[0], lines)
port = int(lines[1].split(" port ")[1].split(",")[0])
signalled = False
try:
    # An unknown frame every 0.25 s, but never a REQUEST: section 4 counts
    # only REQUESTs as activity, so the server may still close it as idle.
    p = Peer(port)
    start, closed_after = time.monotonic(), None
    while time.monotonic() - start < 5:
        try:
            p.sock.sendall(HEADER.pack(0, 0x42, 0, 0))
        except OSError:
            closed_after = time.monotonic() - start
            break
        if not p.is_open():
            closed_after = time.monotonic() - start
            break
    check("no REQUEST for BH1_IDLE_TIMEOUT (2 s) -> closed, even while skippable frames trickle in",
          closed_after is not None and 1.5 < closed_after < 4, f"closed after {closed_after}")

    # A REQUEST every 1.5 s, each answered, keeps it open well past 2 s.
    p = Peer(port)
    statuses = []
    for sid in (1, 2, 3):
        p.sock.sendall(get(b"/index.html", sid))
        statuses.append(p.status_of_next_response())
        time.sleep(1.5)
    check("a REQUEST every 1.5 s keeps the connection open past the 2 s idle limit",
          statuses == [200, 200, 200] and p.is_open(), statuses)
    p.sock.close()

    # The frame timeout really is 5 s, not the 1 s asked for.
    p = Peer(port)
    p.sock.sendall(HEADER.pack(20, 1, 1, 1) + b"/inde")
    start = time.monotonic()
    while p.is_open() and time.monotonic() - start < 12:
        pass
    elapsed = time.monotonic() - start
    check("a stalled frame is given the full 5 s before the connection is closed",
          4.5 < elapsed < 10, f"closed after {elapsed:.2f}s")

    # Shutting down: section 4 says the server "SHOULD NOT [close] while
    # holding an unanswered REQUEST". Ctrl-C arrives while an 8 MiB response
    # is blocked on a client with a tiny receive window, and while another
    # connection sits idle.
    busy = Peer(port, rcvbuf=4096)
    busy.sock.sendall(get(b"/huge.bin", 1))
    idle = Peer(port)
    time.sleep(0.5)                     # the server is now blocked writing huge.bin
    server.send_signal(signal.SIGINT)
    signalled = True
    check("Ctrl-C: an idle connection, which holds no REQUEST, is closed promptly",
          idle.closes_within(3), "still open")
    status, size = busy.response_size()
    check("Ctrl-C mid-response: the 8 MiB response still completes, then that connection closes",
          (status, size) == (200, 8 << 20) and busy.closes_within(3), (status, size))
except Exception as error:
    check("second-server probes", False, f"{type(error).__name__}: {error}")
finally:
    if not signalled:                   # the drain test never got that far
        server.send_signal(signal.SIGINT)
    try:
        rest = server.communicate(timeout=20)[1]
    except subprocess.TimeoutExpired:
        server.kill()
        rest = server.communicate()[1]
check("Ctrl-C (SIGINT) stops it cleanly: exit status 0, every connection finished, none dropped",
      server.returncode == 0 and "shutting down" in rest and "stopped" in rest
      and "dropping" not in rest and "Traceback" not in rest, rest[-400:])
order = [line for line in rest.splitlines() if "shutting down: answering" in line or "GET /huge.bin" in line]
check("the log shows the huge.bin transfer finishing after 'shutting down' (it really was in flight)",
      len(order) == 2 and "shutting down" in order[0] and "-> 200" in order[1], order)
SECOND
tally "$WORK/second.out" $? "second-server script"

if kill -0 "$SERVER_PID" 2>/dev/null; then pass "server: still running after every test above"; else fail "server: died during the tests"; fi
if grep -q -e 'internal error' -e 'Traceback' "$WORK/server.log"; then
  fail "server: no internal errors in its log" "$(grep -e 'internal error' -e 'Traceback' "$WORK/server.log" | head -n 3)"
else
  pass "server: no internal errors in its log"
fi
echo
echo "last lines of the server log (one line per request):"
tail -n 5 "$WORK/server.log" | sed 's/^/  /'

echo
echo "selftest: $PASSED passed, $FAILED failed"
if [ "$FAILED" -eq 0 ]; then exit 0; else exit 1; fi
