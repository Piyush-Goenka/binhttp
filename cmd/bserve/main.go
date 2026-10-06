// Command bserve serves the files under a directory over BH/1:
//
//	bserve [flags] ROOT PORT
//
// Each connection stays open for as many requests as the client sends. A
// missing file is a 404 and a malformed frame a 400, and neither closes the
// connection. Ctrl-C stops accepting, closes idle connections at once and
// gives busy ones up to 5 seconds to finish their current response.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"binhttp/internal/frame"
	"binhttp/internal/server"
)

func main() {
	cfg, err := configure(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bserve: %v\n", err)
		os.Exit(2)
	}

	srv, err := server.New(cfg)
	if err != nil {
		log.Fatalf("bserve: %v", err)
	}
	if err := srv.Listen(); err != nil {
		log.Fatalf("bserve: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-served:
		if err != nil {
			log.Fatalf("bserve: %v", err)
		}
	case sig := <-signals:
		fmt.Fprintf(os.Stderr, "bserve: %v received, shutting down\n", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "bserve: shutdown did not finish cleanly: %v\n", err)
			os.Exit(1)
		}
	}
}

// configure turns the command line into a server configuration.
func configure(args []string, stderr io.Writer) (server.Config, error) {
	cfg := server.DefaultConfig()
	fs := flag.NewFlagSet("bserve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bind := fs.String("bind", "", "address to listen on (default: every interface)")
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", cfg.IdleTimeout,
		"how long a connection may go without a request before it is closed")
	fs.DurationVar(&cfg.FrameTimeout, "frame-timeout", cfg.FrameTimeout,
		"how long a frame may take to arrive once its first byte has")
	fs.DurationVar(&cfg.WriteTimeout, "write-timeout", cfg.WriteTimeout,
		"how long one write may stall before the client is dropped (0 disables)")
	fs.IntVar(&cfg.MaxConns, "max-conns", cfg.MaxConns,
		"most connections served at once; more wait their turn (0: no cap)")
	quiet := fs.Bool("quiet", false, "do not log connections and requests")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: bserve [flags] ROOT PORT")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return cfg, fmt.Errorf("want ROOT and PORT, got %d arguments", fs.NArg())
	}
	// A zero idle timeout would not mean "no timeout": it would close every
	// connection at once. Only the write timeout has an off switch. The frame
	// timeout has a floor because the spec promises peers at least that long.
	if cfg.IdleTimeout <= 0 {
		return cfg, errors.New("-idle-timeout must be more than 0")
	}
	if cfg.FrameTimeout < frame.MinFrameTimeout {
		return cfg, fmt.Errorf("-frame-timeout must be at least %v: SPEC.md promises every peer that long for a frame", frame.MinFrameTimeout)
	}
	if cfg.WriteTimeout < 0 {
		return cfg, errors.New("-write-timeout cannot be negative (0 turns it off)")
	}
	if cfg.MaxConns < 0 {
		return cfg, errors.New("-max-conns cannot be negative (0 means no cap)")
	}
	port, err := strconv.Atoi(fs.Arg(1))
	if err != nil || port < 0 || port > 65535 {
		return cfg, fmt.Errorf("port %q is not a number from 0 to 65535", fs.Arg(1))
	}

	cfg.Root = fs.Arg(0)
	cfg.Addr = net.JoinHostPort(*bind, strconv.Itoa(port))
	if !*quiet {
		cfg.Logger = log.New(stderr, "", log.LstdFlags|log.Lmicroseconds)
	}
	return cfg, nil
}
