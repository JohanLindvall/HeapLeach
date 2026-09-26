package download

import (
	"fmt"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Settings is a partial runtime update. Nil fields keep their current value.
type Settings struct {
	Concurrency *int    `json:"concurrency"`
	Streams     *int    `json:"streams"`
	Paused      *bool   `json:"paused"`
	SpeedLimit  *int64  `json:"speedLimit"`
	DownloadDir *string `json:"downloadDir"`
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

	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return ErrClosed
	}
	changed := false
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
	m.mu.Unlock()
	if s.Paused != nil && !*s.Paused {
		m.resumeRestored()
	}
	if changed {
		m.markDirty()
		m.signal()
	}
	return nil
}
