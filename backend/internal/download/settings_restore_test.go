// SPDX-License-Identifier: MIT

package download

import (
	"path/filepath"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// runManager starts a manager configured for concurrency and streams that
// keeps its queue in file, as the service does across a restart: Restore,
// then Start.
func runManager(t *testing.T, file string, concurrency, streams, most int) *Manager {
	t.Helper()
	m := testManager(t, func(c *config.Config) {
		c.Concurrency, c.Streams, c.MaxConcurrency, c.StateFile = concurrency, streams, most, file
	})
	if _, err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Start()
	return m
}

func liveSettings(m *Manager) (concurrency, streams int) {
	s := m.CurrentSettings()
	return *s.Concurrency, *s.Streams
}

// Parallel downloads and streams chosen in Settings outlive the process:
// every release restarts the service, and each restart used to put them back
// to the configured values. They are written when chosen, so a process that
// never shuts down cleanly keeps them too.
func TestSettingsChosenLiveSurviveARestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "queue.json.zst")
	first := runManager(t, file, 10, 8, 0)
	first.persist() // a queue already saved, unchanged but for the settings
	concurrency, streams := 64, 1
	if err := first.ApplySettings(Settings{Concurrency: &concurrency, Streams: &streams}); err != nil {
		t.Fatal(err)
	}
	// Read before the first manager closes: the change must already be on disk.
	saved, _, err := loadState(file, "")
	if err != nil || saved.Settings == nil || saved.Settings.Concurrency != 64 || saved.Settings.Streams != 1 {
		t.Fatalf("saved settings %+v (%v); want them written when chosen", saved.Settings, err)
	}
	first.Close()

	next := runManager(t, file, 10, 8, 0)
	defer next.Close()
	if c, s := liveSettings(next); c != 64 || s != 1 {
		t.Fatalf("after a restart: concurrency %d, streams %d; want the 64 and 1 chosen before it", c, s)
	}
}

// A configuration changed since a setting was chosen is the later word, and
// wins for that setting alone.
func TestAChangedConfigurationOverridesWhatWasChosen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "queue.json.zst")
	first := runManager(t, file, 10, 8, 0)
	concurrency, streams := 64, 1
	if err := first.ApplySettings(Settings{Concurrency: &concurrency, Streams: &streams}); err != nil {
		t.Fatal(err)
	}
	first.Close()

	next := runManager(t, file, 20, 8, 0) // the deployment now asks for 20
	if c, s := liveSettings(next); c != 20 || s != 1 {
		t.Fatalf("concurrency %d, streams %d; want the newly configured 20, and the 1 still chosen", c, s)
	}
	next.Close()

	// And having taken over, the configuration's value is what is kept.
	last := runManager(t, file, 20, 8, 0)
	defer last.Close()
	if c, _ := liveSettings(last); c != 20 {
		t.Fatalf("concurrency %d after another restart, want 20", c)
	}
}

// A ceiling lowered since cannot be exceeded by what was chosen under it.
func TestAKeptConcurrencyStaysUnderTheCeiling(t *testing.T) {
	file := filepath.Join(t.TempDir(), "queue.json.zst")
	first := runManager(t, file, 10, 8, 128)
	concurrency := 100
	if err := first.ApplySettings(Settings{Concurrency: &concurrency}); err != nil {
		t.Fatal(err)
	}
	first.Close()

	next := runManager(t, file, 10, 8, 32)
	defer next.Close()
	if c, _ := liveSettings(next); c != 32 {
		t.Fatalf("concurrency %d under a ceiling of 32", c)
	}
}

// A queue saved before settings were kept restores to the configuration.
func TestAQueueWithoutSettingsKeepsTheConfiguration(t *testing.T) {
	file := filepath.Join(t.TempDir(), "queue.json.zst")
	if err := saveState(file, &savedState{}); err != nil {
		t.Fatal(err)
	}
	m := runManager(t, file, 10, 8, 0)
	defer m.Close()
	if c, s := liveSettings(m); c != 10 || s != 8 {
		t.Fatalf("concurrency %d, streams %d; want the configured 10 and 8", c, s)
	}
}
