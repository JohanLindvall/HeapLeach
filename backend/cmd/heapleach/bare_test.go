package main

import (
	"io"
	"strings"
	"testing"
)

// Run on its own, heapleach should need no decisions from the user: a free
// port on this machine, and the browser opened at it.
func TestBareInvocationTakesAFreePortAndOpens(t *testing.T) {
	t.Setenv("HEAPLEACH_DIR", t.TempDir())

	cfg, err := loadConfig(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:0" {
		t.Errorf("addr = %q, want a free port on loopback", cfg.Addr)
	}
	if !cfg.OpenBrowser {
		t.Error("a bare invocation should open the browser: nothing else can know the port")
	}
}

// Any argument is a choice, and guessing over a choice would be worse than
// the fixed default ever was.
func TestAnyArgumentRestoresTheExplicitDefaults(t *testing.T) {
	dir := t.TempDir()

	for _, args := range [][]string{
		{dir},
		{"-concurrency", "2"},
		{"-open"},
	} {
		t.Setenv("HEAPLEACH_DIR", dir)
		cfg, err := loadConfig(args, io.Discard)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if cfg.Addr != ":8080" {
			t.Errorf("%v: addr = %q, want the ordinary default", args, cfg.Addr)
		}
	}

	// -open on its own still means what it says, without also moving the
	// address.
	t.Setenv("HEAPLEACH_DIR", dir)
	cfg, err := loadConfig([]string{"-open"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.OpenBrowser {
		t.Error("-open was ignored")
	}
}

// The container image sets an address, and must go on binding the port it
// maps rather than hiding on its own loopback where nothing can reach it.
func TestEnvironmentAddressWinsOverTheBareDefault(t *testing.T) {
	t.Setenv("HEAPLEACH_DIR", t.TempDir())
	t.Setenv("HEAPLEACH_ADDR", ":8080")

	cfg, err := loadConfig(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("addr = %q, want the address the environment asked for", cfg.Addr)
	}
	if cfg.OpenBrowser {
		t.Error("an environment that names an address is a deployment, not a desktop run")
	}
}

// A bare run is server mode, so nothing about it should look headless.
func TestBareInvocationIsNotHeadless(t *testing.T) {
	t.Setenv("HEAPLEACH_DIR", t.TempDir())

	cfg, err := loadConfig(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.URLs) != 0 {
		t.Errorf("URLs = %v, want none", cfg.URLs)
	}
}

func TestUsageMentionsTheBareForm(t *testing.T) {
	var out strings.Builder
	if _, err := loadConfig([]string{"-h"}, &out); err == nil {
		t.Fatal("-h should stop the program")
	}
	if !strings.Contains(out.String(), "serve the UI and open it") {
		t.Errorf("usage does not document the bare invocation:\n%s", out.String())
	}
}
