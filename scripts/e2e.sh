#!/usr/bin/env bash
# End-to-end checks of the built binaries:
#   ./bserve ./www 9000
#   ./bcurl -v localhost:9000/index.html
# Usage: scripts/e2e.sh [port]   (default 9000)
set -uo pipefail
cd "$(dirname "$0")/.."
PORT="${1:-9000}"
URL="localhost:$PORT"

go build -o bserve ./cmd/bserve && go build -o bcurl ./cmd/bcurl || exit 1

LOG="$(mktemp)"
./bserve ./www "$PORT" 2>"$LOG" &
SERVER_PID=$!
trap 'kill "$SERVER_PID" 2>/dev/null; rm -f "$LOG"' EXIT

for _ in $(seq 50); do
  ./bcurl "$URL/" >/dev/null 2>&1 && break
  sleep 0.1
done

FAILED=0
check() { # check NAME COMMAND...
  local name="$1"; shift
  if "$@"; then echo "ok   $name"; else echo "FAIL $name"; FAILED=1; fi
}
connections() { grep -c "connected (connection" "$LOG"; }

body_matches()   { ./bcurl "$URL/index.html" | cmp -s - www/index.html; }
exit_is()        { local want="$1"; shift; "$@" >/dev/null 2>&1; [ $? -eq "$want" ]; }
dumps_5_frames() { [ "$(./bcurl -v "$URL/index.html" 2>&1 >/dev/null | grep -c '^[<>] 00000000')" -eq 5 ]; }
one_connection() {
  local before; before="$(connections)"
  ./bcurl "$URL/index.html" "$URL/missing" "$URL/about.txt" >/dev/null 2>&1
  [ $? -eq 4 ] && [ "$(connections)" -eq $((before + 1)) ]
}

# Malformed and unknown frames used to check error handling.
raw() {
  python3 - "$PORT" "$1" <<'EOF'
import socket, struct, sys
port, case = int(sys.argv[1]), sys.argv[2]

def frame(ftype, flags, stream, payload):
    return struct.pack(">HBBI", len(payload), ftype, flags, stream) + payload

def get(path):
    p = path.encode()
    return b"\x01" + struct.pack(">H", len(p)) + p

def read_status(sock, buf):
    # Read one RESPONSE and its DATA frames; return (status, stream, rest).
    status = stream = None
    while True:
        while len(buf) < 8 or len(buf) < 8 + struct.unpack(">H", buf[:2])[0]:
            chunk = sock.recv(65536)
            if not chunk:
                sys.exit("connection closed")
            buf += chunk
        length, ftype, flags, sid = struct.unpack(">HBBI", buf[:8])
        payload, buf = buf[8:8 + length], buf[8 + length:]
        if ftype == 2:
            status, stream = struct.unpack(">H", payload[:2])[0], sid
        if ftype in (2, 3) and flags & 1:
            return status, stream, buf

s = socket.create_connection(("localhost", port), timeout=3)
batch = b"BH/1"
if case == "malformed":
    batch += frame(1, 1, 1, b"\x01\x00\x09/")  # path length 9, one byte follows
    batch += frame(1, 1, 3, get("/index.html"))  # same connection, next request
    want = [(400, 1), (200, 3)]
elif case == "unknown-type":
    batch += frame(0x42, 0xFF, 1, b"x" * 1000)   # a version 2 frame
    batch += frame(1, 1, 5, get("/index.html"))
    want = [(200, 5)]
s.sendall(batch)
buf = s.recv(4)
assert buf == b"BH/1", buf
buf = b""
for want_status, want_stream in want:
    status, stream, buf = read_status(s, buf)
    assert (status, stream) == (want_status, want_stream), (status, stream)
EOF
}

check "bcurl writes the file to stdout byte for byte"   body_matches
check "exit 0 on 200"                                    exit_is 0 ./bcurl "$URL/index.html"
check "exit 4 on 404"                                    exit_is 4 ./bcurl "$URL/missing.html"
check "exit 2 for a usage error"                         exit_is 2 ./bcurl
check "exit 1 when nothing is listening"                 exit_is 1 ./bcurl localhost:1/x
check "-v hexdumps all 5 frames (2 prefaces, 3 frames)"  dumps_5_frames
check "3 URLs over 1 connection, worst status wins"      one_connection
check "malformed frame: 400, connection stays open"      raw malformed
check "unknown frame type skipped"                       raw unknown-type

kill -INT "$SERVER_PID"
wait "$SERVER_PID"
check "bserve exits 0 on Ctrl-C"                         [ $? -eq 0 ]
trap 'rm -f "$LOG"' EXIT

[ "$FAILED" -eq 0 ] && echo "all end-to-end checks passed"
exit "$FAILED"
