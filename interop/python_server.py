#!/usr/bin/env python3
"""Python BH/1 file server for interoperability testing.

Usage:  python_server.py ROOT PORT [HOST]

Serves the files under ROOT on HOST:PORT. HOST defaults to 127.0.0.1. PORT 0
asks the OS for a free ephemeral port; the chosen port is logged. Each
connection gets its own thread and stays open across requests. One log line
per request goes to stderr. Ctrl-C (or SIGTERM) stops accepting, lets every
connection answer the REQUESTs it has already received (for up to 10 s),
closes the rest and exits; a second Ctrl-C stops waiting.

Timeouts, configurable in seconds through the environment:
  BH1_FRAME_TIMEOUT  the client preface, or the rest of a frame once the
                     server starts waiting for it, must arrive within this long
                     (default 10; section 6 requires at least 5, so less is
                     raised to 5)
  BH1_IDLE_TIMEOUT   close a connection that has sent no REQUEST for this long
                     since its last response (default 120; bserve uses 15)
  BH1_WRITE_TIMEOUT  close a connection whose client stops reading (30)

Standard library only (Python 3.13). Section numbers refer to SPEC.md.
"""

import email.utils
import errno
import mimetypes
import os
import signal
import socket
import stat
import struct
import sys
import threading
import time

PREFACE = b"BH/1"                       # section 1: 42 48 2F 31

FRAME_HEADER = struct.Struct(">HBBI")   # section 2: Length 16, Type 8, Flags 8, Stream ID 32
MAX_PAYLOAD = 0xFFFF                    # Length is 16 bits, so every value is legal
MAX_SKIPPED = 1000                      # section 2: frames skipped in a row before closing (as bserve)

REQUEST, RESPONSE, DATA = 0x01, 0x02, 0x03   # section 3
END_STREAM = 0x01                            # the only flag defined in v1
GET = 0x01                                   # the only method defined in v1

# Section 5: index 0 means a literal name follows; 11..255 are reserved.
FIELD_NAMES = {
    1: b"host", 2: b"user-agent", 3: b"accept", 4: b"server", 5: b"date",
    6: b"content-type", 7: b"content-length", 8: b"last-modified",
    9: b"etag", 10: b"allow",
}
FIELD_INDEX = {name: index for index, name in FIELD_NAMES.items()}

STATUS_TEXT = {400: "Bad Request", 403: "Forbidden", 404: "Not Found",
               500: "Internal Server Error", 501: "Not Implemented"}
NOTHING_HERE = "nothing to serve at this path"
SERVER_SOFTWARE = b"python_server/1.0"

MIN_FRAME_TIMEOUT = 5.0                 # section 6: "MUST allow a frame at least 5 seconds"
REQUESTED_FRAME_TIMEOUT = float(os.environ.get("BH1_FRAME_TIMEOUT", "10"))
FRAME_TIMEOUT = max(MIN_FRAME_TIMEOUT, REQUESTED_FRAME_TIMEOUT)
IDLE_TIMEOUT = float(os.environ.get("BH1_IDLE_TIMEOUT", "120"))
WRITE_TIMEOUT = float(os.environ.get("BH1_WRITE_TIMEOUT", "30"))

# Shutting down (section 4: the server "SHOULD NOT [close] while holding an
# unanswered REQUEST"). Waiting connections check the flag this often, and the
# server waits at most DRAIN_LIMIT for REQUESTs already received to be answered.
SHUTTING_DOWN = threading.Event()
WAIT_SLICE = 0.5
DRAIN_LIMIT = 10.0

# Flags for the one-step lookup in Root.open (section 6). Directories are
# opened for searching only where the OS allows it (O_SEARCH on macOS and the
# BSDs, O_PATH on Linux), so a directory with search but not read permission
# can still be walked through, as it can by path.
if hasattr(os, "O_SEARCH"):
    _DIR_ACCESS = os.O_SEARCH
elif hasattr(os, "O_PATH"):
    _DIR_ACCESS = os.O_PATH
else:
    _DIR_ACCESS = os.O_RDONLY
DIR_FLAGS = _DIR_ACCESS | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC
FILE_FLAGS = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC
# How O_NOFOLLOW refuses a symbolic link: ELOOP (Linux, macOS), ENOTDIR when
# O_DIRECTORY is also set (macOS), EMLINK (FreeBSD).
LINK_ERRNOS = (errno.ELOOP, errno.ENOTDIR, errno.EMLINK)
MAX_SYMLINKS = 40                       # links expanded in one lookup, as Linux allows

_log_lock = threading.Lock()


def log(message):
    """Write one whole line to stderr (threads never interleave inside a line)."""
    line = f"{time.strftime('%Y-%m-%d %H:%M:%S')} {message}\n"
    with _log_lock:
        sys.stderr.write(line)
        sys.stderr.flush()


def printable(raw):
    """Show request bytes in the log as one whitespace-free token: spaces,
    control bytes, backslashes and non-ASCII bytes become \\xNN escapes."""
    return "".join(chr(b) if 0x20 < b < 0x7F and b != 0x5C else f"\\x{b:02x}" for b in raw)


class CloseConnection(Exception):
    """A connection error (section 6): close without answering."""


class Reject(Exception):
    """A stream error (section 6): answer with this status, keep the connection."""

    def __init__(self, status, reason):
        super().__init__(reason)
        self.status = status
        self.reason = reason


# ---------------------------------------------------------------- framing

def frame(frame_type, flags, stream_id, payload=b""):
    return FRAME_HEADER.pack(len(payload), frame_type, flags, stream_id) + payload


class SocketReader:
    """Buffered reads from a socket, each one bounded in time."""

    def __init__(self, sock):
        self.sock = sock
        self.buffer = bytearray()

    def receive_more(self, timeout):
        """Append the next bytes that arrive. Returns False at end of stream;
        raises TimeoutError if nothing arrives within `timeout` seconds."""
        if timeout <= 0:
            raise TimeoutError
        self.sock.settimeout(timeout)
        chunk = self.sock.recv(65536)
        if not chunk:
            return False
        self.buffer += chunk
        return True

    def read_exact(self, count, deadline, what):
        while len(self.buffer) < count:
            try:
                more = self.receive_more(deadline - time.monotonic())
            except TimeoutError:
                raise CloseConnection(f"{what} was not complete in time") from None
            if not more:
                raise CloseConnection(f"the client closed the connection in the middle of {what}")
        data = bytes(self.buffer[:count])
        del self.buffer[:count]
        return data


def read_preface(reader):
    """Section 1: check the client's first four bytes. Gives up at the first
    wrong byte rather than waiting for all four. Section 6 times the preface
    like a frame."""
    deadline = time.monotonic() + FRAME_TIMEOUT
    while len(reader.buffer) < len(PREFACE):
        if not PREFACE.startswith(reader.buffer):
            return False
        if SHUTTING_DOWN.is_set():
            raise CloseConnection("shutting down before the client's preface arrived")
        try:
            if not reader.receive_more(min(deadline - time.monotonic(), WAIT_SLICE)):
                return False
        except TimeoutError:
            if time.monotonic() >= deadline:
                return False
    return reader.read_exact(len(PREFACE), deadline, "the preface") == PREFACE


def read_frame(reader, idle_deadline):
    """Return (type, flags, stream_id, payload), or None if the client closed
    cleanly between frames.

    With nothing buffered, wait for the next frame to start, but not past
    idle_deadline (section 4: the server closes "after no REQUEST for a while
    since its last response") and not once the server is shutting down: no
    byte of a REQUEST is held at that point. The wait is cut into short
    slices so a shutdown is noticed promptly. Once a frame has started, the
    rest must arrive within FRAME_TIMEOUT from now, when the server starts
    waiting for it (section 6); time spent writing responses never counts,
    because this server does not read while it writes."""
    if not reader.buffer:
        while True:
            if SHUTTING_DOWN.is_set():
                raise CloseConnection("shutting down; no REQUEST was waiting")
            remaining = idle_deadline - time.monotonic()
            if remaining <= 0:
                raise CloseConnection(f"idle: no REQUEST for {IDLE_TIMEOUT:g} s since the last response")
            try:
                if not reader.receive_more(min(remaining, WAIT_SLICE)):
                    return None
                break
            except TimeoutError:
                continue
    deadline = time.monotonic() + FRAME_TIMEOUT
    header = reader.read_exact(FRAME_HEADER.size, deadline, "a frame")
    length, frame_type, flags, stream_id = FRAME_HEADER.unpack(header)
    payload = reader.read_exact(length, deadline, "a frame")
    return frame_type, flags, stream_id, payload


# ---------------------------------------------------------- header fields

def encode_field(name, value):
    """Section 5: use the table index when the name has one, else a literal."""
    index = FIELD_INDEX.get(name, 0)
    if index:
        return struct.pack(">BH", index, len(value)) + value
    return struct.pack(">BH", 0, len(name)) + name + struct.pack(">H", len(value)) + value


def decode_fields(payload, offset):
    """Section 5: header fields back to back until the payload ends.
    Returns [(name, value), ...]; fields with reserved indexes 11..255 are
    skipped. A payload that ends inside a field, or an empty literal name,
    is a 400 (section 6, step 3)."""
    fields = []
    end = len(payload)
    while offset < end:
        index = payload[offset]
        offset += 1
        name = None
        if index == 0:
            if offset + 2 > end:
                raise Reject(400, "the payload ends inside a header field's name length")
            (name_length,) = struct.unpack_from(">H", payload, offset)
            offset += 2
            if name_length == 0:
                raise Reject(400, "a literal name is empty")
            if offset + name_length > end:
                raise Reject(400, "the payload ends inside a header field's name")
            name = payload[offset:offset + name_length]
            offset += name_length
        elif index in FIELD_NAMES:
            name = FIELD_NAMES[index]
        # else: a reserved index; read Value Length and skip Value like any other.
        if offset + 2 > end:
            raise Reject(400, "the payload ends inside a header field's value length")
        (value_length,) = struct.unpack_from(">H", payload, offset)
        offset += 2
        if offset + value_length > end:
            raise Reject(400, "the payload ends inside a header field's value")
        if name is not None:
            fields.append((name, payload[offset:offset + value_length]))
        offset += value_length
    return fields


def response_head(status, fields):
    return struct.pack(">H", status) + b"".join(encode_field(n, v) for n, v in fields)


def http_date(timestamp=None):
    """IMF-fixdate (RFC 9110 section 5.6.7), e.g. Sun, 06 Nov 1994 08:49:37 GMT."""
    return email.utils.formatdate(timestamp, usegmt=True).encode("ascii")


# --------------------------------------------------------------- requests

def check_request(stream_id, payload):
    """Section 6, steps 1 to 3. Returns the Path of a well-formed GET;
    raises Reject(400) or Reject(501)."""
    # Step 1.
    if stream_id == 0:
        raise Reject(400, "stream ID 0 is reserved for connection-level frames")
    if not payload:
        raise Reject(400, "the REQUEST payload is empty")
    # Step 2: nothing after the method byte is examined, so a future method
    # may lay out the rest of its payload however it likes (section 3).
    method = payload[0]
    if method != GET:
        raise Reject(501, f"method 0x{method:02x} is not implemented (v1 has only GET, 0x01)")
    # Step 3.
    if len(payload) < 3:
        raise Reject(400, "the payload ends inside the Path Length field")
    (path_length,) = struct.unpack_from(">H", payload, 1)
    if 3 + path_length > len(payload):
        raise Reject(400, "the payload ends inside the path")
    path = payload[3:3 + path_length]
    # Fields are parsed only to validate them: no request field changes what
    # this server sends, and section 5 forbids requiring any.
    decode_fields(payload, 3 + path_length)
    if not path.startswith(b"/"):
        raise Reject(400, "the path does not start with '/'")
    if b"\0" in path:
        raise Reject(400, "the path contains a NUL byte")
    if any(segment in (b".", b"..") for segment in path.split(b"/")):
        raise Reject(400, "the path contains a '.' or '..' segment")
    return path


def reject_for(error):
    """Map an OSError from the lookup to a stream error. Anything unexpected
    is returned unchanged, so raising it becomes a 500."""
    if error.errno in (errno.EACCES, errno.EPERM):
        return Reject(403, "the file exists but cannot be read")
    # ENXIO / EOPNOTSUPP: opening a socket file (Linux / macOS). EILSEQ: a
    # filesystem that only allows UTF-8 names. Neither names a regular file.
    if error.errno in (errno.ENOENT, errno.ENOTDIR, errno.ELOOP, errno.EMLINK, errno.ENAMETOOLONG,
                       errno.EILSEQ, errno.ENXIO, errno.EOPNOTSUPP):
        return Reject(404, NOTHING_HERE)
    return error


def read_link(name, dir_fd):
    """The target of `name` if it is a symbolic link, else None."""
    try:
        return os.readlink(name, dir_fd=dir_fd)
    except OSError:
        return None


class Root:
    """The served directory.

    Section 6: "Resolve and open in one step; check-then-open races." Every
    lookup starts at the root's open descriptor and opens one name at a time
    relative to its parent's descriptor, with O_NOFOLLOW, expanding symbolic
    links by hand. No path is ever checked and then reopened, so entries
    renamed or swapped during a lookup can make it fail but never make it
    leave the root."""

    def __init__(self, path):
        self.real = os.path.realpath(path)    # the root with every symlink resolved
        self.fd = os.open(self.real, DIR_FLAGS)

    def open(self, path):
        """Open what a valid Path names: a regular file, or a directory's
        index.html. Returns (fd, stat, file name). Raises Reject(404) when
        there is nothing to serve, Reject(403) when it cannot be read; any
        other OSError propagates and becomes a 500."""
        pending = [name for name in path.split(b"/") if name]    # "//" means "/"
        # "a file asked for with a trailing /" does not exist (section 6).
        must_be_dir = path.endswith(b"/") and bool(pending)
        stack = [self.fd]       # open directories from the root down
        links = 0
        indexed = False
        try:
            while True:
                if not pending:                  # the name ended at a directory
                    if indexed:
                        raise Reject(404, NOTHING_HERE)   # its index.html is a directory too
                    pending, indexed, must_be_dir = [b"index.html"], True, False
                    continue
                name = pending.pop(0)
                if name == b".":
                    continue
                if name == b"..":                # only from link targets; a valid Path has none
                    if len(stack) == 1:
                        raise Reject(404, NOTHING_HERE)   # above the root: does not exist
                    os.close(stack.pop())
                    continue
                last = not pending
                try:
                    stack.append(os.open(name, DIR_FLAGS, dir_fd=stack[-1]))
                    continue                     # a directory: walk into it
                except OSError as error:
                    if error.errno not in LINK_ERRNOS:
                        raise reject_for(error) from None
                target = read_link(name, stack[-1])
                if target is not None:
                    links += 1
                    if links > MAX_SYMLINKS:
                        raise Reject(404, NOTHING_HERE)   # a loop, or close enough
                    pending = link_names(target) + pending
                    continue
                if not last or must_be_dir:
                    raise Reject(404, NOTHING_HERE)       # a non-directory where a directory is needed
                try:
                    fd = os.open(name, FILE_FLAGS, dir_fd=stack[-1])
                except OSError as error:
                    raise reject_for(error) from None
                info = os.fstat(fd)
                if stat.S_ISREG(info.st_mode):
                    return fd, info, name
                os.close(fd)
                raise Reject(404, NOTHING_HERE)  # anything but a regular file does not exist
        finally:
            for directory in stack[1:]:
                os.close(directory)

def link_names(target):
    """The names to walk for a symbolic link's target, which continues from
    the link's own directory ('..' is handled by the walk and can never climb
    above the root). Section 6 lets a server "treat any absolute link as
    outside", and this one does: whether an absolute target lies inside the
    root would otherwise depend on how the root and the target happen to be
    spelled (/var or /private/var, // or /), so the same tree could answer
    differently depending on where it lives."""
    if target.startswith(b"/"):
        raise Reject(404, NOTHING_HERE)
    return [name for name in target.split(b"/") if name]


def file_fields(info, name):
    guess = getattr(mimetypes, "guess_file_type", mimetypes.guess_type)
    content_type = guess(os.fsdecode(name))[0] or "application/octet-stream"
    return [
        (b"server", SERVER_SOFTWARE),
        (b"date", http_date()),
        (b"content-type", content_type.encode("ascii")),
        (b"content-length", str(info.st_size).encode("ascii")),
        (b"last-modified", http_date(info.st_mtime)),
        (b"etag", f'"{info.st_ino:x}-{info.st_size:x}-{info.st_mtime_ns:x}"'.encode("ascii")),
    ]


# ------------------------------------------------------------- connection

class Connection:
    def __init__(self, sock, peer, root):
        self.sock = sock
        self.root = root
        self.reader = SocketReader(sock)
        host, port = peer[0], peer[1]
        self.peer = f"[{host}]:{port}" if ":" in host else f"{host}:{port}"

    def send(self, data):
        self.sock.settimeout(WRITE_TIMEOUT)
        try:
            self.sock.sendall(data)
        except TimeoutError:
            raise CloseConnection("timed out writing (the client stopped reading)") from None

    def run(self):
        try:
            self.sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            self.send(PREFACE)        # section 1: on accept, not waiting for the client's
            if not read_preface(self.reader):
                log(f"{self.peer} closed: the client did not send the BH/1 preface")
                return                # close without sending more
            idle_deadline = time.monotonic() + IDLE_TIMEOUT
            skipped = 0
            while True:
                received = read_frame(self.reader, idle_deadline)
                if received is None:
                    # The client closed or half-closed between frames. Every
                    # complete REQUEST before that has been answered (section 4).
                    return
                frame_type, _flags, stream_id, payload = received
                if frame_type == REQUEST:
                    # Answered once the frame is complete, END_STREAM or not
                    # (section 4); other flag bits are ignored (section 2).
                    skipped = 0
                    self.answer(stream_id, payload)
                    idle_deadline = time.monotonic() + IDLE_TIMEOUT
                    continue
                # Anything else (unknown types, and RESPONSE or DATA, which
                # mean nothing to a server) is skipped: its payload is already
                # consumed, so the next header starts right here (section 2).
                # Skipped frames do not count as activity, and only so many in
                # a row are allowed (section 2).
                skipped += 1
                if skipped > MAX_SKIPPED:
                    raise CloseConnection(f"more than {MAX_SKIPPED} skipped frames in a row")
        except (CloseConnection, OSError) as error:
            log(f"{self.peer} closed: {error}")
        except Exception as error:   # a bug: drop this connection, keep the server
            log(f"{self.peer} closed after an internal error: {error!r}")
        finally:
            self.sock.close()

    def answer(self, stream_id, payload):
        """Answer one REQUEST on its own stream with the first section 6
        step that applies: 400, 501, 400, 404, then 403 or 500."""
        method = payload[0] if payload else None
        path = status = fd = None
        body_size, outcome = 0, "(failed while sending; connection closed)"
        try:
            try:
                path = check_request(stream_id, payload)
                fd, info, name = self.root.open(path)
                head = response_head(200, file_fields(info, name))
            except Reject as rejection:
                status = rejection.status
                body_size = self.send_error(stream_id, rejection.status, rejection.reason)
            except Exception as error:   # anything else, before the response started
                status = 500
                log(f"{self.peer} stream={stream_id} internal error: {error!r}")
                body_size = self.send_error(stream_id, 500, "the server failed before it started the response")
            else:
                status = 200
                size = info.st_size
                # An empty body is just a RESPONSE carrying END_STREAM (section 4).
                self.send(frame(RESPONSE, 0 if size else END_STREAM, stream_id, head))
                body_size = self.send_body(stream_id, fd, size)
            outcome = ""
        finally:
            if fd is not None:
                os.close(fd)
            method_text = "-" if method is None else ("GET" if method == GET else f"method=0x{method:02x}")
            path_text = "-" if path is None else printable(path)
            log(f"{self.peer} stream={stream_id} {method_text} {path_text} -> {status} "
                f"{body_size} bytes{' ' + outcome if outcome else ''}")

    def send_error(self, stream_id, status, reason):
        """A short text/plain body saying what was wrong (section 6)."""
        body = f"{status} {STATUS_TEXT[status]}: {reason}\n".encode("ascii")
        fields = [(b"server", SERVER_SOFTWARE), (b"date", http_date()),
                  (b"content-type", b"text/plain"),
                  (b"content-length", str(len(body)).encode("ascii"))]
        if status == 501:
            fields.append((b"allow", b"GET"))
        self.send(frame(RESPONSE, 0, stream_id, response_head(status, fields))
                  + frame(DATA, END_STREAM, stream_id, body))
        return len(body)

    def send_body(self, stream_id, fd, size):
        """Send exactly `size` bytes as DATA frames, END_STREAM on the last.
        If the file shrinks under us the status cannot be taken back, so the
        connection is closed instead (section 6)."""
        sent = 0
        while sent < size:
            chunk = os.read(fd, min(MAX_PAYLOAD, size - sent))
            if not chunk:
                raise CloseConnection(f"file shrank while being sent ({sent} of {size} bytes)")
            sent += len(chunk)
            self.send(frame(DATA, END_STREAM if sent == size else 0, stream_id, chunk))
        return sent


# ------------------------------------------------------------------- main

def _stop(_signum, _frame):
    raise KeyboardInterrupt


CONNECTION_THREAD = "bh1-connection"


def busy_connections():
    return [thread for thread in threading.enumerate() if thread.name == CONNECTION_THREAD]


def drain(limit):
    """Wait up to `limit` seconds for every connection to answer what it has
    already received and close (section 4: the server "SHOULD NOT [close]
    while holding an unanswered REQUEST"). Returns the ones still running."""
    deadline = time.monotonic() + limit
    for thread in busy_connections():
        thread.join(max(0.0, deadline - time.monotonic()))
    return busy_connections()


def main(argv):
    usage = "usage: python_server.py ROOT PORT [HOST]\n"
    if len(argv) not in (3, 4):
        sys.stderr.write(usage)
        return 2
    if not os.path.isdir(argv[1]):
        sys.stderr.write(f"python_server: {argv[1]} is not a directory\n")
        return 2
    if not (argv[2].isascii() and argv[2].isdigit() and int(argv[2]) <= 65535):
        sys.stderr.write(usage)
        return 2
    try:
        root = Root(os.fsencode(argv[1]))
    except OSError as error:
        sys.stderr.write(f"python_server: cannot open {argv[1]}: {error}\n")
        return 2
    host = argv[3] if len(argv) == 4 else "127.0.0.1"
    mimetypes.init()   # load the type map once, before any thread uses it
    try:
        family = socket.AF_INET6 if ":" in host else socket.AF_INET
        listener = socket.create_server((host, int(argv[2])), family=family)
    except OSError as error:
        sys.stderr.write(f"python_server: cannot listen on {host} port {argv[2]}: {error}\n")
        return 1
    signal.signal(signal.SIGTERM, _stop)
    try:
        if REQUESTED_FRAME_TIMEOUT < MIN_FRAME_TIMEOUT:
            log(f"BH1_FRAME_TIMEOUT raised to {MIN_FRAME_TIMEOUT:g} s, the minimum in section 6")
        log(f"listening on {host} port {listener.getsockname()[1]}, serving {os.fsdecode(root.real)}")
        while True:
            try:
                sock, peer = listener.accept()
            except OSError as error:   # e.g. out of file descriptors: back off briefly
                log(f"accept failed: {error}")
                time.sleep(0.1)
                continue
            threading.Thread(target=Connection(sock, peer, root).run, name=CONNECTION_THREAD,
                             daemon=True).start()
    except KeyboardInterrupt:
        SHUTTING_DOWN.set()
        listener.close()
        log("shutting down: answering the REQUESTs already received, then closing")
        try:
            left = drain(DRAIN_LIMIT)
        except KeyboardInterrupt:          # a second Ctrl-C: stop waiting
            left = busy_connections()
        log("stopped" if not left else f"stopped, dropping {len(left)} connection(s) still busy")
    finally:
        listener.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
