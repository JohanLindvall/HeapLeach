// SPDX-License-Identifier: MIT

package download

import (
	"fmt"
	"slices"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

// Settings is a partial runtime update. Nil fields keep their current value.
type Settings struct {
	Concurrency    *int      `json:"concurrency"`
	Streams        *int      `json:"streams"`
	Paused         *bool     `json:"paused"`
	SpeedLimit     *int64    `json:"speedLimit"`
	DownloadDir    *string   `json:"downloadDir"`
	Proxies        *bool     `json:"proxies"`
	ProxyEndpoints *[]string `json:"proxyEndpoints"`
	ProxyFeeds     *[]string `json:"proxyFeeds"`
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
	return Settings{Concurrency: &concurrency, Streams: &streams, Paused: &paused,
		SpeedLimit: &limit, DownloadDir: &dir, Proxies: &enabled,
		ProxyEndpoints: &endpoints, ProxyFeeds: &feeds}
}

// ApplySettings validates the whole update before changing any setting.
// Filesystem preparation happens outside mu; publishing the new settings
// happens together so an invalid field cannot leave a half-applied request.
func (m *Manager) ApplySettings(s Settings) error {
	defer m.nudge()
	if s.Concurrency != nil && (*s.Concurrency < 1 || *s.Concurrency > config.MaxConcurrency) {
		return fmt.Errorf("concurrency must be between 1 and %d", config.MaxConcurrency)
	}
	if s.Streams != nil && (*s.Streams < 1 || *s.Streams > config.MaxStreams) {
		return fmt.Errorf("streams must be between 1 and %d", config.MaxStreams)
	}
	if s.SpeedLimit != nil && *s.SpeedLimit < 0 {
		return fmt.Errorf("speed limit cannot be negative, got %d", *s.SpeedLimit)
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
	changed := false
	if proxyUpdate {
		changed = enabled != m.proxyEnabled || !slices.Equal(proxyConfig.Endpoints, m.proxyConfig.Endpoints) ||
			!slices.Equal(proxyConfig.Feeds, m.proxyConfig.Feeds)
		m.proxies, m.proxyEnabled, m.proxyConfig = pool, enabled, proxyConfig
		if pool != nil {
			pool.Configure(proxyConfig, enabled)
		}
		if changed && enabled {
			// A queued direct-address cooldown must not conceal the newly
			// available proxies. The K2S resolver still remembers that timer
			// if the pool tries the direct address again.
			for _, it := range m.queue {
				if proxyEligible(it) && !it.inFlight {
					it.notBefore, it.Note = time.Time{}, ""
				}
			}
		}
	}
	if s.Concurrency != nil && m.limit != *s.Concurrency {
		m.limit, changed = *s.Concurrency, true
	}
	if s.Streams != nil && m.streams != *s.Streams {
		m.streams, changed = *s.Streams, true
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
	return nil
}
