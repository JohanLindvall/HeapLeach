package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFromEnvDefaults(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" || cfg.Concurrency != DefaultConcurrency ||
		cfg.MaxRetries != DefaultMaxRetries || cfg.Streams != DefaultStreams {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.UserAgent == "" || cfg.Language == "" {
		t.Error("a UA and a language must always be set")
	}
}

func TestFromEnvReadsTheEnvironment(t *testing.T) {
	t.Setenv("HEAPLEACH_ADDR", ":9999")
	t.Setenv("HEAPLEACH_CONCURRENCY", "7")
	t.Setenv("HEAPLEACH_MAX_SPEED", "1000000")
	t.Setenv("HEAPLEACH_STALL_TIMEOUT", "45s")
	t.Setenv("HEAPLEACH_KVS_HOSTS", "one.example, two.example  three.example")
	t.Setenv("HEAPLEACH_DEBUG", "1")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9999" || cfg.Concurrency != 7 || cfg.SpeedLimit != 1000000 ||
		cfg.StallTimeout != 45*time.Second || !cfg.Debug {
		t.Errorf("cfg = %+v", cfg)
	}
	kvs := cfg.ExtraHostsFor(FamilyKVS)
	if len(kvs) != 3 || kvs[2] != "three.example" {
		t.Errorf("kvs hosts = %v, want commas and spaces both to separate", kvs)
	}
}

// One setting carries every family's additions; the original KVS-only
// variable still works, because someone is relying on it.
func TestExtraHostsGroupsByFamily(t *testing.T) {
	t.Setenv("HEAPLEACH_KVS_HOSTS", "tube.example")
	t.Setenv("HEAPLEACH_EXTRA_HOSTS",
		"peertube:one.example,two.example; chevereto:pics.example ;kvs:another.example")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ExtraHostsFor(FamilyPeerTube); len(got) != 2 || got[0] != "one.example" {
		t.Errorf("peertube = %v", got)
	}
	if got := cfg.ExtraHostsFor(FamilyChevereto); len(got) != 1 || got[0] != "pics.example" {
		t.Errorf("chevereto = %v", got)
	}
	// The legacy variable and the new one name the same family and both
	// count, rather than one silently replacing the other.
	if got := cfg.ExtraHostsFor(FamilyKVS); len(got) != 2 {
		t.Errorf("kvs = %v, want the legacy variable and the grouped one merged", got)
	}
	if got := cfg.ExtraHostsFor("nothing-configured"); got != nil {
		t.Errorf("unconfigured family = %v, want nil", got)
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
		{"too much concurrency", func(c *Config) { c.Concurrency = MaxConcurrency + 1 }, "concurrency"},
		{"negative retries", func(c *Config) { c.MaxRetries = -1 }, "retries"},
		{"zero streams", func(c *Config) { c.Streams = 0 }, "streams"},
		{"negative slow speed", func(c *Config) { c.SlowSpeed = -1 }, "slow-speed"},
		{"negative ceiling", func(c *Config) { c.SpeedLimit = -1 }, "max-speed"},
		{"no stall timeout", func(c *Config) { c.StallTimeout = 0 }, "stall-timeout"},
		{"blank directory", func(c *Config) { c.DownloadDir = "   " }, "directory"},
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
