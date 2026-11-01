// SPDX-License-Identifier: MIT

package download

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

// Settings is a partial runtime update. Nil fields keep their current value.
type Settings struct {
	// Concurrency resizes the pool without interrupting running transfers;
	// Streams changes the connection ceiling for new transfers.
	Concurrency *int `json:"concurrency"`
	Streams     *int `json:"streams"`
	// Paused stops or resumes the whole queue. Pausing holds the
	// connections open rather than dropping them: a running transfer parks
	// inside its read, and the dispatcher stops handing out work. A long
	// pause may still cost a connection to a server that times it out,
	// which the usual retry and resume handle.
	Paused *bool `json:"paused"`
	// SpeedLimit caps total throughput in bytes per second; zero is
	// unlimited. The cap is shared: it bounds everything moving at once,
	// not each transfer separately.
	SpeedLimit *int64 `json:"speedLimit"`
	// DownloadDir moves the destination, creating the directory and proving
	// it writable first — the same checks startup makes, since a directory
	// named from the UI is no more trustworthy than one named on the command
	// line.
	//
	// Transfers already running keep the destination they started with:
	// their path was settled when the worker took the item, and moving a
	// part file mid-flight would break the resume that part file exists
	// for. Everything still queued goes to the new place. That is worth
	// knowing rather than hiding, so the API says it back to the caller.
	DownloadDir    *string   `json:"downloadDir"`
	Proxies        *bool     `json:"proxies"`
	ProxyEndpoints *[]string `json:"proxyEndpoints"`
	ProxyFeeds     *[]string `json:"proxyFeeds"`
	// ProxyType is the protocol every proxy group draws its routes from:
	// "any", "socks" or "http". ProxyTypes changes the groups it names, to
	// a protocol of their own or, with "", back to ProxyType's; the groups
	// it leaves out keep what they have, so two changes made at once to two
	// groups cannot undo each other.
	ProxyType  *string            `json:"proxyType"`
	ProxyTypes *map[string]string `json:"proxyTypes"`
}

// CurrentSettings is fetched explicitly by the editor. Endpoint credentials
// never appear in the inventory or the frequent SSE queue snapshots.
func (m *Manager) CurrentSettings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	concurrency, streams := m.limit, m.streams
	paused, limit, dir := m.throttle.isPaused(), m.throttle.currentLimit(), m.DownloadDir()
	enabled := m.proxyEnabled
	endpoints := append([]string{}, m.proxyConfig.Endpoints...)
	feeds := append([]string{}, m.proxyConfig.Feeds...)
	kind, kinds := m.proxyType, m.proxyTypesLocked()
	return Settings{Concurrency: &concurrency, Streams: &streams, Paused: &paused,
		SpeedLimit: &limit, DownloadDir: &dir, Proxies: &enabled,
		ProxyEndpoints: &endpoints, ProxyFeeds: &feeds, ProxyType: &kind, ProxyTypes: &kinds}
}

// maxConcurrency is the configured ceiling on parallel transfers. A manager
// built without a config, as tests build them, gets the default.
func (m *Manager) maxConcurrency() int {
	if m.cfg != nil && m.cfg.MaxConcurrency > 0 {
		return m.cfg.MaxConcurrency
	}
	return config.DefaultMaxConcurrency
}

// ApplySettings validates the whole update before changing any setting.
// Filesystem preparation happens outside mu; publishing the new settings
// happens together so an invalid field cannot leave a half-applied request.
func (m *Manager) ApplySettings(s Settings) error {
	defer m.nudge()
	if most := m.maxConcurrency(); s.Concurrency != nil && (*s.Concurrency < 1 || *s.Concurrency > most) {
		return fmt.Errorf("concurrency must be between 1 and %d", most)
	}
	if s.Streams != nil && (*s.Streams < 1 || *s.Streams > config.MaxStreams) {
		return fmt.Errorf("streams must be between 1 and %d", config.MaxStreams)
	}
	if s.SpeedLimit != nil && *s.SpeedLimit < 0 {
		return fmt.Errorf("speed limit cannot be negative, got %d", *s.SpeedLimit)
	}
	if s.ProxyType != nil {
		if err := config.CheckProxyType(*s.ProxyType); err != nil {
			return err
		}
	}
	if s.ProxyTypes != nil {
		kinds, err := config.CheckProxyTypes(*s.ProxyTypes)
		if err != nil {
			return err
		}
		s.ProxyTypes = &kinds
	}
	var dir string
	if s.DownloadDir != nil {
		var err error
		dir, err = config.PrepareDir(*s.DownloadDir)
		if err != nil {
			return err
		}
	}

	m.settingsMu.Lock()
	defer m.settingsMu.Unlock()
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return ErrClosed
	}
	pool, enabled, proxyConfig := m.proxies, m.proxyEnabled, m.proxyConfig
	m.mu.Unlock()
	proxyUpdate := s.Proxies != nil || s.ProxyEndpoints != nil || s.ProxyFeeds != nil
	if proxyUpdate {
		if s.Proxies != nil {
			enabled = *s.Proxies
		}
		if s.ProxyEndpoints != nil {
			proxyConfig.Endpoints = *s.ProxyEndpoints
		}
		if s.ProxyFeeds != nil {
			proxyConfig.Feeds = *s.ProxyFeeds
		}
		var err error
		proxyConfig, err = proxy.ParseConfiguration(proxyConfig.Endpoints, proxyConfig.Feeds)
		if err != nil {
			return err
		}
		if enabled && len(proxyConfig.Endpoints)+len(proxyConfig.Feeds) == 0 {
			return fmt.Errorf("proxy pool needs endpoints or discovery feeds")
		}
		if enabled && pool == nil {
			pool, err = proxy.Open(m.cfg.ProxyDB, proxyConfig.Endpoints, proxyConfig.Feeds, m.client, m.log)
			if err != nil {
				return err
			}
		}
	}

	m.mu.Lock()
	changed, kept := false, false
	if proxyUpdate {
		changed = enabled != m.proxyEnabled || !slices.Equal(proxyConfig.Endpoints, m.proxyConfig.Endpoints) ||
			!slices.Equal(proxyConfig.Feeds, m.proxyConfig.Feeds)
		m.proxies, m.proxyEnabled, m.proxyConfig = pool, enabled, proxyConfig
		if pool != nil {
			pool.Configure(proxyConfig, enabled)
		}
		m.applyProxyTypesLocked()
		if changed && enabled {
			// A queued direct-address cooldown must not conceal the newly
			// available proxies. The host's resolver still remembers that timer
			// if the pool tries the direct address again.
			now := time.Now()
			for _, it := range m.queue {
				if it.inFlight {
					continue
				}
				// A file the normal connection was made to wait for goes on
				// through the routes now on offer, as moveOntoRoutes does.
				if it.routePace != nil && it.notBefore.After(now) {
					it.takeRoutePace()
				}
				if proxyEligible(it) {
					it.notBefore, it.Note = time.Time{}, ""
				}
			}
		}
	}
	if s.ProxyType != nil && m.proxyType != *s.ProxyType {
		m.proxyType, changed, kept = *s.ProxyType, true, true
	}
	if s.ProxyTypes != nil {
		kinds := m.proxyTypesLocked()
		for group, kind := range *s.ProxyTypes {
			if kind == "" {
				delete(kinds, group) // the group follows the others again
			} else {
				kinds[group] = kind
			}
		}
		if !maps.Equal(m.proxyTypes, kinds) {
			m.proxyTypes, changed, kept = kinds, true, true
		}
	}
	if s.ProxyType != nil || s.ProxyTypes != nil {
		m.applyProxyTypesLocked()
	}
	if s.Concurrency != nil && m.limit != *s.Concurrency {
		m.limit, changed, kept = *s.Concurrency, true, true
	}
	if s.Streams != nil && m.streams != *s.Streams {
		m.streams, changed, kept = *s.Streams, true, true
	}
	if s.SpeedLimit != nil && m.throttle.currentLimit() != *s.SpeedLimit {
		m.throttle.setLimit(*s.SpeedLimit)
		changed = true
	}
	if s.Paused != nil && m.throttle.isPaused() != *s.Paused {
		m.throttle.setPaused(*s.Paused)
		changed = true
	}
	if s.DownloadDir != nil {
		m.dirMu.Lock()
		if m.dir != dir {
			m.dir, changed = dir, true
		}
		m.dirMu.Unlock()
	}
	m.updateProxyDemandLocked()
	m.constrainProxyMeasurementsLocked()
	m.mu.Unlock()
	if proxyUpdate && enabled && pool != nil {
		pool.Start(m.ctx)
	}
	if s.Paused != nil && !*s.Paused {
		m.resumeRestored()
	}
	if changed {
		m.markDirty()
		m.signal()
	}
	if kept {
		// Written now rather than at the next save: these outlive the
		// process (restoreSettingsLocked), and a crash before the next save
		// would quietly lose the change.
		m.persist()
	}
	return nil
}

// proxyTypesLocked is a copy of the groups' own protocols, never nil, so
// the wire shows an empty object. Caller holds mu.
func (m *Manager) proxyTypesLocked() map[string]string {
	kinds := maps.Clone(m.proxyTypes)
	if kinds == nil {
		kinds = map[string]string{}
	}
	return kinds
}

// applyProxyTypesLocked holds each proxy group's routes to its protocol, in a
// pool that is open. A change holds from the group's next lease on; leases
// granted before are left to finish. Caller holds mu.
func (m *Manager) applyProxyTypesLocked() {
	if m.proxies == nil {
		return
	}
	if err := proxy.SetTypes(m.proxies, m.proxyType, m.proxyTypes); err != nil {
		m.log.Warn("could not hold the proxy groups to their protocols", "error", err)
	}
}
