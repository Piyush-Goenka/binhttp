#!/usr/bin/env python3
"""Python BH/1 client for interoperability testing.

Usage:  python_client.py [-v] URL [URL ...]

Each URL is host:port[/path], e.g. localhost:8080/index.html or
[::1]:8080/docs/. All URLs must name the same host:port. The client opens
exactly one TCP connection, sends the preface and one REQUEST per URL
(stream IDs 1, 2, 3, ...), pipelined from a second thread while the main
thread reads the responses, and writes each response body to stdout in order.

  -v  hexdump every frame sent and received (and both prefaces) to stderr

Exit status: 0 every response arrived intact with a status below 400;
1 some response was 4xx or 5xx; 2 usage error; 3 connection or protocol
error (3 wins over 1).

BH1_TIMEOUT (seconds, default 30; section 6 says a receiver "MUST allow a frame
at least 5 seconds", so less is raised to 5) bounds the connect, the wait for
the server preface, each wait for the next frame to start, and the rest of
each frame from when the client starts waiting for it.

Standard library only (Python 3.13). Section numbers refer to SPEC.md.
"""

import argparse
import os
import socket
import struct
import sys
import threading
import time
import urllib.parse

PREFACE = b"BH/1"                       # section 1
FRAME_HEADER = struct.Struct(">HBBI")   # section 2: Length 16, Type 8, Flags 8, Stream ID 32
MAX_PAYLOAD = 0xFFFF
MAX_STREAM_ID = 0xFFFFFFFF              # section 4: IDs never wrap
MAX_SKIPPED = 1000                      # section 2: frames skipped in a row before closing (as bcurl)
REQUEST, RESPONSE, DATA = 0x01, 0x02, 0x03
TYPE_NAMES = {REQUEST: "REQUEST", RESPONSE: "RESPONSE", DATA: "DATA"}
END_STREAM = 0x01
GET = 0x01
FIELD_NAMES = {                         # section 5
    1: b"host", 2: b"user-agent", 3: b"accept", 4: b"server", 5: b"date",
    6: b"content-type", 7: b"content-length", 8: b"last-modified",
    9: b"etag", 10: b"allow",
}
HOST, USER_AGENT_FIELD, ACCEPT = 1, 2, 3
USER_AGENT = b"python_client/1.0"

MIN_TIMEOUT = 5.0                       # section 6: "MUST allow a frame at least 5 seconds"
TIMEOUT = max(MIN_TIMEOUT, float(os.environ.get("BH1_TIMEOUT", "30")))

EXIT_OK, EXIT_HTTP_ERROR, EXIT_USAGE, EXIT_PROTOCOL = 0, 1, 2, 3


def complain(message):
    sys.stderr.write(f"python_client: {message}\n")
    sys.stderr.flush()


def printable(raw):
    return "".join(chr(b) if 0x20 <= b < 0x7F and b != 0x5C else f"\\x{b:02x}" for b in raw)


class UsageError(Exception):
    pass


class ConnectionFailed(Exception):
    """A connection error, or frames that no longer line up with our
    requests: nothing more on this connection can be trusted."""


# ------------------------------------------------------------------- URLs

class Target:
    """One command-line URL and the REQUEST it turns into."""

    def __init__(self, url, host, port, authority, path):
        self.url = url
        self.host = host
        self.port = port
        self.authority = authority    # bytes, sent as the host field
        self.path = path              # bytes, sent as the Path


def split_authority(url, authority):
    """host:port or [IPv6]:port. The spec names no default port, so the
    port is required."""
    if authority.startswith("["):
        host, bracket, rest = authority[1:].partition("]")
        if not bracket or not rest.startswith(":"):
            raise UsageError(f"{url}: expected [address]:port")
        port_text = rest[1:]
    else:
        host, colon, port_text = authority.rpartition(":")
        if not colon or ":" in host:
            raise UsageError(f"{url}: expected host:port/path (BH/1 has no default port)")
    if not host:
        raise UsageError(f"{url}: the host is missing")
    if not (port_text.isascii() and port_text.isdigit() and 0 < int(port_text) <= 65535):
        raise UsageError(f"{url}: bad port {port_text!r}")
    return host, int(port_text)


def remove_dot_segments(path):
    """RFC 3986 section 5.2.4 on bytes."""
    rest, out = path, b""
    while rest:
        if rest.startswith(b"../"):
            rest = rest[3:]
        elif rest.startswith(b"./"):
            rest = rest[2:]
        elif rest.startswith(b"/./"):
            rest = rest[2:]
        elif rest == b"/.":
            rest = b"/"
        elif rest.startswith(b"/../") or rest == b"/..":
            rest = b"/" + rest[4:]
            out = out[:out.rfind(b"/")] if b"/" in out else b""
        elif rest in (b".", b".."):
            rest = b""
        else:
            start = 1 if rest.startswith(b"/") else 0
            end = rest.find(b"/", start)
            end = len(rest) if end == -1 else end
            out, rest = out + rest[:end], rest[end:]
    return out


def parse_url(url):
    """Section 3: "A client turns a URL into a Path by dropping the query and
    fragment, percent-decoding, and then resolving dot segments"."""
    if "://" in url:
        raise UsageError(f"{url}: expected host:port/path (BH/1 defines no URL scheme)")
    cut = min([i for i in (url.find(mark) for mark in "/?#") if i != -1], default=len(url))
    authority, rest = url[:cut], url[cut:]
    host, port = split_authority(url, authority)
    rest = rest.partition("#")[0]
    rest, query_mark, _ = rest.partition("?")
    if query_mark:
        complain(f"{url}: ignoring the query; a BH/1 path has none (section 3)")
    decoded = urllib.parse.unquote_to_bytes(os.fsencode(rest or "/"))
    path = remove_dot_segments(decoded)
    if b"\0" in path:
        raise UsageError(f"{url}: the path decodes to a NUL byte, which a Path MUST NOT contain (section 3)")
    return Target(url, host, port, os.fsencode(authority), path)


# ---------------------------------------------------------------- framing

def encode_field(index, value):
    return struct.pack(">BH", index, len(value)) + value


def build_request(stream_id, target):
    """Section 3: Method, Path Length, Path, header fields. Section 4:
    END_STREAM on every REQUEST. The head must fit in one frame."""
    try:
        fields = (encode_field(HOST, target.authority)
                  + encode_field(USER_AGENT_FIELD, USER_AGENT)
                  + encode_field(ACCEPT, b"*/*"))
        payload = struct.pack(">BH", GET, len(target.path)) + target.path + fields
    except struct.error:
        payload = None
    if payload is None or len(payload) > MAX_PAYLOAD:
        raise UsageError(f"{target.url}: too long for one REQUEST frame (section 3)")
    return FRAME_HEADER.pack(len(payload), REQUEST, END_STREAM, stream_id) + payload


def decode_fields(payload, offset):
    """Section 5. Returns [(name, value), ...] in order, skipping reserved
    indexes 11..255. Raises ValueError if the payload ends inside a field or
    a literal name is empty."""
    fields = []
    end = len(payload)
    while offset < end:
        index = payload[offset]
        offset += 1
        name = None
        if index == 0:
            if offset + 2 > end:
                raise ValueError("the payload ends inside a field's name length")
            (name_length,) = struct.unpack_from(">H", payload, offset)
            offset += 2
            if name_length == 0:
                raise ValueError("a literal name is empty")
            if offset + name_length > end:
                raise ValueError("the payload ends inside a field's name")
            name = payload[offset:offset + name_length]
            offset += name_length
        elif index in FIELD_NAMES:
            name = FIELD_NAMES[index]
        if offset + 2 > end:
            raise ValueError("the payload ends inside a field's value length")
        (value_length,) = struct.unpack_from(">H", payload, offset)
        offset += 2
        if offset + value_length > end:
            raise ValueError("the payload ends inside a field's value")
        if name is not None:
            fields.append((name, payload[offset:offset + value_length]))
        offset += value_length
    return fields


def first_value(fields, name):
    """Section 5: when a name repeats, the first one counts."""
    for field_name, value in fields:
        if field_name == name:
            return value
    return None


def parse_response_head(payload):
    if len(payload) < 2:
        raise ValueError("no room for the Status")
    (status,) = struct.unpack_from(">H", payload, 0)
    return status, decode_fields(payload, 2)


def hexdump(data):
    """Same layout as hexdump -C: offset, 16 bytes in two groups of 8, ASCII."""
    lines = []
    for offset in range(0, len(data), 16):
        row = data[offset:offset + 16]
        left = " ".join(f"{b:02x}" for b in row[:8])
        right = " ".join(f"{b:02x}" for b in row[8:])
        text = "".join(chr(b) if 0x20 <= b < 0x7F else "." for b in row)
        lines.append(f"{offset:08x}  {left:<23}  {right:<23}  |{text}|")
    lines.append(f"{len(data):08x}")
    return "\n".join(lines)


def describe(frame_bytes):
    length, frame_type, flags, stream_id = FRAME_HEADER.unpack_from(frame_bytes)
    kind = TYPE_NAMES.get(frame_type, f"type 0x{frame_type:02x} (unknown, skipped)")
    flag_text = f"0x{flags:02x}" + (" END_STREAM" if flags & END_STREAM else "")
    return f"{kind} stream={stream_id} flags={flag_text} length={length}"


# ------------------------------------------------------------- connection

class Connection:
    def __init__(self, sock, verbose):
        self.sock = sock              # the main thread reads from this one
        self.verbose = verbose
        self.buffer = bytearray()
        self.skipped = 0              # frames skipped in a row (section 2)
        self.trace_lock = threading.Lock()
        self.writer = None
        self.closing = threading.Event()

    def trace(self, direction, title, data):
        if self.verbose:
            text = f"{direction} {title}\n{hexdump(data)}\n"
            with self.trace_lock:     # the writer thread traces too
                sys.stderr.write(text)
                sys.stderr.flush()

    def start_writing(self, items):
        """Send the preface and every REQUEST from a second thread, so the
        main thread keeps reading while they go out. Section 4: a client that
        sends a request before the previous response ends "MUST keep reading
        while it writes: a server need not read while it writes".

        The writer has its own duplicate of the socket. A dup shares the
        descriptor's non-blocking flag, which the reader's timeouts set, so
        the writer also uses a timeout (Python then waits for room before
        each send instead of failing with EAGAIN). It may wait as long as the
        server is busy; only the reader decides that the connection failed,
        and then close() stops the writer."""
        out = self.sock.dup()
        out.settimeout(1.0)

        def write():
            try:
                for title, data in items:
                    self.trace(">", title, data)
                    unsent = memoryview(data)
                    while unsent:
                        if self.closing.is_set():
                            return
                        try:
                            unsent = unsent[out.send(unsent):]
                        except TimeoutError:
                            continue      # no room yet: the server is busy writing
            except OSError:
                pass          # the reader sees the same connection fail and reports it
            finally:
                out.close()

        self.writer = threading.Thread(target=write, name="writer", daemon=True)
        self.writer.start()

    def close(self):
        """Close the connection. If the writer is still running (we are giving
        up early), stop it and shut the socket down so it sends nothing more."""
        self.closing.set()
        if self.writer is not None and self.writer.is_alive():
            try:
                self.sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            self.writer.join(2)
        self.sock.close()

    def receive_more(self, deadline, waiting_for):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise ConnectionFailed(f"timed out waiting for {waiting_for}")
        self.sock.settimeout(remaining)
        try:
            chunk = self.sock.recv(65536)
        except TimeoutError:
            raise ConnectionFailed(f"timed out waiting for {waiting_for}") from None
        except OSError as error:
            raise ConnectionFailed(f"connection lost while waiting for {waiting_for}: {error}") from None
        if not chunk:
            raise ConnectionFailed(f"the server closed the connection while we waited for {waiting_for}")
        self.buffer += chunk

    def take(self, count, deadline, waiting_for):
        while len(self.buffer) < count:
            self.receive_more(deadline, waiting_for)
        data = bytes(self.buffer[:count])
        del self.buffer[:count]
        return data

    def check_server_preface(self):
        """Section 1: check the server's first four bytes before reading any
        frame; on anything else, close without sending more."""
        deadline = time.monotonic() + TIMEOUT
        while len(self.buffer) < len(PREFACE) and PREFACE.startswith(self.buffer):
            self.receive_more(deadline, "the server preface")
        got = bytes(self.buffer[:len(PREFACE)])
        self.trace("<", "preface", got)
        if got != PREFACE:
            raise ConnectionFailed(f"the server sent {got!r} instead of the BH/1 preface")
        del self.buffer[:len(PREFACE)]

    def read_frame(self):
        """Wait up to TIMEOUT for a frame to start, then up to TIMEOUT more
        for all of it (section 6: timed from its first byte)."""
        if not self.buffer:
            self.receive_more(time.monotonic() + TIMEOUT, "the next frame")
        deadline = time.monotonic() + TIMEOUT
        header = self.take(FRAME_HEADER.size, deadline, "the rest of a frame header")
        length, frame_type, flags, stream_id = FRAME_HEADER.unpack(header)
        payload = self.take(length, deadline, "the rest of a frame payload")
        self.trace("<", describe(header), header + payload)
        return frame_type, flags, stream_id, payload

    def read_response(self, stream_id, out):
        """Read one response (section 4): a RESPONSE, then DATA until
        END_STREAM (which alone ends it), all on stream_id; skippable frames
        may appear anywhere. The body is copied to `out` as it arrives.
        Returns (status, problems): problems make the response failed but
        leave the connection usable, since the frames themselves were intact.
        Section 6: a client "MUST close on DATA before its RESPONSE, a second
        RESPONSE, or a RESPONSE or DATA frame for a stream it is not reading",
        so those raise ConnectionFailed; other frames are skipped."""
        status = None
        head_seen = head_ok = False
        content_length = None
        received = 0
        problems = []
        while True:
            frame_type, flags, frame_stream, payload = self.read_frame()
            if frame_type not in (RESPONSE, DATA):
                # Unknown types, and REQUEST (meaningless to a client), are
                # skipped wherever they appear, whatever their stream, but
                # not more than 1,000 in a row without a frame we act on
                # (section 2).
                self.skipped += 1
                if self.skipped > MAX_SKIPPED:
                    raise ConnectionFailed(f"more than {MAX_SKIPPED} skipped frames in a row")
                continue
            self.skipped = 0
            kind = TYPE_NAMES[frame_type]
            if frame_stream != stream_id:
                raise ConnectionFailed(
                    f"{kind} for stream {frame_stream} arrived while stream {stream_id} was unfinished")
            if frame_type == RESPONSE:
                if head_seen:
                    raise ConnectionFailed(f"a second RESPONSE arrived on stream {stream_id}")
                head_seen = True
                try:
                    status, fields = parse_response_head(payload)
                except ValueError as error:
                    problems.append(f"malformed RESPONSE: {error}")
                else:
                    head_ok = True
                    self.show_head(status, fields)
                    if not 200 <= status <= 599:
                        problems.append(f"status {status} is not a final status, 200 to 599 (section 3)")
                    declared = first_value(fields, b"content-length")
                    if declared is not None:
                        # Section 5: fail a response whose content-length "is
                        # not all digits" (an empty value counts as not digits).
                        if declared.isdigit():
                            content_length = int(declared)
                        else:
                            problems.append(f"content-length {declared!r} is not all digits")
            else:
                if not head_seen:
                    raise ConnectionFailed(f"DATA arrived on stream {stream_id} before its RESPONSE")
                received += len(payload)
                if head_ok:
                    out.write(payload)
            if flags & END_STREAM:
                break
        if content_length is not None and received != content_length:
            problems.append(f"content-length is {content_length} but the DATA total is {received}")
        return status, problems

    def show_head(self, status, fields):
        if self.verbose:
            lines = [f"<   status: {status}"]
            lines += [f"<   {printable(name)}: {value.decode('ascii', 'backslashreplace')}"
                      for name, value in fields]
            with self.trace_lock:
                sys.stderr.write("\n".join(lines) + "\n")


def fetch_all(host, port, requests, verbose):
    try:
        sock = socket.create_connection((host, port), timeout=TIMEOUT)
    except OSError as error:
        complain(f"cannot connect to {host} port {port}: {error}")
        return EXIT_PROTOCOL
    sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    connection = Connection(sock, verbose)
    out = sys.stdout.buffer
    result, finished = EXIT_OK, 0
    items = [("preface", PREFACE)]
    items += [(f"{describe(data)}  GET {printable(target.path)}", data) for _, target, data in requests]
    try:
        connection.start_writing(items)
        connection.check_server_preface()
        for stream_id, target, _ in requests:
            status, problems = connection.read_response(stream_id, out)
            out.flush()
            finished += 1
            for problem in problems:
                complain(f"{target.url}: {problem}")
            if problems:
                result = EXIT_PROTOCOL
            elif status >= 400:
                complain(f"{target.url}: status {status}")
                result = max(result, EXIT_HTTP_ERROR)
    except ConnectionFailed as error:
        complain(str(error))
        for _, target, _ in requests[finished:]:
            complain(f"{target.url}: failed (no complete response)")
        result = EXIT_PROTOCOL
    finally:
        connection.close()
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(
        prog="python_client.py",
        description="Fetch BH/1 URLs over one connection; bodies go to stdout.")
    parser.add_argument("-v", "--verbose", action="store_true",
                        help="hexdump every frame sent and received to stderr")
    parser.add_argument("urls", nargs="+", metavar="URL", help="host:port[/path]")
    args = parser.parse_args(argv)     # a usage error exits with status 2
    try:
        if len(args.urls) > MAX_STREAM_ID:
            raise UsageError("more URLs than stream IDs; IDs never wrap (section 4)")
        targets = [parse_url(url) for url in args.urls]
        if len({(target.host.lower(), target.port) for target in targets}) != 1:
            raise UsageError("all URLs must share one host:port (they share one connection)")
        requests = [(number, target, build_request(number, target))
                    for number, target in enumerate(targets, start=1)]
    except UsageError as error:
        complain(str(error))
        return EXIT_USAGE
    try:
        return fetch_all(targets[0].host, targets[0].port, requests, args.verbose)
    except BrokenPipeError:
        # stdout was closed early (e.g. piped into head): stop quietly.
        os.dup2(os.open(os.devnull, os.O_WRONLY), sys.stdout.fileno())
        return EXIT_PROTOCOL
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    sys.exit(main())
