# Annotated hexdump: GET /index.html

One complete request and response, every byte accounted for. Captured from a
real run:

```
$ ./bserve ./www 9000
$ ./bcurl -v localhost:9000/index.html
```

`www/index.html` is 62 bytes. The client sent 61 bytes, the server 209. Every
frame begins with the 8-byte header of SPEC.md section 2: Length (2 bytes),
Type (1), Flags (1), Stream ID (4).

## 1. As `bcurl -v` prints it

Lines starting `>` were sent, lines starting `<` received. Each frame is dumped
from its own offset 0, so its header is always the first 8-byte group of its
first line.

```text
* connected to [::1]:9000; every request will use this one connection
> preface "BH/1"
> 00000000  42 48 2f 31                                       |BH/1|
> REQUEST length=49 flags=0x01(END_STREAM) stream=1
>   GET /index.html
>   host: localhost:9000
>   user-agent: bcurl/1.0
>   accept: */*
> 00000000  00 31 01 01 00 00 00 01  01 00 0b 2f 69 6e 64 65  |.1........./inde|
> 00000010  78 2e 68 74 6d 6c 01 00  0e 6c 6f 63 61 6c 68 6f  |x.html...localho|
> 00000020  73 74 3a 39 30 30 30 02  00 09 62 63 75 72 6c 2f  |st:9000...bcurl/|
> 00000030  31 2e 30 03 00 03 2a 2f  2a                       |1.0...*/*|
< preface "BH/1"
< 00000000  42 48 2f 31                                       |BH/1|
< RESPONSE length=127 flags=0x00 stream=1
<   status 200
<   server: bserve/1.0
<   date: Sun, 04 Oct 2026 23:05:10 GMT
<   content-type: text/html; charset=utf-8
<   content-length: 62
<   last-modified: Sun, 04 Oct 2026 23:03:50 GMT
<   etag: "6ac2db56-3e"
< 00000000  00 7f 02 00 00 00 00 01  00 c8 04 00 0a 62 73 65  |.............bse|
< 00000010  72 76 65 2f 31 2e 30 05  00 1d 53 75 6e 2c 20 30  |rve/1.0...Sun, 0|
< 00000020  34 20 4f 63 74 20 32 30  32 36 20 32 33 3a 30 35  |4 Oct 2026 23:05|
< 00000030  3a 31 30 20 47 4d 54 06  00 18 74 65 78 74 2f 68  |:10 GMT...text/h|
< 00000040  74 6d 6c 3b 20 63 68 61  72 73 65 74 3d 75 74 66  |tml; charset=utf|
< 00000050  2d 38 07 00 02 36 32 08  00 1d 53 75 6e 2c 20 30  |-8...62...Sun, 0|
< 00000060  34 20 4f 63 74 20 32 30  32 36 20 32 33 3a 30 33  |4 Oct 2026 23:03|
< 00000070  3a 35 30 20 47 4d 54 09  00 0d 22 36 61 63 32 64  |:50 GMT..."6ac2d|
< 00000080  62 35 36 2d 33 65 22                              |b56-3e"|
< DATA length=62 flags=0x01(END_STREAM) stream=1
< 00000000  00 3e 03 01 00 00 00 01  3c 21 64 6f 63 74 79 70  |.>......<!doctyp|
< 00000010  65 20 68 74 6d 6c 3e 0a  3c 74 69 74 6c 65 3e 42  |e html>.<title>B|
< 00000020  48 2f 31 3c 2f 74 69 74  6c 65 3e 0a 3c 68 31 3e  |H/1</title>.<h1>|
< 00000030  53 65 72 76 65 64 20 6f  76 65 72 20 42 48 2f 31  |Served over BH/1|
< 00000040  3c 2f 68 31 3e 0a                                 |</h1>.|
```

## 2. Client to server, field by field

Indented lines are the payload; the unindented ones are the preface and the
frame header.

```hex client
42 48 2f 31                                         Preface: ASCII "BH/1" (section 1)
00 31                                               Length = 0x0031 = 49 payload bytes after this header
01                                                  Type = 0x01 REQUEST
01                                                  Flags = 0x01 END_STREAM: a v1 request has no body
00 00 00 01                                         Stream ID = 1: the first request on this connection
  01                                                Method = 0x01 GET
  00 0b                                             Path Length = 11
  2f 69 6e 64 65 78 2e 68 74 6d 6c                  Path = "/index.html"
  01                                                field index 1 = host (static table, section 5)
  00 0e                                             Value Length = 14
  6c 6f 63 61 6c 68 6f 73 74 3a 39 30 30 30         Value = "localhost:9000"
  02                                                field index 2 = user-agent
  00 09                                             Value Length = 9
  62 63 75 72 6c 2f 31 2e 30                        Value = "bcurl/1.0"
  03                                                field index 3 = accept
  00 03                                             Value Length = 3
  2a 2f 2a                                          Value = "*/*"
```

## 3. Server to client, field by field

```hex server
42 48 2f 31                                         Preface: ASCII "BH/1", sent without waiting for the client's
00 7f                                               Length = 0x007f = 127 payload bytes
02                                                  Type = 0x02 RESPONSE
00                                                  Flags = 0x00: no END_STREAM, so DATA follows
00 00 00 01                                         Stream ID = 1: copied from the request
  00 c8                                             Status = 0x00c8 = 200
  04                                                field index 4 = server
  00 0a                                             Value Length = 10
  62 73 65 72 76 65 2f 31 2e 30                     Value = "bserve/1.0"
  05                                                field index 5 = date
  00 1d                                             Value Length = 29
  53 75 6e 2c 20 30 34 20 4f 63 74 20 32 30 32 36   Value = "Sun, 04 Oct 2026 23:05:10 GMT"
  20 32 33 3a 30 35 3a 31 30 20 47 4d 54
  06                                                field index 6 = content-type
  00 18                                             Value Length = 24
  74 65 78 74 2f 68 74 6d 6c 3b 20 63 68 61 72 73   Value = "text/html; charset=utf-8"
  65 74 3d 75 74 66 2d 38
  07                                                field index 7 = content-length
  00 02                                             Value Length = 2
  36 32                                             Value = "62": the body is 62 bytes
  08                                                field index 8 = last-modified
  00 1d                                             Value Length = 29
  53 75 6e 2c 20 30 34 20 4f 63 74 20 32 30 32 36   Value = "Sun, 04 Oct 2026 23:03:50 GMT"
  20 32 33 3a 30 33 3a 35 30 20 47 4d 54
  09                                                field index 9 = etag
  00 0d                                             Value Length = 13
  22 36 61 63 32 64 62 35 36 2d 33 65 22            Value = "\"6ac2db56-3e\"": mtime and size in hex
00 3e                                               Length = 0x003e = 62 payload bytes
03                                                  Type = 0x03 DATA
01                                                  Flags = 0x01 END_STREAM: the last frame of the response
00 00 00 01                                         Stream ID = 1
  3c 21 64 6f 63 74 79 70 65 20 68 74 6d 6c 3e 0a   Payload: the 62 bytes of www/index.html
  3c 74 69 74 6c 65 3e 42 48 2f 31 3c 2f 74 69 74
  6c 65 3e 0a 3c 68 31 3e 53 65 72 76 65 64 20 6f
  76 65 72 20 42 48 2f 31 3c 2f 68 31 3e 0a
```

## 4. What the bytes show

- **Skipping needs only the first two bytes.** A receiver that did not know
  type `0x02` would read `00 7f` and discard 127 bytes. Nothing else about the
  frame has to be understood (section 2).
- **The stream ID ties the exchange together.** `00 00 00 01` appears in all
  three frames; the server copied it from the request (section 4).
- **END_STREAM marks where each message ends.** It is set on the REQUEST
  (no body), clear on the RESPONSE (DATA follows), and set on the DATA (last
  frame). No byte count is needed to find the end of the body.
- **Names cost one byte.** `05` stands for `date` and `06` for
  `content-type`. Values carry their length in front, so no byte in a value is
  ever a delimiter and nothing needs escaping.
- **Smaller than text.** The same request in HTTP/1.1 is 86 bytes of text
  against a 57-byte frame here; the response head is 203 bytes against 135.
  The 4-byte preface is paid once per connection.
