package download

import (
	"testing"
)

// patchManager is a manager with one job of three items, enough to tell a
// patched frame from a whole one.
func patchManager() (*Manager, *Job) {
	job := &Job{ID: "j", Title: "Job", Items: []*Item{
		{ID: "a", Name: "a.mp4", Status: StatusDone},
		{ID: "b", Name: "b.mp4", Status: StatusRunning},
		{ID: "c", Name: "c.mp4", Status: StatusQueued},
	}}
	m := &Manager{jobs: map[string]*Job{"j": job}, order: []string{"j"}}
	return m, job
}

// The first frame carries everything, because a browser has nothing to
// merge a patch into.
func TestPatchSendsTheWholeListTheFirstTime(t *testing.T) {
	m, _ := patchManager()
	out := m.patchLocked(m.snapshotLocked())

	if out.Jobs[0].Patch {
		t.Error("the first frame was marked a patch")
	}
	if got := len(out.Jobs[0].Items); got != 3 {
		t.Errorf("first frame carried %d items, want all 3", got)
	}
}

// And after that, only what moved. This is the whole point: a thousand
// finished files say exactly what they said a second ago.
func TestPatchCarriesOnlyWhatChanged(t *testing.T) {
	m, job := patchManager()
	m.patchLocked(m.snapshotLocked()) // the first, whole frame

	out := m.patchLocked(m.snapshotLocked())
	if !out.Jobs[0].Patch {
		t.Error("a frame with nothing new was not marked a patch")
	}
	if got := len(out.Jobs[0].Items); got != 0 {
		t.Errorf("a frame with nothing new carried %d items, want none", got)
	}

	// One row moves.
	job.Items[1].downloaded.Store(4096)
	out = m.patchLocked(m.snapshotLocked())
	if !out.Jobs[0].Patch || len(out.Jobs[0].Items) != 1 {
		t.Fatalf("want one changed row in a patch, got %d (patch=%v)",
			len(out.Jobs[0].Items), out.Jobs[0].Patch)
	}
	if got := out.Jobs[0].Items[0].ID; got != "b" {
		t.Errorf("the patch carried %q, want the row that moved", got)
	}

	// Everything outside the item list rides in every frame, patched or
	// not: it is small, and it is what a collapsed card is drawn from.
	if out.Jobs[0].Title == "" || out.Jobs[0].Total != 3 {
		t.Errorf("a patched frame lost the job's own state: %+v", out.Jobs[0])
	}
}

// A list that lost rows cannot be patched into what a browser holds: a
// merge only ever adds or replaces, so it has no way to say a row is gone.
func TestPatchSendsTheWholeListWhenRowsAreLost(t *testing.T) {
	m, job := patchManager()
	m.patchLocked(m.snapshotLocked())

	job.Items = job.Items[:2]
	out := m.patchLocked(m.snapshotLocked())
	if out.Jobs[0].Patch {
		t.Error("a shortened list was sent as a patch")
	}
	if got := len(out.Jobs[0].Items); got != 2 {
		t.Errorf("carried %d items, want the whole shortened list", got)
	}
}

// A list that merely grew is a patch carrying the new rows, which the
// client appends. This is the common case while a large album resolves —
// items arriving a page at a time — and it is exactly when sending the
// whole list again is most expensive.
func TestPatchCarriesOnlyTheNewRowsWhenAListGrows(t *testing.T) {
	m, job := patchManager()
	m.patchLocked(m.snapshotLocked())

	job.Items = append(job.Items, &Item{ID: "d", Name: "d.mp4", Status: StatusQueued})
	out := m.patchLocked(m.snapshotLocked())
	if !out.Jobs[0].Patch {
		t.Error("a lengthened list was sent whole")
	}
	if got := len(out.Jobs[0].Items); got != 1 {
		t.Fatalf("carried %d items, want only the new one", got)
	}
	if got := out.Jobs[0].Items[0].ID; got != "d" {
		t.Errorf("carried %q, want the row that arrived", got)
	}
}

// Contents replaced wholesale is the case that makes a count comparison
// insufficient on its own: a job re-read after a restart drops its items and
// resolves them again, and both halves can land between two frames. Every
// row being new is what gives that away.
func TestPatchSendsTheWholeListWhenEveryRowIsNew(t *testing.T) {
	m, job := patchManager()
	m.patchLocked(m.snapshotLocked())

	job.Items = []*Item{
		{ID: "x", Name: "x.mp4", Status: StatusQueued},
		{ID: "y", Name: "y.mp4", Status: StatusQueued},
		{ID: "z", Name: "z.mp4", Status: StatusQueued},
	}
	out := m.patchLocked(m.snapshotLocked())
	if out.Jobs[0].Patch {
		t.Error("a list replaced wholesale was sent as a patch, leaving the old rows behind")
	}
	if got := len(out.Jobs[0].Items); got != 3 {
		t.Errorf("carried %d items, want the whole replaced list", got)
	}
}

// A frame dropped on the way to a slow reader takes its changes with it, so
// that subscriber is owed a whole snapshot rather than the next patch.
func TestADroppedFrameLeavesTheSubscriberOwedAWholeSnapshot(t *testing.T) {
	ch := make(chan []byte, 1)
	m := &Manager{}

	if dropped := m.deliverLocked(ch, []byte("first")); dropped {
		t.Error("the first frame into an empty buffer was reported dropped")
	}
	if dropped := m.deliverLocked(ch, []byte("second")); !dropped {
		t.Error("replacing an unread frame was not reported")
	}
	if got := string(<-ch); got != "second" {
		t.Errorf("the reader got %q, want the newest frame", got)
	}
}
