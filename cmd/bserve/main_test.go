package main

import (
	"errors"
	"flag"
	"io"
	"testing"
	"time"
)

func TestConfigureTheBriefsCommandLine(t *testing.T) {
	cfg, err := configure([]string{"./www", "9000"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != "./www" || cfg.Addr != ":9000" || cfg.Logger == nil {
		t.Fatalf("got root %q addr %q logger %v", cfg.Root, cfg.Addr, cfg.Logger)
	}
}

func TestConfigureFlags(t *testing.T) {
	cfg, err := configure([]string{"-quiet", "-bind", "127.0.0.1", "-idle-timeout", "2s",
		"-frame-timeout", "7s", "-write-timeout", "0", "site", "8080"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:8080" || cfg.Root != "site" || cfg.Logger != nil ||
		cfg.IdleTimeout != 2*time.Second || cfg.FrameTimeout != 7*time.Second || cfg.WriteTimeout != 0 {
		t.Fatalf("got %+v", cfg)
	}
}

func TestConfigureConnectionCapAndDisabledWriteTimeout(t *testing.T) {
	cfg, err := configure([]string{"-max-conns", "50", "-write-timeout", "0", "www", "9000"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != 50 || cfg.WriteTimeout != 0 {
		t.Fatalf("got max-conns %d write-timeout %v", cfg.MaxConns, cfg.WriteTimeout)
	}
	if cfg, _ := configure([]string{"www", "9000"}, io.Discard); cfg.MaxConns != 1024 {
		t.Fatalf("default max-conns %d, want 1024", cfg.MaxConns)
	}
}

func TestConfigureRejectsBadCommandLines(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"www"},
		{"www", "http"},
		{"www", "70000"},
		{"www", "-1"},
		{"www", "9000", "extra"},
		{"-nosuchflag", "www", "9000"},
		{"-idle-timeout", "0", "www", "9000"},  // would close every connection at once
		{"-frame-timeout", "0", "www", "9000"}, // would drop any frame needing two reads
		{"-frame-timeout", "-1s", "www", "9000"},
		{"-frame-timeout", "4s", "www", "9000"}, // SPEC.md section 6: at least 5 seconds per frame
		{"-write-timeout", "-1s", "www", "9000"},
		{"-max-conns", "-1", "www", "9000"},
	} {
		if _, err := configure(args, io.Discard); err == nil {
			t.Errorf("configure(%q) accepted it", args)
		}
	}
	if _, err := configure([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: err = %v, want flag.ErrHelp", err)
	}
}
