#!/usr/bin/env bash
# Interoperability checks between the Go and Python implementations.
#   bcurl (Go)               ->  interop/python_server.py
#   interop/python_client.py  ->  bserve (Go)
# Checks both client/server combinations and compares protocol edge cases.
set -uo pipefail
cd "$(dirname "$0")/.."

go build -o bserve ./cmd/bserve && go build -o bcurl ./cmd/bcurl || exit 1

ROOT="$(mktemp -d)"
LOG="$(mktemp)"
PIDS=()
cleanup() {
  for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null; done
  rm -rf "$ROOT" "$LOG"
}
trap cleanup EXIT

# A site with one of each thing worth checking.
printf '<!doctype html>\n<title>interop</title>\n<h1>interop</h1>\n' > "$ROOT/index.html"
mkdir -p "$ROOT/sub" && printf '<p>sub</p>\n' > "$ROOT/sub/index.html"
: > "$ROOT/empty.txt"
head -c 300000 /dev/urandom > "$ROOT/big.bin"

free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
wait_for() { # wait_for PORT
  for _ in $(seq 50); do
    python3 -c "import socket,sys; socket.create_connection(('127.0.0.1',$1),timeout=0.2)" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

FAILED=0
check() { # check NAME COMMAND...
  local name="$1"; shift
  if "$@"; then echo "ok   $name"; else echo "FAIL $name"; FAILED=1; fi
}
exit_is() { local want="$1"; shift; "$@" >/dev/null 2>&1; [ $? -eq "$want" ]; }
same() { cmp -s "$1" "$2"; }

echo "== Go client -> Python server"
P1="$(free_port)"
python3 interop/python_server.py "$ROOT" "$P1" 2>/dev/null &
PIDS+=($!)
wait_for "$P1" || { echo "FAIL Python server did not start"; exit 1; }
S="127.0.0.1:$P1"
check "index.html, byte for byte"          same <(./bcurl "$S/index.html") "$ROOT/index.html"
check "300 KB across many DATA frames"     same <(./bcurl "$S/big.bin") "$ROOT/big.bin"
check "directory serves its index.html"    same <(./bcurl "$S/sub/") "$ROOT/sub/index.html"
check "empty file"                         same <(./bcurl "$S/empty.txt") "$ROOT/empty.txt"
check "404 exits 4"                        exit_is 4 ./bcurl "$S/missing"
check "-v hexdumps against the Python server" bash -c "./bcurl -v '$S/index.html' 2>&1 >/dev/null | grep -q '^< 00000000'"
check "4 URLs, 1 connection, exit 4"       exit_is 4 ./bcurl "$S/index.html" "$S/missing" "$S/big.bin" "$S/sub/"

echo "== Python client -> Go server"
P2="$(free_port)"
./bserve "$ROOT" "$P2" 2>"$LOG" &
PIDS+=($!)
wait_for "$P2" || { echo "FAIL bserve did not start"; exit 1; }
B="127.0.0.1:$P2"
conns() { grep -c "connected (connection" "$LOG"; }
check "index.html, byte for byte"          same <(python3 interop/python_client.py "$B/index.html") "$ROOT/index.html"
check "300 KB across many DATA frames"     same <(python3 interop/python_client.py "$B/big.bin") "$ROOT/big.bin"
check "directory serves its index.html"    same <(python3 interop/python_client.py "$B/sub/") "$ROOT/sub/index.html"
check "empty file"                         same <(python3 interop/python_client.py "$B/empty.txt") "$ROOT/empty.txt"
check "404 makes the Python client fail"    bash -c "python3 interop/python_client.py '$B/missing' >/dev/null 2>&1; [ \$? -ne 0 ]"
before="$(conns)"
python3 interop/python_client.py "$B/index.html" "$B/big.bin" "$B/sub/" >/dev/null 2>&1
check "3 URLs, 1 connection to bserve"     [ "$(conns)" -eq $((before + 1)) ]

echo "== the same 19 edge cases sent to both servers (scripts/conformance.py)"
check "both servers answer every edge case as SPEC.md says" python3 scripts/conformance.py "$P1" "$P2"

[ "$FAILED" -eq 0 ] && echo "all interop checks passed"
exit "$FAILED"
