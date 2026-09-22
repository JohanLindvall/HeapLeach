package download

import (
	"encoding/json"
	"fmt"
	"testing"
)

// snapshotWith builds a snapshot of two jobs, each with one fully populated
// item, which is what a real frame is made of.
func snapshotWith() Snapshot {
	item := func(id string) ItemView {
		return ItemView{
			ID: id, Name: id + ".mp4", Dir: "Album", Status: StatusRunning,
			Size: 1 << 30, Downloaded: 1 << 29, Speed: 1 << 20,
			Note: "waiting for a slot", Path: "Album/" + id + ".mp4",
			Streams: 4, SegmentsDone: 3, SegmentsTotal: 9, Elapsed: 12.5,
		}
	}
	// Enough items that they dominate the frame, as they do in a real one:
	// a job's own fields are a fixed couple of hundred bytes and the items
	// behind it are thousands.
	many := func(prefix string) []ItemView {
		out := make([]ItemView, 0, 50)
		for i := range 50 {
			out = append(out, item(fmt.Sprintf("%s%02d", prefix, i)))
		}
		return out
	}
	return Snapshot{Jobs: []JobView{
		{ID: "open", Title: "Open", Items: many("a")},
		{ID: "shut", Title: "Shut", Items: many("b")},
	}}
}

// A job the browser has open is sent whole; everything else keeps only what
// the views spanning the whole queue read. Those are the file search, which
// matches a name, and the progress panel, which counts finished files by id
// and adds up their bytes.
func TestTrimSnapshotKeepsOpenJobsWholeAndSlimsTheRest(t *testing.T) {
	out := trimSnapshot(snapshotWith(), newOpenSet([]string{"open"}))

	whole := out.Jobs[0].Items[0]
	if whole.Path == "" || whole.Note == "" || whole.Speed == 0 || whole.Streams == 0 {
		t.Errorf("an open job's item lost detail: %+v", whole)
	}

	slim := out.Jobs[1].Items[0]
	if slim.ID == "" || slim.Name == "" || slim.Status == "" ||
		slim.Size == 0 || slim.Downloaded == 0 {
		t.Errorf("a closed job's item lost what the queue-wide views need: %+v", slim)
	}
	if slim.Path != "" || slim.Note != "" || slim.Dir != "" ||
		slim.Speed != 0 || slim.Streams != 0 || slim.SegmentsTotal != 0 {
		t.Errorf("a closed job's item kept detail no one can see: %+v", slim)
	}
}

// A job small enough to be opened on sight is sent whole whatever the
// subscriber said, which is what lets the browser declare only the jobs it
// expanded by hand.
func TestTrimSnapshotSendsSmallJobsWhole(t *testing.T) {
	snap := Snapshot{Jobs: []JobView{{ID: "one", Items: []ItemView{
		{ID: "a", Name: "a.mp4", Status: StatusRunning, Note: "waiting", Path: "a.mp4"},
	}}}}
	out := trimSnapshot(snap, nil)
	if got := out.Jobs[0].Items[0]; got.Note == "" || got.Path == "" {
		t.Errorf("a one-file job was slimmed: %+v", got)
	}
}

// The snapshot handed in is read by the terminal display in process and by
// every other subscriber, so trimming must not reach back into it.
func TestTrimSnapshotLeavesTheOriginalAlone(t *testing.T) {
	snap := snapshotWith()
	trimSnapshot(snap, nil)

	if got := snap.Jobs[1].Items[0].Path; got == "" {
		t.Error("trimming emptied the snapshot it was given")
	}
	if got := snap.Jobs[0].Items[0].Note; got == "" {
		t.Error("trimming emptied the snapshot it was given")
	}
}

// The saving is the point, so it is worth measuring rather than assuming.
func TestTrimSnapshotIsSubstantiallySmallerOnTheWire(t *testing.T) {
	snap := snapshotWith()
	whole, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	slim, err := json.Marshal(trimSnapshot(snap, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(slim)*2 > len(whole) {
		t.Errorf("slimmed frame is %d bytes against %d whole; want it at least halved",
			len(slim), len(whole))
	}
	// And the URL, which was the largest thing in a frame, is gone from
	// both: nothing in the browser ever read it.
	for _, payload := range [][]byte{whole, slim} {
		var back map[string]any
		if err := json.Unmarshal(payload, &back); err != nil {
			t.Fatal(err)
		}
		jobs := back["jobs"].([]any)
		items := jobs[0].(map[string]any)["items"].([]any)
		if _, ok := items[0].(map[string]any)["url"]; ok {
			t.Error("the item URL is still on the wire")
		}
		if _, ok := items[0].(map[string]any)["elapsed"]; ok {
			t.Error("elapsed is still on the wire")
		}
	}
}

// Two browsers looking at the same thing share one encoding of it.
func TestOpenSetKeyIsOrderIndependent(t *testing.T) {
	a := newOpenSet([]string{"one", "two"})
	b := newOpenSet([]string{"two", " one "})
	if a.key() != b.key() {
		t.Errorf("keys differ: %q and %q", a.key(), b.key())
	}
	if newOpenSet(nil).key() != "" {
		t.Error("an empty set should key as empty")
	}
	if newOpenSet([]string{"", "  "}).key() != "" {
		t.Error("blank ids should be ignored")
	}
}
