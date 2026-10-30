// SPDX-License-Identifier: MIT

package config

import (
	"bytes"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnvBoolSeparatesUnsetFromFalse(t *testing.T) {
	// LookupEnv supplies the HEAPLEACH_ prefix, so the setting is named
	// without it and the variable with it.
	const name = "TEST_FLAG"
	cases := map[string][2]bool{
		"":      {false, false},
		"0":     {false, true},
		"false": {false, true},
		"No":    {false, true},
		"OFF":   {false, true},
		"1":     {true, true},
		"true":  {true, true},
		"yes":   {true, true},
		"on":    {true, true},
	}
	for raw, want := range cases {
		t.Setenv(envPrefix+name, raw)
		value, set := EnvBool(name)
		if value != want[0] || set != want[1] {
			t.Errorf("EnvBool(%q) = (%v, %v), want (%v, %v)", raw, value, set, want[0], want[1])
		}
	}
}

func TestFromEnvDefaults(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" || cfg.Concurrency != DefaultConcurrency ||
		cfg.MaxRetries != DefaultMaxRetries || cfg.Streams != DefaultStreams || !cfg.Debug {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.UserAgent == "" || cfg.Language == "" {
		t.Error("a UA and a language must always be set")
	}
}

// The route inventory lives beside the queue wherever the queue is put, and
// in the default state directory when no queue is kept.
func TestTheProxyDatabaseLivesBesideTheQueue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HEAPLEACH_PROXY_DB", "")
	t.Setenv("HEAPLEACH_STATE", filepath.Join(dir, "mounted", "queue.json.zst"))
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "mounted", "proxies.db"); cfg.ProxyDB != want {
		t.Fatalf("proxy database %q, want %q beside the queue", cfg.ProxyDB, want)
	}
	t.Setenv("HEAPLEACH_STATE", "") // no queue kept
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "xdg"))
	if cfg, err = FromEnv(); err != nil {
		t.Fatal(err)
	}
	if cfg.StateFile != "" || cfg.ProxyDB != defaultProxyDB("") || cfg.ProxyDB == "" {
		t.Fatalf("with no queue: state %q, proxy database %q", cfg.StateFile, cfg.ProxyDB)
	}
	t.Setenv("HEAPLEACH_PROXY_DB", filepath.Join(dir, "elsewhere.db"))
	if cfg, err = FromEnv(); err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyDB != filepath.Join(dir, "elsewhere.db") {
		t.Fatalf("an explicit proxy database was overridden: %q", cfg.ProxyDB)
	}
}

func TestProxyConfigurationDoesNotCreateStateUntilStartup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HEAPLEACH_PROXIES", "1")
	t.Setenv("HEAPLEACH_PROXY_DB", filepath.Join(dir, "state", "routes.db"))
	t.Setenv("HEAPLEACH_PROXY_FEEDS", "")
	t.Setenv("HEAPLEACH_PROXY_ENDPOINTS", "http://one.example.test:8000,socks5://two.example.test:1080")
	t.Setenv("HEAPLEACH_PROXY_RETRIES", "12")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Proxies || len(cfg.ProxyFeeds) != 0 || len(cfg.ProxyEndpoints) != 2 || cfg.ProxyRetries != 12 {
		t.Fatalf("proxy config=%+v", cfg)
	}
	if _, err := os.Stat(filepath.Dir(cfg.ProxyDB)); !os.IsNotExist(err) {
		t.Fatal("FromEnv created the state directory")
	}
	cfg.URLs = []string{"https://example.test/file"}
	cfg.DownloadDir = dir
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	if cfg.StateFile != "" || cfg.ProxyDB == "" {
		t.Fatal("CLI mode must keep the proxy database independently of the queue")
	}
	t.Setenv("HEAPLEACH_PROXY_RETRIES", "-1")
	cfg, err = FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Prepare() == nil {
		t.Fatal("negative proxy retry budget accepted")
	}
}

func TestProxyDefaultAndEnvironmentOptOut(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"", true}, {"1", true}, {"true", true}, {"0", false}, {"false", false}, {"off", false}} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("HEAPLEACH_PROXIES", tc.value)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Proxies != tc.want {
				t.Fatalf("Proxies = %v for %q, want %v", cfg.Proxies, tc.value, tc.want)
			}
		})
	}
}

func TestFromEnvReadsTheEnvironment(t *testing.T) {
	t.Setenv("HEAPLEACH_ADDR", ":9999")
	t.Setenv("HEAPLEACH_CONCURRENCY", "7")
	t.Setenv("HEAPLEACH_MAX_SPEED", "1000000")
	t.Setenv("HEAPLEACH_STALL_TIMEOUT", "45s")
	t.Setenv("HEAPLEACH_DEBUG", "1")
	t.Setenv("HEAPLEACH_RESUME", "true")
	t.Setenv("HEAPLEACH_MAX_SOURCES", "5000")
	t.Setenv("HEAPLEACH_MAX_FILES", "400000")
	t.Setenv("HEAPLEACH_MAX_CONCURRENCY", "256")
	t.Setenv("HEAPLEACH_CAPTCHA_CONCURRENCY", "8")
	t.Setenv("HEAPLEACH_PPROF", "127.0.0.1:6060")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9999" || cfg.Concurrency != 7 || cfg.SpeedLimit != 1000000 ||
		cfg.StallTimeout != 45*time.Second || !cfg.Debug || !cfg.ResumeRestored ||
		cfg.MaxSources != 5000 || cfg.MaxFiles != 400000 || cfg.ProfileAddr != "127.0.0.1:6060" ||
		cfg.MaxConcurrency != 256 || cfg.CaptchaConcurrency != 8 {
		t.Errorf("cfg = %+v", cfg)
	}
}

// The host-list settings are retired: still accepted, so a configuration
// carrying one starts as before, and named in a warning, so its owner learns
// it no longer does anything.
func TestRetiredHostSettingsAreAcceptedAndReported(t *testing.T) {
	t.Setenv("HEAPLEACH_KVS_HOSTS", "tube.example.test")
	t.Setenv("HEAPLEACH_EXTRA_HOSTS", "peertube:one.example.test")
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	if _, err := FromEnv(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HEAPLEACH_KVS_HOSTS", "HEAPLEACH_EXTRA_HOSTS"} {
		if !strings.Contains(logged.String(), name) {
			t.Errorf("no warning names %s: %q", name, logged.String())
		}
	}
}

func TestFromEnvRejectsGarbage(t *testing.T) {
	t.Setenv("HEAPLEACH_CONCURRENCY", "several")
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "HEAPLEACH_CONCURRENCY") {
		t.Errorf("a bad value should be refused and named, got %v", err)
	}

	t.Setenv("HEAPLEACH_CONCURRENCY", "")
	t.Setenv("HEAPLEACH_STALL_TIMEOUT", "whenever")
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "HEAPLEACH_STALL_TIMEOUT") {
		t.Errorf("a bad duration should be refused and named, got %v", err)
	}
}

func TestPrepareValidates(t *testing.T) {
	base := func() *Config {
		return &Config{
			DownloadDir: t.TempDir(), Concurrency: 4, Streams: 8,
			MaxRetries: 3, StallTimeout: time.Minute,
		}
	}

	cases := []struct {
		name   string
		break_ func(*Config)
		hint   string
	}{
		{"zero concurrency", func(c *Config) { c.Concurrency = 0 }, "concurrency"},
		{"over a ceiling set lower", func(c *Config) { c.MaxConcurrency, c.Concurrency = 16, 17 }, "concurrency"},
		{"negative ceiling on concurrency", func(c *Config) { c.MaxConcurrency = -1 }, "max-concurrency"},
		{"too much concurrency", func(c *Config) { c.Concurrency = DefaultMaxConcurrency + 1 }, "concurrency"},
		{"negative CAPTCHA concurrency", func(c *Config) { c.CaptchaConcurrency = -1 }, "captcha-concurrency"},
		{"too many CAPTCHAs at once", func(c *Config) { c.CaptchaConcurrency = MaxCaptchaConcurrency + 1 }, "captcha-concurrency"},
		{"negative retries", func(c *Config) { c.MaxRetries = -1 }, "retries"},
		{"zero streams", func(c *Config) { c.Streams = 0 }, "streams"},
		{"negative slow speed", func(c *Config) { c.SlowSpeed = -1 }, "slow-speed"},
		{"negative ceiling", func(c *Config) { c.SpeedLimit = -1 }, "max-speed"},
		{"no stall timeout", func(c *Config) { c.StallTimeout = 0 }, "stall-timeout"},
		{"blank directory", func(c *Config) { c.DownloadDir = "   " }, "directory"},
		{"unknown proxy protocol", func(c *Config) { c.ProxyType = "tor" }, "proxy type"},
		{"protocol for no proxy group", func(c *Config) { c.ProxyTypes = map[string]string{"elsewhere": ProxyTypeSOCKS} }, "proxy types"},
		{"unknown protocol for a group", func(c *Config) { c.ProxyTypes = map[string]string{"fileboom": "tor"} }, "proxy types"},
	}
	for _, tc := range cases {
		cfg := base()
		tc.break_(cfg)
		err := cfg.Prepare()
		if err == nil || !strings.Contains(err.Error(), tc.hint) {
			t.Errorf("%s: err = %v, want mention of %q", tc.name, err, tc.hint)
		}
	}

	if err := base().Prepare(); err != nil {
		t.Errorf("a sound config should prepare cleanly: %v", err)
	}
}

// The ceiling on parallel transfers defaults to a hundred and twenty-eight,
// and a higher one can be set: a pool of proxy addresses carries one
// Keep2Share transfer each.
func TestTheConcurrencyCeilingIsASetting(t *testing.T) {
	cfg := &Config{DownloadDir: t.TempDir(), Concurrency: 128, Streams: 8, StallTimeout: time.Minute}
	if err := cfg.Prepare(); err != nil || cfg.MaxConcurrency != DefaultMaxConcurrency {
		t.Fatalf("default: max=%d err=%v; want %d allowed", cfg.MaxConcurrency, err, DefaultMaxConcurrency)
	}
	raised := &Config{DownloadDir: t.TempDir(), Concurrency: 300, MaxConcurrency: 512, Streams: 8, StallTimeout: time.Minute}
	if err := raised.Prepare(); err != nil {
		t.Fatalf("a raised ceiling refused what it allows: %v", err)
	}
}

func TestPrepareCreatesAndProvesTheDirectory(t *testing.T) {
	cfg := &Config{
		DownloadDir: filepath.Join(t.TempDir(), "made", "on", "demand"),
		Concurrency: 1, Streams: 1, StallTimeout: time.Minute,
	}
	if err := cfg.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	info, err := os.Stat(cfg.DownloadDir)
	if err != nil || !info.IsDir() {
		t.Errorf("directory was not created: %v", err)
	}
	if !filepath.IsAbs(cfg.DownloadDir) {
		t.Errorf("directory %q was not resolved to an absolute path", cfg.DownloadDir)
	}
}

func TestPrepareRefusesAFileInTheWay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DownloadDir: path, Concurrency: 1, Streams: 1, StallTimeout: time.Minute}
	if err := cfg.Prepare(); err == nil {
		t.Error("a file where the directory should be must be refused")
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory here")
	}
	got, err := expandHome("~/Downloads")
	if err != nil || got != filepath.Join(home, "Downloads") {
		t.Errorf("expandHome(~/Downloads) = %q, %v", got, err)
	}
	got, err = expandHome("~")
	if err != nil || got != home {
		t.Errorf("expandHome(~) = %q, %v", got, err)
	}
	// Only a leading ~ means home; anything else passes through.
	got, err = expandHome("/tmp/~x")
	if err != nil || got != "/tmp/~x" {
		t.Errorf("expandHome(/tmp/~x) = %q, %v", got, err)
	}
}

func TestAcceptLanguage(t *testing.T) {
	if got := (&Config{Language: "en-US"}).AcceptLanguage(); got != "en-US,en;q=0.9" {
		t.Errorf("en-US -> %q", got)
	}
	if got := (&Config{Language: "sv"}).AcceptLanguage(); got != "sv" {
		t.Errorf("a bare tag has no base to add: %q", got)
	}
}

func TestXDGDownloadDir(t *testing.T) {
	home := t.TempDir()

	// The explicit variable wins outright.
	t.Setenv("XDG_DOWNLOAD_DIR", filepath.Join(home, "Hämtningar"))
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := xdgDownloadDir(home); got != filepath.Join(home, "Hämtningar") {
		t.Errorf("explicit variable: %q", got)
	}

	// Otherwise the user-dirs file speaks for the desktop.
	t.Setenv("XDG_DOWNLOAD_DIR", "")
	confDir := filepath.Join(home, ".config")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# generated\nXDG_DOWNLOAD_DIR=\"$HOME/Nedlastinger\"\n"
	if err := os.WriteFile(filepath.Join(confDir, "user-dirs.dirs"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := xdgDownloadDir(home); got != filepath.Join(home, "Nedlastinger") {
		t.Errorf("user-dirs file: %q", got)
	}
}

func TestExpandHomeVarRejectsRelativePaths(t *testing.T) {
	// A relative entry would put downloads somewhere unpredictable.
	if got := expandHomeVar("Downloads", "/home/u"); got != "" {
		t.Errorf("relative path accepted: %q", got)
	}
	if got := expandHomeVar("$HOME/dl", "/home/u"); got != filepath.Join("/home/u", "dl") {
		t.Errorf("$HOME expansion: %q", got)
	}
	if got := expandHomeVar("/abs/dl", "/home/u"); got != "/abs/dl" {
		t.Errorf("absolute passthrough: %q", got)
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"1024", 1024},
		{" 4096 ", 4096},
		{"512B", 512},
		// Decimal units, matching every figure the program prints.
		{"10GB", 10_000_000_000},
		{"10 gb", 10_000_000_000},
		{"2M", 2_000_000},
		{"1t", 1_000_000_000_000},
		// Binary units, matching what a filesystem reports. The two differ
		// by 7% at the gigabyte, which is why both are honoured rather than
		// one being guessed at.
		{"10GiB", 10 << 30},
		{"1KiB", 1024},
		{"1 mib", 1 << 20},
		// Fractions, since "1.5GB" is how a person writes it.
		{"1.5GB", 1_500_000_000},
	}
	for _, tc := range cases {
		got, err := ParseSize(tc.in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	for _, bad := range []string{"", "   ", "GB", "-5", "-1GB", "12 parsecs", "1.2.3", "0x10"} {
		if got, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", bad, got)
		}
	}
}

// The rates are sizes too, and a size is far easier to state with a unit
// than as a run of zeroes. A bare number still means bytes.
func TestRatesFromEnvTakeUnits(t *testing.T) {
	t.Setenv("HEAPLEACH_SLOW_SPEED", "500kB")
	t.Setenv("HEAPLEACH_MAX_SPEED", "5MB")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SlowSpeed != 500_000 {
		t.Errorf("SlowSpeed = %d, want 500000", cfg.SlowSpeed)
	}
	if cfg.SpeedLimit != 5_000_000 {
		t.Errorf("SpeedLimit = %d, want 5000000", cfg.SpeedLimit)
	}

	t.Setenv("HEAPLEACH_MAX_SPEED", "2500000")
	if cfg, err = FromEnv(); err != nil || cfg.SpeedLimit != 2_500_000 {
		t.Errorf("a bare byte count: %d, %v", cfg.SpeedLimit, err)
	}

	t.Setenv("HEAPLEACH_MAX_SPEED", "fast")
	if _, err := FromEnv(); err == nil {
		t.Error("an unparseable rate was accepted")
	}
}

// What FormatSize prints, ParseSize reads back to the same number — that is
// the whole contract, since the printed form is the default `-h` shows.
func TestFormatSizeRoundTrips(t *testing.T) {
	tests := map[int64]string{
		0:                 "0",
		1:                 "1",
		999:               "999",
		1_000:             "1kB",
		2_000_000:         "2MB",
		5_000_000_000:     "5GB",
		1_000_000_000_000: "1TB",
		10 << 30:          "10GiB",
		1 << 20:           "1MiB",
		1 << 40:           "1TiB",
		1_500_000:         "1500kB",
		12_345:            "12345",
	}
	for n, want := range tests {
		got := FormatSize(n)
		if got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", n, got, want)
		}
		back, err := ParseSize(got)
		if err != nil || back != n {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", got, back, err, n)
		}
	}
}

func TestMinFreeDiskFromEnv(t *testing.T) {
	t.Run("defaults to ten gibibytes", func(t *testing.T) {
		cfg, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MinFreeDisk != DefaultMinFreeDisk {
			t.Errorf("MinFreeDisk = %d, want %d", cfg.MinFreeDisk, DefaultMinFreeDisk)
		}
	})

	t.Run("takes a unit", func(t *testing.T) {
		t.Setenv("HEAPLEACH_MIN_FREE", "250GB")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MinFreeDisk != 250_000_000_000 {
			t.Errorf("MinFreeDisk = %d, want 250000000000", cfg.MinFreeDisk)
		}
	})

	t.Run("zero turns the check off", func(t *testing.T) {
		t.Setenv("HEAPLEACH_MIN_FREE", "0")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MinFreeDisk != 0 {
			t.Errorf("MinFreeDisk = %d, want 0", cfg.MinFreeDisk)
		}
	})

	t.Run("nonsense is reported rather than ignored", func(t *testing.T) {
		t.Setenv("HEAPLEACH_MIN_FREE", "plenty")
		if _, err := FromEnv(); err == nil {
			t.Error("an unparseable floor was accepted")
		}
	})
}

func TestEmptyStateEnvironmentDisablesPersistence(t *testing.T) {
	for _, value := range []string{"", "  ", filepath.Join(t.TempDir(), "queue.json.zst")} {
		t.Setenv(envPrefix+"STATE", value)
		cfg, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.StateFile != strings.TrimSpace(value) {
			t.Errorf("STATE=%q became %q", value, cfg.StateFile)
		}
		if cfg.LegacyStateFile != "" {
			t.Errorf("explicit STATE enabled migration from %q", cfg.LegacyStateFile)
		}
	}
}

func TestDefaultStatePathEnablesLegacyMigration(t *testing.T) {
	t.Setenv(envPrefix+"STATE", "")
	if err := os.Unsetenv(envPrefix + "STATE"); err != nil {
		t.Fatal(err)
	}
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LegacyStateFile == "" || cfg.StateFile != cfg.LegacyStateFile+".zst" {
		t.Fatalf("default paths = %q, %q", cfg.StateFile, cfg.LegacyStateFile)
	}
}

// The listing caps default to the built-in ones, zero included, and refuse
// a negative count: there is no "unlimited", only a larger number.
func TestListingCapsDefaultAndRefuseNegatives(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSources != MaxExpandedSources || cfg.MaxFiles != MaxListingFiles {
		t.Errorf("defaults = %d sources, %d files", cfg.MaxSources, cfg.MaxFiles)
	}

	c := *cfg
	c.DownloadDir = t.TempDir()
	c.MaxSources, c.MaxFiles = 0, 0
	if err := c.Prepare(); err != nil {
		t.Fatalf("zero caps: %v", err)
	}
	if c.MaxSources != MaxExpandedSources || c.MaxFiles != MaxListingFiles {
		t.Errorf("zero became %d sources, %d files; want the defaults", c.MaxSources, c.MaxFiles)
	}

	for _, bad := range []struct{ sources, files int }{{-1, 10}, {10, -1}} {
		c := *cfg
		c.DownloadDir = t.TempDir()
		c.MaxSources, c.MaxFiles = bad.sources, bad.files
		if err := c.Prepare(); err == nil {
			t.Errorf("%d sources, %d files was accepted", bad.sources, bad.files)
		}
	}
}

// CAPTCHAs read at once default to four, and the setting names another.
func TestTheCaptchaConcurrencyIsASetting(t *testing.T) {
	cfg := &Config{DownloadDir: t.TempDir(), Concurrency: 4, Streams: 8, StallTimeout: time.Minute}
	if err := cfg.Prepare(); err != nil || cfg.CaptchaConcurrency != DefaultCaptchaConcurrency || DefaultCaptchaConcurrency != 4 {
		t.Fatalf("default: %d, err %v; want %d", cfg.CaptchaConcurrency, err, 4)
	}
	cfg = &Config{DownloadDir: t.TempDir(), Concurrency: 4, Streams: 8, StallTimeout: time.Minute, CaptchaConcurrency: 8}
	if err := cfg.Prepare(); err != nil || cfg.CaptchaConcurrency != 8 {
		t.Fatalf("set to 8: %d, err %v", cfg.CaptchaConcurrency, err)
	}
}

// Download proxies can be held to one protocol, for every group or for one
// group alone, and the environment names both.
func TestProxyTypesFromTheEnvironment(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyType != ProxyTypeAny || len(cfg.ProxyTypes) != 0 {
		t.Errorf("defaults: %q %v; want any protocol for every group", cfg.ProxyType, cfg.ProxyTypes)
	}

	t.Setenv("HEAPLEACH_PROXY_TYPE", "socks")
	t.Setenv("HEAPLEACH_PROXY_TYPES", "Cloudflare=any,MEGA=http,keep2share=HTTP,EMLOAD=socks")
	if cfg, err = FromEnv(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"keep2share": ProxyTypeHTTP, "emload": ProxyTypeSOCKS, "mega": ProxyTypeHTTP, "cloudflare": ProxyTypeAny}
	if cfg.ProxyType != ProxyTypeSOCKS || !maps.Equal(cfg.ProxyTypes, want) {
		t.Errorf("read %q %v; want socks and %v", cfg.ProxyType, cfg.ProxyTypes, want)
	}
	if got := FormatProxyTypes(cfg.ProxyTypes); got != "keep2share=http,emload=socks,mega=http,cloudflare=any" {
		t.Errorf("written back as %q", got)
	}

	for _, bad := range []string{"keep2share=tor", "elsewhere=socks", "keep2share", "fileboom="} {
		t.Setenv("HEAPLEACH_PROXY_TYPES", bad)
		if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "HEAPLEACH_PROXY_TYPES") {
			t.Errorf("%q: err = %v, want it refused and named", bad, err)
		}
	}
}

// Prepare spells the protocol one way whatever it was given as, and no
// protocol at all is any.
func TestPrepareSpellsTheProxyProtocol(t *testing.T) {
	for raw, want := range map[string]string{"": ProxyTypeAny, " SOCKS ": ProxyTypeSOCKS, "Http": ProxyTypeHTTP} {
		cfg := &Config{DownloadDir: t.TempDir(), Concurrency: 4, Streams: 8, StallTimeout: time.Minute, ProxyType: raw}
		if err := cfg.Prepare(); err != nil || cfg.ProxyType != want {
			t.Errorf("%q prepared as %q, err %v; want %q", raw, cfg.ProxyType, err, want)
		}
	}
}
