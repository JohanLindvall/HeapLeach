// SPDX-License-Identifier: MIT

// Package config holds the process-wide settings, resolved from the
// environment and then from command-line flags, with sensible defaults for
// both container and local runs.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	Addr        string
	DownloadDir string
	// StateFile is where the queue is written so a restart can pick it up
	// again. Empty disables the whole mechanism, which is what a run given
	// URLs on the command line wants: it downloads and exits, and has no
	// queue worth outliving it.
	//
	// Deliberately not under DownloadDir: that moves while the service runs,
	// and state that followed it would be split across every destination
	// ever used.
	StateFile string
	// LegacyStateFile is the old default queue path, migrated only when
	// STATE was not explicitly configured. A custom .zst path must never
	// adopt or delete the unrelated file with that suffix removed.
	LegacyStateFile string
	Concurrency     int
	UserAgent       string
	Language        string
	GofileSecret    string
	// ArchiveFormats overrides which archive.org renditions are taken. An
	// item there routinely offers the same film six times, so the extractor
	// picks per media type; this is the escape for when its taste is wrong.
	ArchiveFormats []string
	MaxRetries     int
	Streams        int
	SlowSpeed      int64
	// SpeedLimit caps total throughput in bytes per second; 0 is unlimited.
	SpeedLimit int64
	// MinFreeDisk is how much room must be left at the destination before
	// another transfer is started. Nothing new begins below it and the queue
	// waits, which is what keeps a long run from filling the disk it is
	// writing to. Zero disables the check.
	MinFreeDisk int64
	// StallTimeout is how long a transfer may make no progress before the
	// attempt is abandoned and retried from what is already on disk.
	StallTimeout time.Duration
	Timeout      time.Duration
	Debug        bool
	OpenBrowser  bool
	// ResumeRestored sets going, at startup, the jobs a previous run left
	// unfinished, instead of holding them for a person to resume. For a
	// service that is replaced without anyone watching — a deploy tool
	// rolling a new image — where held would mean stopped until noticed.
	ResumeRestored bool
	// Version is the build's version, for the health endpoint to report.
	Version string
	// ProfileAddr, when set, is where Go's runtime profiles are served, on a
	// listener of their own; empty is off. They describe the process from
	// the inside, so a loopback address is the one to name.
	ProfileAddr string
	// MaxSources and MaxFiles bound what one submitted URL may expand to:
	// how many of a page's links are followed (an index search's albums, a
	// thread's links) and how many files a job may hold. The defaults are
	// MaxExpandedSources and MaxListingFiles; these are for somebody who
	// really did mean the whole index. There is no "unlimited" — name a
	// number, since every file of a job is also a row in the UI and a line
	// in the saved queue. Zero means the default.
	MaxSources int
	MaxFiles   int
	// MaxConcurrency is the ceiling on Concurrency, whether set at startup
	// or live in Settings. Zero means DefaultMaxConcurrency.
	MaxConcurrency int
	// CaptchaConcurrency is how many CAPTCHAs are read at once, each in a
	// model session of its own. Zero means DefaultCaptchaConcurrency.
	CaptchaConcurrency int
	// ExitWhenIdle ends the process once there is nothing left to download
	// and no browser is watching. Set only for a bare invocation, which is
	// a desktop session rather than a service: see applyBareDefaults.
	ExitWhenIdle bool

	// URLs, when non-empty, switch the process out of server mode: they
	// are downloaded straight to DownloadDir and then it exits. Set only
	// from the command line — there is no environment equivalent.
	URLs []string
	// Password unlocks protected sources in that mode.
	Password string

	// Proxies enables extra egress routes for K2S, FileBoom, EMLOAD, MEGA once
	// this address's quota is used up, and WAF recovery on any host. The
	// inventory and health live independently of the queue.
	Proxies        bool
	ProxyDB        string
	ProxyEndpoints []string
	ProxyFeeds     []string
	// ProxyType is the protocol every proxy group draws its routes from
	// unless ProxyTypes names one of its own: "any", "socks" or "http". A
	// specific protocol leaves the direct connection out as well.
	ProxyType string
	// ProxyTypes is a protocol per proxy group (ProxyGroups), overriding
	// ProxyType for that group alone.
	ProxyTypes   map[string]string
	ProxyRetries int
	// ProxyExitProbes are asked, through each route and in turn, for the
	// public address the route leaves by (ProxyExitProbeURL). None skips the
	// question, which is what a Config built by hand, as tests build them,
	// does.
	ProxyExitProbes []string
}

// FromEnv reads HEAPLEACH_*-prefixed environment variables over the defaults.
//
// It performs no validation and touches no filesystem, so a caller can layer
// command-line flags on top before anything is created: resolving the
// directory here would mean creating whichever one the environment named,
// even when an argument overrides it.
func FromEnv() (*Config, error) {
	c := &Config{
		Addr:            env("ADDR", ":8080"),
		DownloadDir:     env("DIR", defaultDir()),
		StateFile:       env("STATE", defaultStateFile()),
		Concurrency:     DefaultConcurrency,
		UserAgent:       env("USER_AGENT", DefaultUserAgent),
		Language:        env("LANGUAGE", DefaultLanguage),
		GofileSecret:    env("GOFILE_SECRET", FallbackGofileSecret),
		ArchiveFormats:  envList("IA_FORMATS"),
		MaxRetries:      DefaultMaxRetries,
		Streams:         DefaultStreams,
		SlowSpeed:       DefaultSlowSpeed,
		StallTimeout:    StallTimeout,
		Timeout:         DefaultTimeout,
		MinFreeDisk:     DefaultMinFreeDisk,
		MaxSources:      MaxExpandedSources,
		MaxFiles:        MaxListingFiles,
		Debug:           true,
		Proxies:         true,
		ProxyDB:         env("PROXY_DB", ""),
		ProxyEndpoints:  envList("PROXY_ENDPOINTS"),
		ProxyFeeds:      []string{DefaultProxyFeed},
		ProxyType:       env("PROXY_TYPE", ProxyTypeAny),
		ProxyRetries:    DefaultProxyRetries,
		ProxyExitProbes: []string{ProxyExitProbeURL, ProxyExitProbeFallbackURL},
	}
	if enabled, set := EnvBool("PROXIES"); set {
		c.Proxies = enabled
	}
	warnRetiredSettings()
	if _, set := os.LookupEnv(envPrefix + "PROXY_ENDPOINTS"); !set {
		c.ProxyEndpoints = []string{"direct"}
	}
	if _, set := os.LookupEnv(envPrefix + "PROXY_FEEDS"); set {
		c.ProxyFeeds = envList("PROXY_FEEDS")
	}

	// An explicitly empty state path disables persistence; env's usual
	// empty-means-default rule cannot represent that choice.
	if value, set := os.LookupEnv(envPrefix + "STATE"); set {
		c.StateFile = strings.TrimSpace(value)
	} else if c.StateFile != "" {
		c.LegacyStateFile = strings.TrimSuffix(c.StateFile, ".zst")
	}
	if c.ProxyDB == "" {
		c.ProxyDB = defaultProxyDB(c.StateFile)
	}

	var err error
	if c.ProxyTypes, err = ParseProxyTypes(LookupEnv("PROXY_TYPES")); err != nil {
		return nil, fmt.Errorf("%sPROXY_TYPES: %w", envPrefix, err)
	}
	if c.ProxyRetries, err = envInt("PROXY_RETRIES", c.ProxyRetries); err != nil {
		return nil, err
	}
	if c.Concurrency, err = envInt("CONCURRENCY", c.Concurrency); err != nil {
		return nil, err
	}
	if c.MaxRetries, err = envInt("MAX_RETRIES", c.MaxRetries); err != nil {
		return nil, err
	}
	if c.Streams, err = envInt("STREAMS", c.Streams); err != nil {
		return nil, err
	}
	if c.MaxConcurrency, err = envInt("MAX_CONCURRENCY", c.MaxConcurrency); err != nil {
		return nil, err
	}
	if c.CaptchaConcurrency, err = envInt("CAPTCHA_CONCURRENCY", c.CaptchaConcurrency); err != nil {
		return nil, err
	}
	if c.MaxSources, err = envInt("MAX_SOURCES", c.MaxSources); err != nil {
		return nil, err
	}
	if c.MaxFiles, err = envInt("MAX_FILES", c.MaxFiles); err != nil {
		return nil, err
	}
	if c.SlowSpeed, err = envSize("SLOW_SPEED", c.SlowSpeed); err != nil {
		return nil, err
	}
	if c.SpeedLimit, err = envSize("MAX_SPEED", c.SpeedLimit); err != nil {
		return nil, err
	}
	if c.MinFreeDisk, err = envSize("MIN_FREE", c.MinFreeDisk); err != nil {
		return nil, err
	}

	if v := env("STALL_TIMEOUT", ""); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%sSTALL_TIMEOUT: %w", envPrefix, err)
		}
		c.StallTimeout = d
	}

	if enabled, set := EnvBool("DEBUG"); set {
		c.Debug = enabled
	}
	c.OpenBrowser, _ = EnvBool("OPEN")
	c.ResumeRestored, _ = EnvBool("RESUME")
	c.ProfileAddr = env("PPROF", "")
	return c, nil
}

// ParseSize reads a byte count written as a plain number or with a unit.
//
// Both conventions are accepted because both are meant: kB/MB/GB are powers
// of a thousand, as every disk and every figure this program prints uses,
// and KiB/MiB/GiB are powers of 1024, which is what a filesystem reports. A
// bare number is bytes. The distinction is worth honouring rather than
// guessing at — the two differ by 7% at the gigabyte, which is the
// difference between a floor that holds and one that does not.
func ParseSize(raw string) (int64, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, errors.New("no size given")
	}

	digits := strings.TrimRight(text, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ ")
	unit := strings.ToLower(strings.TrimSpace(text[len(digits):]))

	value, err := strconv.ParseFloat(strings.TrimSpace(digits), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size", raw)
	}
	if value < 0 {
		return 0, fmt.Errorf("%q is negative", raw)
	}

	multiplier, ok := sizeUnits[unit]
	if !ok {
		return 0, fmt.Errorf("%q has an unknown unit %q", raw, unit)
	}
	scaled := value * float64(multiplier)
	// float64(math.MaxInt64) is 2^63, which itself does not fit.
	if scaled >= math.MaxInt64 {
		return 0, fmt.Errorf("%q does not fit in an int64", raw)
	}
	return int64(scaled), nil
}

// sizeUnits are the suffixes ParseSize accepts. "b" and "" are bytes; the
// single letters follow the decimal convention the rest of the program
// prints in, and the "i" forms are binary.
var sizeUnits = map[string]int64{
	"":    1,
	"b":   1,
	"k":   1_000,
	"kb":  1_000,
	"m":   1_000_000,
	"mb":  1_000_000,
	"g":   1_000_000_000,
	"gb":  1_000_000_000,
	"t":   1_000_000_000_000,
	"tb":  1_000_000_000_000,
	"kib": 1 << 10,
	"mib": 1 << 20,
	"gib": 1 << 30,
	"tib": 1 << 40,
}

// FormatSize renders a byte count the way ParseSize reads it, exactly: a
// value that is a whole number of some unit is written in that unit, and
// anything else as plain bytes, so what is printed as a default can be
// pasted back as a setting without changing it. Both conventions are tried
// and the shorter wins — ten gibibytes reads as 10GiB, not 10737418240.
func FormatSize(n int64) string {
	if n <= 0 {
		return strconv.FormatInt(n, 10)
	}
	best := strconv.FormatInt(n, 10)
	for _, unit := range []struct {
		suffix string
		size   int64
	}{
		{"TB", 1_000_000_000_000}, {"GB", 1_000_000_000}, {"MB", 1_000_000}, {"kB", 1_000},
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	} {
		if n%unit.size != 0 {
			continue
		}
		if s := strconv.FormatInt(n/unit.size, 10) + unit.suffix; len(s) < len(best) {
			best = s
		}
	}
	return best
}

// EnvBool reads a setting that can be turned off as well as on, reporting
// both the value and whether it was set at all.
//
// Most switches here are on when present and absent otherwise, which cannot
// express "no". HEAPLEACH_OPEN has to: a bare run opens a browser by default,
// so somebody who does not want one — running over SSH, or on a machine with
// no desktop — needs a way to say so, and setting the variable to anything
// would otherwise turn it further on.
func EnvBool(name string) (value, set bool) {
	switch strings.ToLower(LookupEnv(name)) {
	case "":
		return false, false
	case "0", "false", "no", "off":
		return false, true
	default:
		return true, true
	}
}

// PrepareDir expands, resolves and creates a download directory, and proves
// it can be written to.
//
// Split out of Prepare because the destination is no longer settled at
// startup: the UI can move it while the program runs, and a directory chosen
// then has to be checked exactly as carefully as one chosen on the command
// line — the difference between the two is only when the user said it.
func PrepareDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("no download directory given")
	}
	expanded, err := expandHome(path)
	if err != nil {
		return "", err
	}
	dir, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("resolve download dir: %w", err)
	}
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		return "", fmt.Errorf("download path %s exists but is not a directory", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create download dir: %w", err)
	}
	if err := checkWritable(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// Prepare validates the settled configuration and makes the download
// directory usable, failing fast rather than letting every transfer discover
// the same problem separately.
func (c *Config) Prepare() error {
	if c.ProxyRetries < 0 {
		return errors.New("proxy retries cannot be negative")
	}
	if c.Proxies {
		if strings.TrimSpace(c.ProxyDB) == "" {
			return errors.New("proxy-db must name a Bolt database")
		}
	}
	c.ProxyType = strings.ToLower(strings.TrimSpace(c.ProxyType))
	if c.ProxyType == "" {
		c.ProxyType = ProxyTypeAny
	}
	if !ValidProxyType(c.ProxyType) {
		return fmt.Errorf("proxy type must be any, socks or http, got %q", c.ProxyType)
	}
	for group, kind := range c.ProxyTypes {
		if !slices.Contains(ProxyGroups, group) || !ValidProxyType(kind) {
			return fmt.Errorf("proxy types: %q=%q is not a proxy group and a protocol", group, kind)
		}
	}
	if c.ProxyDB != "" {
		// A disabled pool can be enabled through the live settings later.
		var err error
		c.ProxyDB, err = expandHome(c.ProxyDB)
		if err != nil {
			return err
		}
	}
	// URLs on the command line mean download these and quit. There is no
	// queue to outlive the process, and writing one would leave a service
	// started later picking up a list the user thought was long finished.
	if len(c.URLs) > 0 {
		c.StateFile = ""
		c.LegacyStateFile = ""
	}
	if c.MaxConcurrency < 0 {
		return fmt.Errorf("max-concurrency cannot be negative, got %d", c.MaxConcurrency)
	}
	if c.MaxConcurrency == 0 {
		c.MaxConcurrency = DefaultMaxConcurrency
	}
	if c.Concurrency < 1 || c.Concurrency > c.MaxConcurrency {
		return fmt.Errorf("concurrency must be between 1 and %d, got %d", c.MaxConcurrency, c.Concurrency)
	}
	if c.CaptchaConcurrency == 0 {
		c.CaptchaConcurrency = DefaultCaptchaConcurrency
	}
	if c.CaptchaConcurrency < 1 || c.CaptchaConcurrency > MaxCaptchaConcurrency {
		return fmt.Errorf("captcha-concurrency must be between 1 and %d, got %d", MaxCaptchaConcurrency, c.CaptchaConcurrency)
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
	if c.MinFreeDisk < 0 {
		return fmt.Errorf("min-free cannot be negative, got %d", c.MinFreeDisk)
	}
	// Zero is the default rather than nothing at all, as it is wherever the
	// caps are read; a config written as a literal leaves them unset.
	if c.MaxSources < 0 {
		return fmt.Errorf("max-sources cannot be negative, got %d", c.MaxSources)
	}
	if c.MaxSources == 0 {
		c.MaxSources = MaxExpandedSources
	}
	if c.MaxFiles < 0 {
		return fmt.Errorf("max-files cannot be negative, got %d", c.MaxFiles)
	}
	if c.MaxFiles == 0 {
		c.MaxFiles = MaxListingFiles
	}
	dir, err := PrepareDir(c.DownloadDir)
	if err != nil {
		return err
	}
	c.DownloadDir = dir
	return nil
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
			"hint: when bind-mounting a host directory, run the container as its owner "+
			"(docker run --user $(id -u):$(id -g), or `make run-image`, which does)", dir, os.Getuid(), err)
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

// defaultStateFile is where the queue is remembered between runs.
//
// XDG_STATE_HOME is the right shelf for it by the specification's own
// description — state that should persist between restarts but is not
// precious enough to be config or data. Windows and macOS have no such
// convention worth honouring here, so both get the same place under the home
// directory rather than a platform tour for one small file.
func defaultStateFile() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Nowhere to remember anything: the queue simply does not outlive
		// the process, which is what happened before it could.
		return ""
	}
	base := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if base == "" || runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		base = filepath.Join(home, ".local", "state")
	}
	// Compressed; a queue.json beside it is from before that, and is read
	// once and retired (see download.loadState).
	return filepath.Join(base, "heapleach", "queue.json.zst")
}

// defaultProxyDB keeps the route inventory beside the queue, wherever the
// queue has been put. A deployment mounts that directory to keep its queue,
// and the inventory went beside the default queue path instead: in a
// container whose home was not mounted, every restart forgot every route.
// With no queue, it still has a home of its own.
func defaultProxyDB(state string) string {
	if state == "" {
		state = defaultStateFile()
	}
	if state != "" {
		return filepath.Join(filepath.Dir(state), "proxies.db")
	}
	return "proxies.db"
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

// retiredHostSettings named installs of each platform family, back when an
// extractor was chosen by the URL's host. Pages are now recognised by what
// they serve, so an install nobody named is found like any other and these
// have nothing left to do; they are still read, so that someone relying on
// one learns it no longer applies rather than wondering why nothing changed.
var retiredHostSettings = []string{"EXTRA_HOSTS", "KVS_HOSTS"}

// warnRetiredSettings logs each retired setting that is still set.
func warnRetiredSettings() {
	for _, name := range retiredHostSettings {
		if LookupEnv(name) != "" {
			slog.Warn("setting is ignored: sites are recognised by their pages, so no host list is needed",
				"setting", envPrefix+name)
		}
	}
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

// envSize reads a byte count that may carry a unit — 5MB, 10GiB — since a
// rate or a floor is far easier to state that way than as a run of zeroes.
// A bare number is still bytes, so what worked before still works.
func envSize(name string, def int64) (int64, error) {
	v := LookupEnv(name)
	if v == "" {
		return def, nil
	}
	n, err := ParseSize(v)
	if err != nil {
		return 0, fmt.Errorf("%s%s: %w", envPrefix, name, err)
	}
	return n, nil
}

// The protocols a proxy group can be held to.
const (
	ProxyTypeAny   = "any"
	ProxyTypeSOCKS = "socks"
	ProxyTypeHTTP  = "http"
)

// ProxyGroups are the groups that draw on the proxy pool, each a scope of
// its own: services that count free downloads per address, and WAF recovery
// on any host.
var ProxyGroups = []string{"keep2share", "fileboom", "emload", "mega", "cloudflare"}

// ValidProxyType reports a protocol a proxy group can be held to.
func ValidProxyType(kind string) bool {
	return kind == ProxyTypeAny || kind == ProxyTypeSOCKS || kind == ProxyTypeHTTP
}

// ParseProxyTypes reads protocols per proxy group, written
// "keep2share=socks,cloudflare=any". An empty value is no override.
func ParseProxyTypes(raw string) (map[string]string, error) {
	types := make(map[string]string)
	for _, field := range splitList(raw) {
		group, kind, ok := strings.Cut(field, "=")
		group, kind = strings.ToLower(strings.TrimSpace(group)), strings.ToLower(strings.TrimSpace(kind))
		if !ok || !slices.Contains(ProxyGroups, group) || !ValidProxyType(kind) {
			return nil, fmt.Errorf("%q is not a proxy group (%s) and a protocol (any, socks or http)",
				field, strings.Join(ProxyGroups, ", "))
		}
		types[group] = kind
	}
	return types, nil
}

// FormatProxyTypes writes protocols per proxy group as ParseProxyTypes reads
// them, in the groups' order.
func FormatProxyTypes(types map[string]string) string {
	var fields []string
	for _, group := range ProxyGroups {
		if kind, ok := types[group]; ok {
			fields = append(fields, group+"="+kind)
		}
	}
	return strings.Join(fields, ",")
}
