// Package config holds the process-wide settings, resolved from the
// environment and then from command-line flags, with sensible defaults for
// both container and local runs.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultUserAgent is a current, ordinary desktop Chrome UA. Several of the
// supported hosts reject unknown clients outright, and gofile mixes the UA
// into its request signature, so this string must match the one used when
// signing (see extractor.Gofile).
const (
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	DefaultLanguage  = "en-US"

	// FallbackGofileSecret signs gofile requests when the current secret
	// cannot be recovered from gofile's own script, which is where the
	// extractor looks first. Gofile rotates it, so a copy pinned here goes
	// stale in time; HEAPLEACH_GOFILE_SECRET overrides both.
	FallbackGofileSecret = "12af056dacea0b"
)

// Config is the resolved runtime configuration.
type Config struct {
	Addr         string
	DownloadDir  string
	Concurrency  int
	UserAgent    string
	Language     string
	GofileSecret string
	// ExtraHosts extends the built-in install list of each platform family,
	// keyed by family name ("kvs", "peertube", "chevereto", ...).
	//
	// Every family this program covers is software sold or given to many
	// operators, so a list compiled into a binary can only ever trail the
	// sites running it. One setting rather than one per family, because
	// there is nothing family-specific about the reasoning and six
	// near-identical environment variables would be worse than one.
	ExtraHosts map[string][]string
	MaxRetries int
	Streams    int
	SlowSpeed  int64
	// SpeedLimit caps total throughput in bytes per second; 0 is unlimited.
	SpeedLimit int64
	// StallTimeout is how long a transfer may make no progress before the
	// attempt is abandoned and retried from what is already on disk.
	StallTimeout time.Duration
	Timeout      time.Duration
	Debug        bool
	OpenBrowser  bool

	// URLs, when non-empty, switch the process out of server mode: they
	// are downloaded straight to DownloadDir and then it exits. Set only
	// from the command line — there is no environment equivalent.
	URLs []string
	// Password unlocks protected sources in that mode.
	Password string
}

// FromEnv reads HEAPLEACH_*-prefixed environment variables over the defaults.
//
// It performs no validation and touches no filesystem, so a caller can layer
// command-line flags on top before anything is created: resolving the
// directory here would mean creating whichever one the environment named,
// even when an argument overrides it.
func FromEnv() (*Config, error) {
	c := &Config{
		Addr:         env("ADDR", ":8080"),
		DownloadDir:  env("DIR", defaultDir()),
		Concurrency:  DefaultConcurrency,
		UserAgent:    env("USER_AGENT", DefaultUserAgent),
		Language:     env("LANGUAGE", DefaultLanguage),
		GofileSecret: env("GOFILE_SECRET", FallbackGofileSecret),
		ExtraHosts:   extraHosts(),
		MaxRetries:   DefaultMaxRetries,
		Streams:      DefaultStreams,
		SlowSpeed:    DefaultSlowSpeed,
		StallTimeout: StallTimeout,
		Timeout:      DefaultTimeout,
	}

	var err error
	if c.Concurrency, err = envInt("CONCURRENCY", c.Concurrency); err != nil {
		return nil, err
	}
	if c.MaxRetries, err = envInt("MAX_RETRIES", c.MaxRetries); err != nil {
		return nil, err
	}
	if c.Streams, err = envInt("STREAMS", c.Streams); err != nil {
		return nil, err
	}
	slow, err := envInt("SLOW_SPEED", int(c.SlowSpeed))
	if err != nil {
		return nil, err
	}
	c.SlowSpeed = int64(slow)

	ceiling, err := envInt("MAX_SPEED", int(c.SpeedLimit))
	if err != nil {
		return nil, err
	}
	c.SpeedLimit = int64(ceiling)

	if v := env("STALL_TIMEOUT", ""); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%sSTALL_TIMEOUT: %w", envPrefix, err)
		}
		c.StallTimeout = d
	}

	c.Debug = env("DEBUG", "") != ""
	c.OpenBrowser = env("OPEN", "") != ""
	return c, nil
}

// Prepare validates the settled configuration and makes the download
// directory usable, failing fast rather than letting every transfer discover
// the same problem separately.
func (c *Config) Prepare() error {
	if c.Concurrency < 1 || c.Concurrency > MaxConcurrency {
		return fmt.Errorf("concurrency must be between 1 and %d, got %d", MaxConcurrency, c.Concurrency)
	}
	if c.MaxRetries < 0 {
		return fmt.Errorf("retries cannot be negative, got %d", c.MaxRetries)
	}
	if c.Streams < 1 || c.Streams > MaxStreams {
		return fmt.Errorf("streams must be between 1 and %d, got %d", MaxStreams, c.Streams)
	}
	if c.SlowSpeed < 0 {
		return fmt.Errorf("slow-speed cannot be negative, got %d", c.SlowSpeed)
	}
	if c.SpeedLimit < 0 {
		return fmt.Errorf("max-speed cannot be negative, got %d", c.SpeedLimit)
	}
	if c.StallTimeout <= 0 {
		return fmt.Errorf("stall-timeout must be positive, got %s", c.StallTimeout)
	}
	if strings.TrimSpace(c.DownloadDir) == "" {
		return fmt.Errorf("no download directory given")
	}

	dir, err := expandHome(c.DownloadDir)
	if err != nil {
		return err
	}
	if c.DownloadDir, err = filepath.Abs(dir); err != nil {
		return fmt.Errorf("resolve download dir: %w", err)
	}

	if info, err := os.Stat(c.DownloadDir); err == nil && !info.IsDir() {
		return fmt.Errorf("download path %s exists but is not a directory", c.DownloadDir)
	}
	if err := os.MkdirAll(c.DownloadDir, 0o755); err != nil {
		return fmt.Errorf("create download dir: %w", err)
	}
	return checkWritable(c.DownloadDir)
}

// expandHome resolves a leading ~ so a shell-quoted path still works.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// checkWritable proves the download directory can actually be written to,
// so a permission problem surfaces once at startup rather than as a wall of
// failed transfers. This bites most often with a bind-mounted volume whose
// owner does not match the user the container runs as.
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".heapleach-write-probe-*")
	if err != nil {
		return fmt.Errorf("download dir %s is not writable by uid %d: %w\n"+
			"hint: when bind-mounting a host directory, run the container as its owner, "+
			"e.g. docker run --user $(id -u):$(id -g) ...", dir, os.Getuid(), err)
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

// AcceptLanguage renders the Accept-Language header value for the configured
// language, e.g. "en-US" -> "en-US,en;q=0.9".
func (c *Config) AcceptLanguage() string {
	if base, _, ok := strings.Cut(c.Language, "-"); ok {
		return c.Language + "," + base + ";q=0.9"
	}
	return c.Language
}

// defaultDir is where files go when nothing says otherwise: the user's own
// Downloads folder, which is where someone running this expects to find
// them, on all three platforms.
func defaultDir() string {
	// Except in the container image, which ships /downloads to be mounted
	// over and runs as a user with no home directory to speak of.
	if fi, err := os.Stat("/downloads"); err == nil && fi.IsDir() {
		return "/downloads"
	}
	if dir := userDownloadDir(); dir != "" {
		return dir
	}
	// No home directory at all: the working directory beats nothing.
	return "downloads"
}

// userDownloadDir resolves the platform's download folder.
//
// Windows and macOS both keep it at Downloads under the home directory.
// Linux desktops let the user move it and record where it went in the XDG
// user-dirs file, so that is read first: a localised desktop names the
// folder in its own language, and a relocated one is not under home at all.
func userDownloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		if dir := xdgDownloadDir(home); dir != "" {
			return dir
		}
	}
	return filepath.Join(home, "Downloads")
}

// xdgDownloadDir reads the desktop's own record of where downloads belong.
func xdgDownloadDir(home string) string {
	if dir := expandHomeVar(strings.TrimSpace(os.Getenv("XDG_DOWNLOAD_DIR")), home); dir != "" {
		return dir
	}
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	f, err := os.Open(filepath.Join(configHome, "user-dirs.dirs"))
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		rest, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "XDG_DOWNLOAD_DIR=")
		if !ok {
			continue
		}
		if dir := expandHomeVar(strings.Trim(rest, `"`), home); dir != "" {
			return dir
		}
	}
	return ""
}

// expandHomeVar resolves the $HOME prefix the user-dirs file writes paths
// with, and rejects anything still not absolute afterwards: a relative entry
// there would put downloads somewhere unpredictable.
func expandHomeVar(dir, home string) string {
	if dir == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(dir, "$HOME"); ok {
		return filepath.Join(home, rest)
	}
	if !filepath.IsAbs(dir) {
		return ""
	}
	return dir
}

// envPrefix namespaces every setting this program reads.
const envPrefix = "HEAPLEACH_"

// LookupEnv returns a setting's value, trimmed. Exported because httpx reads
// a setting of its own and there should be one place that spells the prefix.
func LookupEnv(name string) string {
	return strings.TrimSpace(os.Getenv(envPrefix + name))
}

func env(name, def string) string {
	if v := LookupEnv(name); v != "" {
		return v
	}
	return def
}

// extraHosts reads the per-family host additions.
//
// HEAPLEACH_EXTRA_HOSTS carries them all, as
// "family:host,host;family:host" — semicolons between families, commas or
// spaces within one. HEAPLEACH_KVS_HOSTS is still honoured, because it
// shipped first and someone is relying on it; it simply folds into the "kvs"
// family.
func extraHosts() map[string][]string {
	out := map[string][]string{}
	add := func(family string, hosts []string) {
		family = strings.ToLower(strings.TrimSpace(family))
		if family == "" || len(hosts) == 0 {
			return
		}
		out[family] = append(out[family], hosts...)
	}

	add(FamilyKVS, envList("KVS_HOSTS"))
	for _, group := range strings.Split(LookupEnv("EXTRA_HOSTS"), ";") {
		family, list, ok := strings.Cut(group, ":")
		if !ok {
			continue
		}
		add(family, splitList(list))
	}
	return out
}

// Family names for ExtraHosts, so the extractors and this file cannot drift
// apart over a spelling.
const (
	FamilyKVS       = "kvs"
	FamilyPeerTube  = "peertube"
	FamilyChevereto = "chevereto"
	FamilyFoolFuuka = "foolfuuka"
	FamilyFediverse = "fediverse"
)

// ExtraHostsFor returns the additional hosts configured for one family.
func (c *Config) ExtraHostsFor(family string) []string {
	if c == nil {
		return nil
	}
	return c.ExtraHosts[family]
}

// envList reads a comma- or whitespace-separated list, for settings that
// name several things rather than one.
func envList(name string) []string {
	return splitList(LookupEnv(name))
}

// splitList separates a value that names several things.
func splitList(raw string) []string {
	var out []string
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	}) {
		if field = strings.TrimSpace(field); field != "" {
			out = append(out, field)
		}
	}
	return out
}

func envInt(name string, def int) (int, error) {
	v := LookupEnv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s%s: %w", envPrefix, name, err)
	}
	return n, nil
}
