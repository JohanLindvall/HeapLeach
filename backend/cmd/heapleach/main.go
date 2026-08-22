// Command heapleach serves the bulk downloader: a JSON API and the embedded
// TypeScript frontend, backed by a parallel download manager.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/JohanLindvall/HeapLeach/internal/cli"
	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/download"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/server"
	"github.com/JohanLindvall/HeapLeach/internal/webui"
)

// version is stamped at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		// The CLI has already explained itself for -h, -version and bad
		// flags; anything else gets the usual one-line report.
		var exit *exitError
		if errors.As(err, &exit) {
			os.Exit(exit.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// exitError ends the process with a status code and no further output.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func run() error {
	cfg, err := loadConfig(os.Args[1:], os.Stderr)
	if err != nil {
		return err
	}
	headless := len(cfg.URLs) > 0
	log := newLogger(cfg.Debug, headless)

	// Signals are handled the same either way: the manager is closed, and
	// partial files stay on disk for the next run to resume.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), cfg.MaxRetries, cfg.Timeout)
	registry := extractor.NewRegistry(cfg, client)

	manager := download.New(cfg, registry, client, log)
	manager.Start()
	// Close is idempotent; this covers the error paths below.
	defer manager.Close()

	// URLs on the command line mean "download these and quit" — no server,
	// no browser, progress on the terminal.
	if headless {
		err := runHeadless(ctx, cfg, manager)
		switch {
		case errors.Is(err, cli.ErrIncomplete):
			// The display already named every file that failed.
			return &exitError{code: 1}
		case errors.Is(err, context.Canceled):
			// Interrupted; conventionally 128 + SIGINT.
			return &exitError{code: 130}
		}
		return err
	}

	assets, err := webui.FS()
	if err != nil {
		return fmt.Errorf("load frontend: %w", err)
	}
	if !webui.Built() {
		log.Warn("frontend is a placeholder — run `make frontend` to build the UI")
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(cfg, manager, log, assets).Handler(),
		ReadHeaderTimeout: config.ReadHeaderTimeout,
		// No WriteTimeout: /api/events is a long-lived stream.
		IdleTimeout: config.IdleTimeout,
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// Bind before serving so a port of 0 — "any free port" — can be
	// reported and opened. Only the process that binds knows what it got,
	// so choosing a port any earlier would be a guess that could race.
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	url := serviceURL(listener.Addr())

	log.Info("starting", "version", version, "url", url,
		"dir", cfg.DownloadDir, "concurrency", cfg.Concurrency,
		"hosts", registry.Hosts())

	errs := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	if cfg.OpenBrowser {
		if err := openBrowser(url); err != nil {
			log.Warn("could not open a browser", "url", url, "err", err)
		}
	}

	select {
	case err := <-errs:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Stop the manager first. http.Server.Shutdown waits for in-flight
	// requests, and it does not cancel their contexts — so the long-lived
	// /api/events streams would keep it waiting until the deadline. Closing
	// the manager ends those streams (and aborts transfers, whose .part
	// files stay on disk for the next run to resume).
	manager.Close()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// serviceURL renders a browsable URL for a bound listener, turning the
// wildcard addresses into something a browser will actually accept.
func serviceURL(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "http://" + addr.String()
	}
	switch host {
	case "", "::", "0.0.0.0":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// openBrowser hands the URL to the desktop. Failure is never fatal: the URL
// has already been logged, so the user can open it themselves.
func openBrowser(url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}
	return exec.Command(name, append(args, url)...).Start()
}

// newLogger builds a text logger.
//
// A headless run owns stdout — the progress display repaints it — so its log
// goes to stderr, and only for things the display cannot show. Asking for
// -debug turns the animation off, so the two never compete.
func newLogger(debug, headless bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	out := io.Writer(os.Stdout)
	if headless {
		out = os.Stderr
		if !debug {
			level = slog.LevelWarn
		}
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))
}

// runHeadless downloads the URLs the command line supplied and returns when
// the last one has finished.
func runHeadless(ctx context.Context, cfg *config.Config, manager *download.Manager) error {
	// -debug interleaves log lines with the display, so plain progress
	// lines are the only readable option there.
	animate := cli.IsTerminal(os.Stdout) && !cfg.Debug
	width := config.CLIDefaultWidth
	if animate {
		if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
			width = w
		}
	}
	return cli.Run(ctx, manager, cli.Options{
		URLs:     cfg.URLs,
		Password: cfg.Password,
		Out:      os.Stdout,
		Animate:  animate,
		Colour:   animate && os.Getenv("NO_COLOR") == "",
		Width:    width,
	})
}

// loadConfig layers command-line flags over the environment.
//
// The download directory can be given three ways; the most explicit wins:
// a positional argument, then -dir, then DOWN_DIR. Flags default to the
// environment's values so `DOWN_DIR=... heapleach` keeps working unchanged.
func loadConfig(args []string, out io.Writer) (*config.Config, error) {
	cfg, err := config.FromEnv()
	if err != nil {
		return nil, err
	}

	flags := flag.NewFlagSet("heapleach", flag.ContinueOnError)
	flags.SetOutput(out)
	flags.Usage = func() { usage(flags, cfg) }

	var showVersion bool
	flags.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address")
	flags.StringVar(&cfg.DownloadDir, "dir", cfg.DownloadDir, "directory to download into")
	flags.IntVar(&cfg.Concurrency, "concurrency", cfg.Concurrency,
		fmt.Sprintf("parallel transfers (1-%d)", config.MaxConcurrency))
	flags.IntVar(&cfg.MaxRetries, "retries", cfg.MaxRetries, "retries per request and per transfer")
	flags.IntVar(&cfg.Streams, "streams", cfg.Streams,
		fmt.Sprintf("connections to split a slow file across (1-%d)", config.MaxStreams))
	flags.Int64Var(&cfg.SlowSpeed, "slow-speed", cfg.SlowSpeed,
		"bytes per second below which extra connections are opened")
	flags.Int64Var(&cfg.SpeedLimit, "max-speed", cfg.SpeedLimit,
		"total download rate ceiling in bytes per second (0 is unlimited)")
	flags.DurationVar(&cfg.StallTimeout, "stall-timeout", cfg.StallTimeout,
		"abandon and retry a transfer that makes no progress for this long")
	flags.BoolVar(&cfg.Debug, "debug", cfg.Debug, "verbose logging")
	flags.BoolVar(&cfg.OpenBrowser, "open", cfg.OpenBrowser, "open the UI in a browser once it is listening")
	flags.BoolVar(&showVersion, "version", false, "print the version and exit")
	var password string
	flags.StringVar(&password, "password", "", "password for protected sources (headless downloads)")

	if err := flags.Parse(args); err != nil {
		// Parse has already reported the problem and printed the usage.
		if errors.Is(err, flag.ErrHelp) {
			return nil, &exitError{code: 0}
		}
		return nil, &exitError{code: 2}
	}

	if showVersion {
		fmt.Fprintln(out, "heapleach", version)
		return nil, &exitError{code: 0}
	}

	// Positional arguments are URLs to download, plus at most one download
	// directory. A URL is anything with an http(s) scheme, which is also
	// the only thing the extractors can fetch, so the split is unambiguous
	// in both orders: `heapleach ~/Videos URL` and `heapleach URL ~/Videos`.
	var dirs []string
	for _, arg := range flags.Args() {
		switch {
		case cli.IsURL(arg):
			cfg.URLs = append(cfg.URLs, arg)
		case arg == "-":
			urls, err := cli.ReadURLs(os.Stdin)
			if err != nil {
				return nil, fmt.Errorf("read URLs from stdin: %w", err)
			}
			cfg.URLs = append(cfg.URLs, urls...)
		default:
			dirs = append(dirs, arg)
		}
	}
	switch len(dirs) {
	case 0:
	case 1:
		cfg.DownloadDir = dirs[0]
	default:
		return nil, fmt.Errorf("expected at most one download directory, got %d: %s",
			len(dirs), strings.Join(dirs, " "))
	}
	cfg.Password = password

	if err := cfg.Prepare(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// usage prints the help text, showing the values the environment supplies as
// the defaults the user would actually get.
func usage(flags *flag.FlagSet, cfg *config.Config) {
	out := flags.Output()
	fmt.Fprintf(out, `heapleach — parallel bulk downloader

Usage:
  heapleach [options] [download-dir]              serve the web UI
  heapleach [options] <url>... [download-dir]     download and exit

With no URLs heapleach serves its web UI. Given URLs it downloads them to
disk, animating progress on the terminal, and exits non-zero if any
file failed. A "-" argument reads URLs from standard input, one per
line (blank lines and #comments are ignored).

Files are saved to the download directory, which may be given as an
argument, with -dir, or through DOWN_DIR. The argument wins.
Currently: %s

Options:
`, cfg.DownloadDir)
	flags.PrintDefaults()
	fmt.Fprint(out, `
Environment:
  DOWN_ADDR, DOWN_DIR, DOWN_CONCURRENCY, DOWN_MAX_RETRIES, DOWN_STREAMS,
  DOWN_SLOW_SPEED, DOWN_MAX_SPEED, DOWN_STALL_TIMEOUT, DOWN_DEBUG, DOWN_OPEN,
  DOWN_USER_AGENT, DOWN_LANGUAGE, DOWN_GOFILE_SECRET

Examples:
  heapleach ~/Downloads
  heapleach -concurrency 8 -addr :9000 /mnt/media
  heapleach https://example.com/d/abc123 ~/Downloads
  heapleach -streams 8 https://example.com/a/one https://example.com/a/two
  cat urls.txt | heapleach - ~/Downloads
`)
}
