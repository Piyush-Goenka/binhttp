# BH/1: HTTP in binary

Version 1: everything needed to write a client or a server that talks to
`bcurl` and `bserve` (`HEXDUMP.md` annotates one exchange byte by byte).
Keywords are as in RFC 2119; integers are unsigned big-endian. A client sends
requests over one TCP connection, which the server keeps open and answers in
order: a request is one REQUEST frame, a response one RESPONSE frame then zero
or more DATA frames.

## 1. Preface

Each side's first four bytes are `42 48 2F 31` (ASCII `BH/1`): the client's
before its first frame, the server's on accept, neither waiting for the
other's. Each checks the other's before reading frames and MUST close,
sending nothing more, on any other bytes: a wrong-protocol peer is caught in
four bytes, not read as a frame (as a header, HTTP/1.1's `GET / HT` claims an
18,245-byte payload).

## 2. Frame header

```
+-------------------------------+---------------+---------------+
|          Length (16)          |   Type (8)    |   Flags (8)   |
+-------------------------------+---------------+---------------+
|                        Stream ID (32)                         |
+---------------------------------------------------------------+
```

**Length** is the payload size, 0 to 65,535 bytes, after the header. **Type**
says what the payload is (section 3) and **Stream ID** which request it belongs
to (section 4). In **Flags**, `0x01` END_STREAM marks a message's last frame;
senders MUST send the other bits as 0 and receivers MUST ignore them.

**A receiver meeting a frame type it does not know MUST skip it cleanly:
discard exactly Length payload bytes and read the next header.** The same goes
for a known type that means nothing to the receiver, such as DATA at a server.
Skippable frames may come anywhere, even between a RESPONSE and its DATA. A
receiver MAY close after skipping too many in a row without one it acts on
(`bcurl`, `bserve`: over 1,000), against floods like HTTP/2's (CVE-2019-9518).

**Why 16/8/8/32, against HTTP/2's 24/8/8/31.** *Length 16:* HTTP/2 needs 24
bits because SETTINGS can raise its frame limit to 16 MiB, which costs it a
negotiated maximum and a "frame too large" error. Without a handshake, BH/1's
field width is the limit: every value is legal, that error cannot exist, no
receiver buffers over 64 KiB + 8, and bodies span DATA frames at 0.012%
overhead. *Type 8, Flags 8:* as HTTP/2; 253 types and 7 flags remain for
version 2. *Stream ID 32:* ties each response to its request, so a desync is
detected rather than misread, and lets v2 multiplex without a new header.
HTTP/2's reserved bit is a SPDY leftover; 2^32 IDs outlast a connection a
client never replaces (49 days at 1,000 requests a second). *8 bytes:* every
field aligned, one `hexdump -C` group per header.

## 3. Frame types

| Type | Name | Sent by | Payload |
|---|---|---|---|
| `0x01` | REQUEST | client | Method (8), Path Length (16), Path, header fields |
| `0x02` | RESPONSE | server | Status (16), header fields |
| `0x03` | DATA | server | body bytes |

Every other type is reserved, and skipped. **Method** `0x01` is GET, the only
v1 method. **Status** is a final HTTP status, 200 to 599: v1 has no interim
responses. **Path** is the bytes of a file name, not a URL: it MUST start with
`/` and MUST NOT contain a NUL byte or a `.` or `..` segment. Every other byte
is literal (`%`, `?`, `#`, `\` and spaces are part of the name), and `//` means
`/`. A client turns a URL into a Path by dropping the query and fragment,
percent-decoding, then resolving dot segments (RFC 3986 §5.2.4), and refuses a
URL whose Path would still break these rules. For example, GET `/a` with
`host: h` on stream 1 is `00 09 01 01 00 00 00 01`
`01 00 02 2F 61 01 00 01 68`, and a 200 with no fields and no body is
`00 02 02 01 00 00 00 01` `00 C8`.

A request or response head MUST fit in one frame: no CONTINUATION frame, so
no 2024-style "CONTINUATION flood". Fixed slots, not pseudo-headers, mean
method, path and status cannot be missing, repeated or misplaced. New methods
get new method values, never new frame types: a skipped request would leave its
client waiting forever, while an unknown method gets a 501. Hence **a new frame
type may only carry what an older peer can ignore.**

## 4. Streams and order

- The client gives each request a new stream ID, nonzero and larger than its
  last (1, 2, 3, ...); IDs never wrap, so after `0xFFFFFFFF` it closes. Servers
  do not check the order. Stream 0 is reserved for connection-level frames.
  The server copies the request's stream ID onto its RESPONSE and DATA frames,
  even when rejecting it (stream 0 included).
- **END_STREAM alone ends a message**, never the status or content-length.
  Requests have no body: a client MUST set END_STREAM on every REQUEST, and a
  server answers a complete REQUEST either way.
- A body travels in DATA frames (any may be empty), the last carrying
  END_STREAM. An empty body SHOULD be a bare RESPONSE with END_STREAM; clients
  MUST also accept one empty DATA frame.
- A client MAY send a request before the previous response ends, but then MUST
  keep reading while it writes, since a server need not read while it writes.
  Responses come in request order and are never interleaved.
- The server keeps the connection open: beyond section 6's errors it closes
  only to shut down or after no REQUEST for a while since its last response
  (`bserve`: 15 s), and SHOULD NOT while holding an unanswered REQUEST. A
  client MAY close between messages; after a half-close the server MUST answer
  every complete REQUEST that arrived first. A request never fully answered MAY
  be retried (GET is safe).

## 5. Header fields

After the fixed fields, a REQUEST or RESPONSE payload holds header fields back
to back until it ends:

```
Index (8) | only if Index = 0: Name Length (16), Name | Value Length (16) | Value
```

Index 1 to 10 names a table entry; index 0 means a literal name of at least 1
byte follows; index 11 to 255 is reserved for future entries, and a receiver
MUST skip such a field by its Value Length, so the table can grow without
breaking v1. Senders MUST use lowercase ASCII names. Receivers compare names
byte for byte (a literal `host` equals index 1; `Host` does not), ignore names
they do not use, take the first of a repeated one, and MUST NOT require any.
This is HPACK's static table and length-prefixed literals, without its dynamic
table or Huffman code. The table holds exactly the ten names `bcurl` and
`bserve` send (clients 1 to 3, servers 4 to 10):

| # | Name | Value | # | Name | Value |
|---|---|---|---|---|---|
| 1 | `host` | the URL's authority | 6 | `content-type` | media type |
| 2 | `user-agent` | e.g. `bcurl/1.0` | 7 | `content-length` | body size, in digits |
| 3 | `accept` | `*/*` | 8 | `last-modified` | IMF-fixdate |
| 4 | `server` | e.g. `bserve/1.0` | 9 | `etag` | quoted validator |
| 5 | `date` | IMF-fixdate (RFC 9110) | 10 | `allow` | `GET`, with every 501 |

A server SHOULD send `content-length`; a client MUST fail a response whose
`content-length` is not one or more digits or differs from the DATA total.

## 6. Errors

**Stream errors: answer and keep the connection.** Every 8-byte header is
valid, so the next frame starts exactly Length bytes on and bad payload bytes
cannot desynchronise the stream: a 400 can keep the connection where HTTP/1.1
must close (RFC 9112 §6.3). The server answers on the request's stream, with a
short `text/plain` explanation, by the first rule that applies:

1. **400** if the stream ID is 0 or the payload is empty.
2. **501** if the method is not GET, with `allow: GET`. Nothing after the
   method byte is examined.
3. **400** if the GET payload ends inside a field, a literal name is empty, or
   the path breaks section 3.
4. **404** if there is nothing to serve. A directory is served as its
   `index.html`; anything but a regular file, a file asked for with a trailing
   `/`, and anything outside the root, even via a symbolic link, does not exist
   (absolute links SHOULD count as outside). Resolve and open in one step,
   without blocking; check-then-open races.
5. **403** if the file exists but cannot be read; **500** if the server failed
   before it started the response.

**Connection errors: close without answering.** A wrong preface; the peer
closing mid-frame; a client not reading its responses for a while (`bserve`:
10 s); too many skipped frames; a frame (or preface) late. A receiver MUST
allow a frame at least 5 seconds from when it starts waiting for the rest; time
spent writing its own responses does not count. A server failing mid-body
cannot take back its status, so it closes; a client that loses the connection
before END_STREAM MUST treat the response as failed. A client MUST close on
DATA before its RESPONSE, a second RESPONSE, or a RESPONSE or DATA frame for a
stream it is not reading: its place in the stream is lost.

## 7. Room for version 2

Unused: types `0x00` and `0x04` to `0xFF`, flags `0x02` to `0x80`, stream 0's
connection-level frames, field indexes 11 to 255, methods but `0x01`. A v2 peer
still opens with `BH/1`, and spots a v1 peer for free with a HELLO frame before
its first REQUEST: a v2 server answers it first, a v1 server skips it.
