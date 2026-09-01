package main

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The download directory can arrive three ways. The most explicit wins:
// a positional argument, then -dir, then HEAPLEACH_DIR.
func TestDownloadDirPrecedence(t *testing.T) {
	envDir := t.TempDir()
	flagDir := t.TempDir()
	argDir := t.TempDir()

	tests := []struct {
		name string
		env  string
		args []string
		want string
	}{
		{"environment only", envDir, nil, envDir},
		{"flag beats environment", envDir, []string{"-dir", flagDir}, flagDir},
		{"argument beats environment", envDir, []string{argDir}, argDir},
		{"argument beats flag", envDir, []string{"-dir", flagDir, argDir}, argDir},
		{"argument alone", "", []string{argDir}, argDir},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				t.Setenv("HEAPLEACH_DIR", t.TempDir()) // never the CWD default
			} else {
				t.Setenv("HEAPLEACH_DIR", tc.env)
			}

			cfg, err := loadConfig(tc.args, io.Discard)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			want, err := filepath.Abs(tc.want)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.DownloadDir != want {
				t.Errorf("dir = %q, want %q", cfg.DownloadDir, want)
			}
		})
	}
}

func TestDirectoryIsCreated(t *testing.T) {
	target := filepath.Join(t.TempDir(), "nested", "downloads")

	cfg, err := loadConfig([]string{target}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	info, err := os.Stat(cfg.DownloadDir)
	if err != nil {
		t.Fatalf("directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("created path is not a directory")
	}
}

func TestOtherFlags(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadConfig([]string{"-addr", ":9999", "-concurrency", "12", "-retries", "7", "-debug", dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9999" {
		t.Errorf("addr = %q", cfg.Addr)
	}
	if cfg.Concurrency != 12 {
		t.Errorf("concurrency = %d", cfg.Concurrency)
	}
	if cfg.MaxRetries != 7 {
		t.Errorf("retries = %d", cfg.MaxRetries)
	}
	if !cfg.Debug {
		t.Error("debug not set")
	}
}

// The byte-count flags read units, print their defaults in units, and
// -min-free exists at all: the floor had been environment-only, which is a
// strange gap for the one setting that decides whether a run fills a disk.
func TestSizeFlagsTakeUnits(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadConfig([]string{"-max-speed", "5MB", "-slow-speed", "750kB", "-min-free", "20GiB", dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SpeedLimit != 5_000_000 {
		t.Errorf("max-speed = %d", cfg.SpeedLimit)
	}
	if cfg.SlowSpeed != 750_000 {
		t.Errorf("slow-speed = %d", cfg.SlowSpeed)
	}
	if cfg.MinFreeDisk != 20<<30 {
		t.Errorf("min-free = %d", cfg.MinFreeDisk)
	}

	cfg, err = loadConfig([]string{"-max-speed", "3000000", "-min-free", "0", dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SpeedLimit != 3_000_000 || cfg.MinFreeDisk != 0 {
		t.Errorf("bare numbers: max-speed %d, min-free %d", cfg.SpeedLimit, cfg.MinFreeDisk)
	}

	var help strings.Builder
	if _, err := loadConfig([]string{"-h"}, &help); err == nil {
		t.Fatal("-h should exit")
	}
	for _, want := range []string{"-min-free", "(default 10GiB)", "(default 2MB)", "HEAPLEACH_MIN_FREE", "HEAPLEACH_STATE"} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("help does not mention %q", want)
		}
	}

	// A value the flag cannot read is refused by the flag package itself,
	// which prints the reason and the usage; the exit status is all that
	// comes back.
	for _, tc := range []struct{ arg, want string }{
		{"-max-speed=fast", "not a size"},
		{"-min-free=-1GB", "negative"},
	} {
		var out strings.Builder
		_, err := loadConfig([]string{tc.arg, dir}, &out)
		if exit, ok := errors.AsType[*exitError](err); !ok || exit.code != 2 {
			t.Errorf("%s: err = %v, want exit status 2", tc.arg, err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: the output does not say %q:\n%s", tc.arg, tc.want, out.String())
		}
	}
}

func TestRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "a-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"two directories", []string{dir, t.TempDir()}, "at most one"},
		{"concurrency too high", []string{"-concurrency", "99", dir}, "between 1 and"},
		{"concurrency zero", []string{"-concurrency", "0", dir}, "between 1 and"},
		{"negative retries", []string{"-retries", "-1", dir}, "negative"},
		{"path is a file", []string{notADir}, "not a directory"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HEAPLEACH_DIR", dir)
			_, err := loadConfig(tc.args, io.Discard)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestHelpAndVersionExitCleanly(t *testing.T) {
	for _, arg := range []string{"-h", "-version"} {
		t.Run(arg, func(t *testing.T) {
			t.Setenv("HEAPLEACH_DIR", t.TempDir())
			_, err := loadConfig([]string{arg}, io.Discard)

			var exit *exitError
			if !errors.As(err, &exit) {
				t.Fatalf("err = %v, want an exitError", err)
			}
			if exit.code != 0 {
				t.Errorf("exit code = %d, want 0", exit.code)
			}
		})
	}
}

func TestTildeIsExpanded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HEAPLEACH_DIR", t.TempDir())

	cfg, err := loadConfig([]string{"~/media/clips"}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := filepath.Join(home, "media", "clips")
	if cfg.DownloadDir != want {
		t.Errorf("dir = %q, want %q", cfg.DownloadDir, want)
	}
}

// A bound listener's address is not always browsable: the wildcard forms
// have to become something a browser will accept.
func TestServiceURL(t *testing.T) {
	tests := []struct{ addr, want string }{
		{"127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"0.0.0.0:8080", "http://localhost:8080"},
		{"[::]:8080", "http://localhost:8080"},
		{"192.168.1.5:9000", "http://192.168.1.5:9000"},
		{"[::1]:9000", "http://[::1]:9000"},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			if got := serviceURL(fakeAddr(tc.addr)); got != tc.want {
				t.Errorf("serviceURL(%s) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

// A port of 0 must come back as the port the kernel actually assigned.
func TestPortZeroReportsTheRealPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	url := serviceURL(ln.Addr())
	_, port, err := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatalf("unparseable url %q: %v", url, err)
	}
	if port == "0" || port == "" {
		t.Fatalf("url %q still reports port 0", url)
	}
	if _, err := net.Dial("tcp", strings.TrimPrefix(url, "http://")); err != nil {
		t.Errorf("nothing is listening on the reported address: %v", err)
	}
}

// fakeAddr is a net.Addr with a fixed string form.
type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

// Positional arguments mix URLs and one download directory in either order,
// because "heapleach URL ~/Videos" and "heapleach ~/Videos URL" are both natural.
func TestPositionalURLsAndDirectory(t *testing.T) {
	dir := t.TempDir()
	const a = "https://example.com/d/one"
	const b = "http://example.com/d/two"

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"no urls serves the ui", []string{dir}, nil},
		{"dir first", []string{dir, a}, []string{a}},
		{"dir last", []string{a, dir}, []string{a}},
		{"several urls", []string{a, b, dir}, []string{a, b}},
		{"urls without a dir", []string{a}, []string{a}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HEAPLEACH_DIR", dir)
			cfg, err := loadConfig(tc.args, io.Discard)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.DownloadDir != dir {
				t.Errorf("DownloadDir = %q, want %q", cfg.DownloadDir, dir)
			}
			if len(cfg.URLs) != len(tc.want) {
				t.Fatalf("URLs = %v, want %v", cfg.URLs, tc.want)
			}
			for i := range tc.want {
				if cfg.URLs[i] != tc.want[i] {
					t.Errorf("URL %d = %q, want %q", i, cfg.URLs[i], tc.want[i])
				}
			}
		})
	}
}

// Two directories is a typo, not a second destination.
func TestTwoDirectoriesIsAnError(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	if _, err := loadConfig([]string{one, two}, io.Discard); err == nil {
		t.Fatal("expected an error for two download directories")
	}
}
