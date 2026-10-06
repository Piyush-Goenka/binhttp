# Python interoperability notes

This directory contains a Python BH/1 client and server used to test the Go
implementation. Both use the frame format and rules in `SPEC.md`.

## Running the checks

From the project root:

```sh
scripts/interop.sh
interop/selftest.sh
```

The interoperability script runs each Go program against its Python
counterpart and sends the same 19 protocol cases to both servers. The Python
selftest runs 119 checks, including fake-server responses, malformed requests,
pipelining, file handling, timeouts and shutdown.

The Python programs require Python 3.13 or later. They can also be run directly:

```sh
python3 interop/python_server.py ./www 9000
python3 interop/python_client.py -v localhost:9000/index.html
```

## Protocol handling

- Empty files are sent as a RESPONSE with END_STREAM. Clients also accept
  an empty DATA frame carrying END_STREAM after the response head.
- Paths must start with `/` and contain no NUL byte or `.` or `..` segment.
  URL queries and fragments are dropped before decoding and resolving the path.
- Unknown frame types and reserved field indexes are skipped. More than 1,000
  consecutive skipped frames closes the connection.
- Error checks follow the order in section 6: stream 0 or an empty payload
  gives 400; an unknown method gives 501 before its remaining payload is parsed.
- Final status codes must be in the range 200 to 599. A content-length value
  must contain one or more digits and match the total DATA bytes.
- The Python client writes pipelined requests from a separate thread while
  reading responses. Responses are consumed in request order.
- An incorrect preface, incomplete frame or mismatched response stream fails
  the connection. Neither client reconnects automatically.

## File handling

The Python server opens paths relative to an open root-directory descriptor.
It follows relative symbolic links that stay inside the root and rejects
absolute links. Directories serve their `index.html`; missing files and
non-regular files return 404. Files that cannot be read return 403.

Final file opens use O_NONBLOCK so a FIFO does not wait for a writer. Directory
opens use O_SEARCH on macOS or O_PATH on Linux where available. File type and
size are read from the opened file.

## Command-line differences

| Setting | Go programs | Python programs |
| --- | --- | --- |
| URL form | Optional `bh://` prefix; port defaults to 9000 | `host:port/path`; explicit port required |
| Requests | Sequential requests over one connection | Pipelined requests over one connection |
| Server bind address | All interfaces by default | 127.0.0.1 by default |
| Server idle timeout | 15 seconds | 120 seconds |
| Frame timeout | 5 seconds | 10 seconds at the server, 30 at the client |
| Write timeout | 10 seconds at the server | 30 seconds at the server |
| Shutdown drain | Up to 5 seconds | Up to 10 seconds |
| Client error status | 4 for 4xx; 5 for 5xx; 1 for connection/protocol errors | 1 for 4xx/5xx; 3 for connection/protocol errors |

Python timeouts can be configured with `BH1_FRAME_TIMEOUT`, `BH1_IDLE_TIMEOUT`,
`BH1_WRITE_TIMEOUT` and `BH1_TIMEOUT`. Frame timeouts below five seconds are
raised to five seconds. The Go command-line programs reject values below that
minimum.
