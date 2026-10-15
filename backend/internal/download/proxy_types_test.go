// SPDX-License-Identifier: MIT

package download

import (
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

const (
	typesHTTPRoute  = "http://127.0.0.2:8080"
	typesSOCKSRoute = "socks5h://127.0.0.3:1080"
)

var typesEveryRoute = []string{proxy.Direct, typesHTTPRoute, typesSOCKSRoute}

// openTypesPool opens m's proxy pool on the direct connection, one HTTP
// route and one SOCKS route.
func openTypesPool(t *testing.T, m *Manager) {
	t.Helper()
	m.cfg.ProxyDB = filepath.Join(t.TempDir(), "proxies.db")
	enabled, endpoints, feeds := true, slices.Clone(typesEveryRoute), []string{}
	if err := m.ApplySettings(Settings{Proxies: &enabled, ProxyEndpoints: &endpoints, ProxyFeeds: &feeds}); err != nil {
		t.Fatal(err)
	}
}

// leasedRoutes is every route the group is given while it holds the ones it
// was given before, in order, handed back at the end.
func leasedRoutes(m *Manager, group string) []string {
	m.mu.Lock()
	pool := m.proxies
	m.mu.Unlock()
	var routes []string
	for range len(typesEveryRoute) + 1 {
		l := pool.AcquireFor(group, "", 1, 1<<20, false)
		if l == nil {
			break
		}
		defer l.Release()
		routes = append(routes, l.Endpoint())
	}
	slices.Sort(routes)
	return routes
}

// Each proxy group draws its routes from its own protocol where it has one,
// and from the protocol all the groups do where it has not; a specific
// protocol leaves the direct connection out.
func TestProxyTypesHoldEachGroupToItsProtocol(t *testing.T) {
	m := busyManager(t)
	openTypesPool(t, m)
	every := slices.Sorted(slices.Values(typesEveryRoute))
	if got := leasedRoutes(m, "keep2share"); !slices.Equal(got, every) {
		t.Fatalf("with any protocol, keep2share leased %v; want %v", got, every)
	}

	all, own := config.ProxyTypeHTTP, map[string]string{"keep2share": config.ProxyTypeSOCKS}
	if err := m.ApplySettings(Settings{ProxyType: &all, ProxyTypes: &own}); err != nil {
		t.Fatal(err)
	}
	for group, want := range map[string]string{"keep2share": typesSOCKSRoute, "fileboom": typesHTTPRoute, "cloudflare": typesHTTPRoute} {
		if got := leasedRoutes(m, group); !slices.Equal(got, []string{want}) {
			t.Errorf("%s leased %v, want %s alone", group, got, want)
		}
	}
	snap := m.Snapshot()
	if snap.ProxyType != config.ProxyTypeHTTP || !maps.Equal(snap.ProxyTypes, own) {
		t.Errorf("snapshot shows %q %v", snap.ProxyType, snap.ProxyTypes)
	}

	// A change names the groups it changes, and an empty protocol has the
	// group follow the others again.
	fileboom := map[string]string{"fileboom": config.ProxyTypeSOCKS}
	if err := m.ApplySettings(Settings{ProxyTypes: &fileboom}); err != nil {
		t.Fatal(err)
	}
	if kinds := *m.CurrentSettings().ProxyTypes; !maps.Equal(kinds, map[string]string{"keep2share": config.ProxyTypeSOCKS, "fileboom": config.ProxyTypeSOCKS}) {
		t.Errorf("after changing one group, the groups hold %v", kinds)
	}
	cleared := map[string]string{"keep2share": "", "fileboom": ""}
	if err := m.ApplySettings(Settings{ProxyTypes: &cleared}); err != nil {
		t.Fatal(err)
	}
	if got := leasedRoutes(m, "keep2share"); !slices.Equal(got, []string{typesHTTPRoute}) {
		t.Errorf("keep2share leased %v once its own protocol was cleared, want the HTTP route alone", got)
	}
	if kinds := *m.CurrentSettings().ProxyTypes; len(kinds) != 0 {
		t.Errorf("settings keep %v", kinds)
	}

	anything := config.ProxyTypeAny
	if err := m.ApplySettings(Settings{ProxyType: &anything}); err != nil {
		t.Fatal(err)
	}
	if got := leasedRoutes(m, "cloudflare"); !slices.Equal(got, every) {
		t.Errorf("with any protocol again, cloudflare leased %v; want %v", got, every)
	}
}

// A protocol or a group the settings do not know is refused, and nothing of
// the update is applied.
func TestProxyTypesRefuseTheUnknown(t *testing.T) {
	m := busyManager(t)
	concurrency := 3
	for name, s := range map[string]Settings{
		"protocol":       {ProxyType: ptr("tor")},
		"group":          {ProxyTypes: &map[string]string{"elsewhere": config.ProxyTypeSOCKS}},
		"group protocol": {ProxyTypes: &map[string]string{"fileboom": "ftp"}},
	} {
		s.Concurrency = &concurrency
		if err := m.ApplySettings(s); err == nil {
			t.Errorf("%s: accepted %+v", name, s)
		}
	}
	if got := m.CurrentSettings(); *got.Concurrency == 3 || *got.ProxyType != config.ProxyTypeAny || len(*got.ProxyTypes) != 0 {
		t.Errorf("a refused update was applied: concurrency %d, type %q, types %v", *got.Concurrency, *got.ProxyType, *got.ProxyTypes)
	}
}

func ptr[T any](v T) *T { return &v }

// typesManager is a manager keeping its queue in file, configured with the
// protocols given, neither restored nor started.
func typesManager(t *testing.T, file, kind string, kinds map[string]string) *Manager {
	cfg := &config.Config{
		DownloadDir: t.TempDir(), Concurrency: 4, Streams: 2, StateFile: file,
		UserAgent: config.DefaultUserAgent, Language: config.DefaultLanguage,
		GofileSecret: config.FallbackGofileSecret, Timeout: 30 * time.Second,
		ProxyType: kind, ProxyTypes: kinds,
	}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), 0, cfg.Timeout)
	return New(cfg, extractor.NewRegistry(cfg, client), client, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// The protocols chosen outlive the process, as the other settings do, while
// the configuration they overrode still says the same; a configuration
// changed since is the later word.
func TestProxyTypesSurviveARestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "queue.json.zst")
	run := func(kind string, kinds map[string]string) *Manager {
		t.Helper()
		m := typesManager(t, file, kind, kinds)
		if _, err := m.Restore(); err != nil {
			t.Fatal(err)
		}
		m.Start()
		return m
	}

	first := run(config.ProxyTypeAny, map[string]string{})
	first.persist() // a queue already saved, unchanged but for the settings
	// Each change is written when chosen: read before the manager closes.
	saved := func() *savedSettings {
		t.Helper()
		st, _, err := loadState(file, "")
		if err != nil || st.Settings == nil {
			t.Fatalf("saved settings %v (%v)", st, err)
		}
		return st.Settings
	}
	all, own := config.ProxyTypeSOCKS, map[string]string{"cloudflare": config.ProxyTypeAny}
	if err := first.ApplySettings(Settings{ProxyType: &all}); err != nil {
		t.Fatal(err)
	}
	if s := saved(); s.ProxyType != all {
		t.Fatalf("saved %q; want the protocol written when chosen", s.ProxyType)
	}
	if err := first.ApplySettings(Settings{ProxyTypes: &own}); err != nil {
		t.Fatal(err)
	}
	if s := saved(); !maps.Equal(s.ProxyTypes, own) {
		t.Fatalf("saved %v; want the group's protocol written when chosen", s.ProxyTypes)
	}
	first.Close()

	next := run(config.ProxyTypeAny, map[string]string{})
	if s := next.CurrentSettings(); *s.ProxyType != config.ProxyTypeSOCKS || !maps.Equal(*s.ProxyTypes, own) {
		t.Errorf("after a restart: %q %v; want the protocols chosen", *s.ProxyType, *s.ProxyTypes)
	}
	cleared := map[string]string{"cloudflare": ""}
	if err := next.ApplySettings(Settings{ProxyTypes: &cleared}); err != nil {
		t.Fatal(err)
	}
	next.Close()

	// No group overrides is a choice too, kept over the configuration's.
	again := run(config.ProxyTypeAny, map[string]string{})
	if s := again.CurrentSettings(); len(*s.ProxyTypes) != 0 {
		t.Errorf("after a restart: %v; want the overrides cleared", *s.ProxyTypes)
	}
	again.Close()

	changed := run(config.ProxyTypeHTTP, map[string]string{"keep2share": config.ProxyTypeSOCKS})
	defer changed.Close()
	if s := changed.CurrentSettings(); *s.ProxyType != config.ProxyTypeHTTP || (*s.ProxyTypes)["keep2share"] != config.ProxyTypeSOCKS {
		t.Errorf("after a configuration change: %q %v; want the configuration's", *s.ProxyType, *s.ProxyTypes)
	}
}

// The protocols kept with the queue hold the pool, which is opened before
// the queue is read.
func TestRestoredProxyTypesHoldThePoolOpenedBeforeThem(t *testing.T) {
	file := filepath.Join(t.TempDir(), "queue.json.zst")
	first := typesManager(t, file, config.ProxyTypeAny, map[string]string{})
	if _, err := first.Restore(); err != nil {
		t.Fatal(err)
	}
	first.Start()
	socks, own := config.ProxyTypeSOCKS, map[string]string{"cloudflare": config.ProxyTypeHTTP}
	if err := first.ApplySettings(Settings{ProxyType: &socks, ProxyTypes: &own}); err != nil {
		t.Fatal(err)
	}
	first.Close()

	next := typesManager(t, file, config.ProxyTypeAny, map[string]string{})
	defer next.Close()
	openTypesPool(t, next)
	if got := leasedRoutes(next, "keep2share"); len(got) != len(typesEveryRoute) {
		t.Fatalf("before the queue was read, keep2share leased %v; want every route", got)
	}
	if _, err := next.Restore(); err != nil {
		t.Fatal(err)
	}
	next.Start()
	for group, want := range map[string]string{"keep2share": typesSOCKSRoute, "fileboom": typesSOCKSRoute, "cloudflare": typesHTTPRoute} {
		if got := leasedRoutes(next, group); !slices.Equal(got, []string{want}) {
			t.Errorf("after the queue was read, %s leased %v; want %s alone", group, got, want)
		}
	}
}

// The configured protocols hold the pool from the moment it opens.
func TestConfiguredProxyTypesHoldThePoolAsItOpens(t *testing.T) {
	m := typesManager(t, "", config.ProxyTypeSOCKS, map[string]string{"cloudflare": config.ProxyTypeAny})
	defer m.Close()
	openTypesPool(t, m)
	if got := leasedRoutes(m, "fileboom"); !slices.Equal(got, []string{typesSOCKSRoute}) {
		t.Errorf("fileboom leased %v; want the SOCKS route alone", got)
	}
	if got := leasedRoutes(m, "cloudflare"); len(got) != len(typesEveryRoute) {
		t.Errorf("cloudflare leased %v; want every route", got)
	}
}
