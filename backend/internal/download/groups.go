// SPDX-License-Identifier: MIT

package download

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// A transfer group is what the queue's limits can be lowered for: a
// service's pacing group where its host declares one (keep2share, fileboom,
// or cloudflare for WAF recovery), and otherwise the site the job was added
// from. A GroupLimit sets how many of the group's files download at once and
// how many streams each may use, beneath the queue's own settings, which
// still apply over it. A group's limit replaces what its host asks for
// itself, within those settings. Files of 0 holds the group's new
// downloads; those running finish.

// GroupLimit is one group's limits. A field left out leaves that to the
// queue's settings and the host's own pacing.
type GroupLimit struct {
	Files   *int `json:"files,omitempty"`
	Streams *int `json:"streams,omitempty"`
}

// groupOfLocked names the transfer group it belongs to. Caller holds mu.
func (m *Manager) groupOfLocked(it *Item) string {
	if it.pace != nil && it.pace.Group != "" {
		return it.pace.Group
	}
	if job := m.jobs[it.JobID]; job != nil {
		if name := util.FirstNonEmpty(job.Host, hostOf(job.Source)); name != "" {
			return name
		}
	}
	return hostOf(it.URL)
}

// groupHeldLocked reports that it's group may start no more files now:
// as many as its limit allows are running, or it allows none. The item's
// note says which. Caller holds mu.
func (m *Manager) groupHeldLocked(it *Item) bool {
	name := m.groupOfLocked(it)
	limit, ok := m.groupLimits[name]
	if !ok || limit.Files == nil || m.groupActive[name] < *limit.Files {
		return false
	}
	if *limit.Files == 0 {
		it.Note = "Paused: new " + name + " downloads are held"
	} else {
		it.Note = fmt.Sprintf("Waiting: %s is limited to %d at a time", name, *limit.Files)
	}
	// A wait that ran out before now would hide the note (itemNoteLocked).
	it.notBefore = time.Time{}
	return true
}

// groupStreamsLocked is the most streams a file of it's group may use, or 0
// when the group sets none. Caller holds mu.
func (m *Manager) groupStreamsLocked(it *Item) int {
	if limit, ok := m.groupLimits[m.groupOfLocked(it)]; ok && limit.Streams != nil {
		return *limit.Streams
	}
	return 0
}

// groupsLocked lists the transfer groups that have files still to download,
// and those with limits set, for the settings panel to offer. Caller holds
// mu.
func (m *Manager) groupsLocked() []string {
	seen := make(map[string]bool, len(m.groupLimits)+4)
	for name := range m.groupLimits {
		seen[name] = true
	}
	for _, job := range m.jobs {
		for _, it := range job.Items {
			if it.Status == StatusQueued || it.Status == StatusRunning {
				seen[m.groupOfLocked(it)] = true
			}
		}
	}
	delete(seen, "")
	return slices.Sorted(maps.Keys(seen))
}

// checkGroupLimits refuses limits a group cannot have: a name of nothing,
// more files than the queue's ceiling, or streams outside what one file may
// ever use. most is the ceiling on files.
func checkGroupLimits(limits map[string]GroupLimit, most int) error {
	if len(limits) > config.MaxGroupLimits {
		return fmt.Errorf("at most %d groups may have limits", config.MaxGroupLimits)
	}
	for name, limit := range limits {
		if strings.TrimSpace(name) == "" || len(name) > config.MaxGroupNameLength {
			return fmt.Errorf("invalid group name %q", name)
		}
		if limit.Files != nil && (*limit.Files < 0 || *limit.Files > most) {
			return fmt.Errorf("files for %s must be between 0 and %d", name, most)
		}
		if limit.Streams != nil && (*limit.Streams < 1 || *limit.Streams > config.MaxStreams) {
			return fmt.Errorf("streams for %s must be between 1 and %d", name, config.MaxStreams)
		}
	}
	return nil
}

// cloneGroupLimits copies limits, leaving out groups that set nothing.
func cloneGroupLimits(limits map[string]GroupLimit) map[string]GroupLimit {
	out := make(map[string]GroupLimit, len(limits))
	for name, limit := range limits {
		var kept GroupLimit
		if limit.Files != nil {
			kept.Files = intRef(*limit.Files)
		}
		if limit.Streams != nil {
			kept.Streams = intRef(*limit.Streams)
		}
		if kept.Files != nil || kept.Streams != nil {
			out[strings.TrimSpace(name)] = kept
		}
	}
	return out
}

func sameGroupLimits(a, b map[string]GroupLimit) bool {
	return maps.EqualFunc(a, b, func(x, y GroupLimit) bool {
		return sameLimit(x.Files, y.Files) && sameLimit(x.Streams, y.Streams)
	})
}

func intRef(v int) *int { return &v }

func sameLimit(a, b *int) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
