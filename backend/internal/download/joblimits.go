// SPDX-License-Identifier: MIT

package download

import (
	"fmt"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// A job's limits lower the queue's own for that job alone: how many of its
// files download at once, how many streams each may use, and how fast they
// may go together. The queue's settings still apply over them, so a job's
// limit can only ever hold it lower. Files of 0 holds the job's new
// downloads; those running finish. A job's limit on streams replaces what
// its host asks for itself, within the queue's.

// JobLimit is one job's limits. A field left out leaves that to the queue's
// settings and the host's own pacing.
type JobLimit struct {
	Files   *int `json:"fileLimit,omitempty"`
	Streams *int `json:"streamLimit,omitempty"`
	// SpeedLimit caps the job's transfers together, in bytes per second,
	// where zero is no cap of its own. It is a bucket of the job's own,
	// drawn on before the queue's (throttledReader).
	SpeedLimit int64 `json:"speedLimit,omitempty"`
}

// checkJobLimit refuses limits a job cannot have: more files than the
// queue's ceiling, streams outside what one file may ever use, or a speed
// below nothing. most is the ceiling on files.
func checkJobLimit(l JobLimit, most int) error {
	if l.Files != nil && (*l.Files < 0 || *l.Files > most) {
		return fmt.Errorf("files at once must be between 0 and %d", most)
	}
	if l.Streams != nil && (*l.Streams < 1 || *l.Streams > config.MaxStreams) {
		return fmt.Errorf("streams must be between 1 and %d", config.MaxStreams)
	}
	if l.SpeedLimit < 0 {
		return fmt.Errorf("speed limit cannot be negative, got %d", l.SpeedLimit)
	}
	return nil
}

// clone copies l. A job's limit is replaced whole, never changed in place, so
// a view or a saved queue built from the old one goes on reading what it was.
func (l JobLimit) clone() JobLimit {
	out := JobLimit{SpeedLimit: l.SpeedLimit}
	if l.Files != nil {
		out.Files = intRef(*l.Files)
	}
	if l.Streams != nil {
		out.Streams = intRef(*l.Streams)
	}
	return out
}

func (l JobLimit) same(o JobLimit) bool {
	return sameLimit(l.Files, o.Files) && sameLimit(l.Streams, o.Streams) && l.SpeedLimit == o.SpeedLimit
}

// restoredJobLimit takes back a saved limit under the ceiling in force now,
// which a configuration may have lowered since; what no build would accept
// is dropped rather than failing the job.
func restoredJobLimit(l JobLimit, most int) JobLimit {
	l = l.clone()
	if l.Files != nil {
		if *l.Files < 0 {
			l.Files = nil
		} else {
			*l.Files = min(*l.Files, most)
		}
	}
	if l.Streams != nil {
		if *l.Streams < 1 {
			l.Streams = nil
		} else {
			*l.Streams = min(*l.Streams, config.MaxStreams)
		}
	}
	l.SpeedLimit = max(l.SpeedLimit, 0)
	return l
}

// SetJobLimit replaces a job's limits. A field left out has none of its own.
func (m *Manager) SetJobLimit(id string, limit JobLimit) error {
	// Something the user did, so they see it at once. See nudge.
	defer m.nudge()
	if err := checkJobLimit(limit, m.maxConcurrency()); err != nil {
		return err
	}
	limit = limit.clone()
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return ErrClosed
	}
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	if job.Limit.same(limit) {
		m.mu.Unlock()
		return nil
	}
	job.Limit = limit
	// In place, so transfers already reading through the bucket are held to
	// the new speed at once rather than from their next attempt.
	job.limiter.setLimit(limit.SpeedLimit)
	if limit.SpeedLimit > 0 {
		// A transfer held to the job's speed says nothing about its route.
		for _, it := range job.Items {
			if it.route != nil {
				it.route.Constrain()
			}
		}
	}
	m.mu.Unlock()
	m.log.Info("job limits set", "job", id, "files", limitText(limit.Files), "streams", limitText(limit.Streams),
		"speed_limit", limit.SpeedLimit)

	m.markDirty()
	m.signal()
	// Written now rather than at the next save, like the queue's own
	// settings: a restart before that would quietly lose the change.
	m.persist()
	return nil
}

// jobHeldLocked reports that it's job may start no more of its files now:
// as many as its limit allows are running, or it allows none. Caller holds
// mu.
func (m *Manager) jobHeldLocked(it *Item) bool {
	job := m.jobs[it.JobID]
	return job != nil && job.Limit.Files != nil && m.jobActive[it.JobID] >= *job.Limit.Files
}

// jobHoldNoteLocked says why a queued item is held by its job's limit, or
// nothing when it is not. It is rendered per snapshot rather than stored, so
// it follows the limit as it is changed, and is gone the moment it is lifted.
// Caller holds mu.
func (m *Manager) jobHoldNoteLocked(it *Item) string {
	if it.Status != StatusQueued || it.inFlight || !m.jobHeldLocked(it) {
		return ""
	}
	job := m.jobs[it.JobID]
	if job.restored {
		// Its own note says what starts it again.
		return ""
	}
	if *job.Limit.Files == 0 {
		return "Paused: new downloads of this job are held"
	}
	return fmt.Sprintf("Waiting: this job is limited to %d at a time", *job.Limit.Files)
}

// jobStreamsLocked is the most streams a file of it's job may use, or 0 when
// the job sets none. Caller holds mu.
func (m *Manager) jobStreamsLocked(it *Item) int {
	if job := m.jobs[it.JobID]; job != nil && job.Limit.Streams != nil {
		return *job.Limit.Streams
	}
	return 0
}

func limitText(v *int) string {
	if v == nil {
		return "queue's"
	}
	return fmt.Sprint(*v)
}

func intRef(v int) *int { return &v }

func sameLimit(a, b *int) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
