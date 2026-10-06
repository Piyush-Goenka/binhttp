// Command bcurl fetches files from a BH/1 server, in the spirit of curl:
//
//	bcurl [-v] [-timeout 10s] host[:port]/path ...
//
// Bodies go to stdout, in order. Every URL is fetched over one connection,
// and bcurl never opens a second. With -v, every frame sent (">") and
// received ("<") is decoded and hexdumped to stderr.
//
// Exit status: 0 when every response is 2xx or 3xx, 4 if the worst was a
// 4xx, 5 if the worst was a 5xx, 1 if the connection or the protocol failed,
// and 2 for a usage error.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"binhttp/internal/client"
	"binhttp/internal/frame"
)

const (
	exitOK        = 0
	exitFailure   = 1
	exitUsage     = 2
	exitClientErr = 4
	exitServerErr = 5
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bcurl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	verbose := fs.Bool("v", false, "hexdump every frame sent and received to stderr")
	timeout := fs.Duration("timeout", 10*time.Second, "how long to wait to connect and for each read or write")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: bcurl [-v] [-timeout 10s] host[:port]/path ...")
		fs.PrintDefaults()
	}

	// Flags may come before or after the URLs, as with curl.
	var urls []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitUsage
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		urls = append(urls, rest[0])
		rest = rest[1:]
	}
	if len(urls) == 0 {
		fs.Usage()
		return exitUsage
	}
	if *timeout < frame.MinFrameTimeout {
		fmt.Fprintf(stderr, "bcurl: -timeout must be at least %v: SPEC.md promises every server that long for a frame\n", frame.MinFrameTimeout)
		return exitUsage
	}

	targets := make([]client.Target, len(urls))
	for i, u := range urls {
		t, err := client.ParseTarget(u)
		if err != nil {
			fmt.Fprintf(stderr, "bcurl: %v\n", err)
			return exitUsage
		}
		if i > 0 && !strings.EqualFold(t.Addr, targets[0].Addr) { // host names ignore case
			fmt.Fprintf(stderr, "bcurl: %s and %s are different servers, and bcurl never opens a second connection\n",
				targets[0].Addr, t.Addr)
			return exitUsage
		}
		targets[i] = t
	}

	opts := client.Options{Timeout: *timeout, UserAgent: "bcurl/1.0"}
	if *verbose {
		opts.Verbose = stderr
	}
	conn, err := client.Dial(targets[0].Addr, targets[0].Host, opts)
	if err != nil {
		fmt.Fprintf(stderr, "bcurl: %v\n", err)
		return exitFailure
	}
	defer conn.Close()

	out := bufio.NewWriter(stdout)
	code := exitOK
	for _, t := range targets {
		res, err := conn.Get(t.Path, out)
		if ferr := out.Flush(); ferr != nil && err == nil {
			err = ferr
		}
		if err != nil {
			// The connection's state is unknown now, so stop rather than
			// send the remaining requests into it.
			fmt.Fprintf(stderr, "bcurl: GET %s: %v\n", t.Path, err)
			return exitFailure
		}
		switch {
		case res.Status >= 500:
			code = max(code, exitServerErr)
		case res.Status >= 400:
			code = max(code, exitClientErr)
		}
		if res.Status >= 400 && !*verbose {
			fmt.Fprintf(stderr, "bcurl: GET %s: status %d\n", t.Path, res.Status)
		}
	}
	return code
}
