#!/usr/bin/env python3
"""Send the same edge-case frames to several BH/1 servers and compare answers.

Usage: scripts/conformance.py PORT [PORT ...]

Every server must give the status SPEC.md requires for each case, on the
stream the request used, over one connection that survives every case. The
cases cover error order, path rules, skipped frames and fields, and missing
END_STREAM.
The site under each server's root must hold index.html and sub/index.html.
"""

import socket
import struct
import sys

PREFACE = b"BH/1"
REQUEST, RESPONSE, DATA = 0x01, 0x02, 0x03
END_STREAM = 0x01


def frame(ftype, flags, stream, payload=b""):
    return struct.pack(">HBBI", len(payload), ftype, flags, stream) + payload


def string(b):
    return struct.pack(">H", len(b)) + b


def get(path, method=0x01, fields=b""):
    return bytes([method]) + string(path) + fields


# (description, frames to send before the request, request flags, stream,
#  REQUEST payload, status SPEC.md requires)
CASES = [
    ("plain GET", b"", END_STREAM, 1, get(b"/index.html"), 200),
    ("missing file", b"", END_STREAM, 3, get(b"/missing"), 404),
    ("trailing / after a file", b"", END_STREAM, 5, get(b"/index.html/"), 404),
    ("directory without /", b"", END_STREAM, 7, get(b"/sub"), 200),
    ("root directory", b"", END_STREAM, 9, get(b"/"), 200),
    ("unknown method, garbage after it", b"", END_STREAM, 11, b"\x07\xff\xff", 501),
    ("unknown method, bad path", b"", END_STREAM, 13, get(b"no-slash", method=0x07), 501),
    ("stream 0", b"", END_STREAM, 0, get(b"/index.html"), 400),
    ("empty payload", b"", END_STREAM, 15, b"", 400),
    ("path without leading /", b"", END_STREAM, 17, get(b"index.html"), 400),
    ("dot segment", b"", END_STREAM, 19, get(b"/sub/../index.html"), 400),
    ("NUL in path", b"", END_STREAM, 21, get(b"/index.html\x00.txt"), 400),
    ("path length overruns", b"", END_STREAM, 23, b"\x01\x00\x09/", 400),
    ("empty literal name", b"", END_STREAM, 25, get(b"/index.html", fields=b"\x00\x00\x00\x00\x00"), 400),
    ("reserved field index 200", b"", END_STREAM, 27, get(b"/index.html", fields=b"\xc8" + string(b"v2")), 200),
    ("literal Host is just unknown", b"", END_STREAM, 29, get(b"/index.html", fields=b"\x00" + string(b"Host") + string(b"x")), 200),
    ("REQUEST without END_STREAM", b"", 0x00, 31, get(b"/index.html"), 200),
    ("every flag bit set", b"", 0xFF, 33, get(b"/index.html"), 200),
    ("unknown frame types first", frame(0x42, 0, 1, b"x" * 300) + frame(0x00, 0xFF, 0), END_STREAM, 35, get(b"/index.html"), 200),
]


class Conn:
    def __init__(self, port):
        self.sock = socket.create_connection(("127.0.0.1", port), timeout=5)
        self.buf = b""
        self.sock.sendall(PREFACE)
        if self.read(4) != PREFACE:
            raise SystemExit(f"port {port}: no BH/1 preface")

    def read(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise ConnectionError("server closed the connection")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def response(self):
        """Return (status, stream) of the next response, consuming its DATA."""
        status = None
        while True:
            length, ftype, flags, stream = struct.unpack(">HBBI", self.read(8))
            payload = self.read(length)
            if ftype == RESPONSE:
                status, head_stream = struct.unpack(">H", payload[:2])[0], stream
            if ftype in (RESPONSE, DATA) and flags & END_STREAM:
                return status, head_stream


def run(port):
    conn = Conn(port)
    results = []
    for name, before, flags, stream, payload, _ in CASES:
        conn.sock.sendall(before + frame(REQUEST, flags, stream, payload))
        try:
            results.append(conn.response())
        except (ConnectionError, OSError) as e:
            results.append((f"closed: {e}", None))
            break
    return results


def main():
    ports = [int(p) for p in sys.argv[1:]]
    if not ports:
        raise SystemExit(__doc__)
    answers = {p: run(p) for p in ports}
    failed = 0
    header = f"{'case':36} {'want':>4} " + " ".join(f"{p:>6}" for p in ports)
    print(header)
    for i, (name, _, _, stream, _, want) in enumerate(CASES):
        cells, ok = [], True
        for p in ports:
            got = answers[p][i] if i < len(answers[p]) else ("-", None)
            status, got_stream = got
            cells.append(f"{status!s:>6}")
            if status != want or got_stream != stream:
                ok = False
        failed += not ok
        print(f"{name:36} {want:>4} " + " ".join(cells) + ("" if ok else "   <-- MISMATCH"))
    print("all servers agree with the spec" if not failed else f"{failed} case(s) differ")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
