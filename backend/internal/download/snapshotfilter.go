package download

import (
	"slices"
	"strings"
)

// What a browser is actually looking at.
//
// A snapshot carries every item of every job, and the browser renders a
// fraction of them: a collapsed job card shows its aggregates, which the
// server has already worked out, and an open one shows a windowed slice of
// its rows. On a queue of a couple of thousand files that was most of a
// megabyte, two and a half times a second, to paint a few dozen rows.
//
// So a subscriber says which jobs it has open, and every other job's items
// arrive reduced to what the views that span the whole queue still need:
// the file search matches on a name, and the progress panel counts finished
// files by id and adds up their bytes. Everything else — where the file
// landed, what it is waiting for, how many connections it has — belongs to
// a row that is not on screen.
//
// The snapshot stays whole in the sense that matters: it is complete for
// what the client is showing, so a client that misses a frame still heals
// on the next one.

// smallJob is the size below which a job is sent whole whatever the
// subscriber says.
//
// Slimming a handful of items saves nothing worth the arithmetic, and it
// spares the browser from having to declare the jobs it opens by default:
// a single-file job is expanded on sight, and it is exactly the case this
// threshold covers. What the saving is actually made of is the albums of
// hundreds.
const smallJob = 8

// openSet is the set of job ids a subscriber renders items for.
type openSet map[string]bool

// newOpenSet builds the set, ignoring blanks.
func newOpenSet(ids []string) openSet {
	if len(ids) == 0 {
		return nil
	}
	out := make(openSet, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	return out
}

// key identifies the set, so subscribers looking at the same thing share one
// encoding of it.
func (o openSet) key() string {
	if len(o) == 0 {
		return ""
	}
	ids := make([]string, 0, len(o))
	for id := range o {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return strings.Join(ids, ",")
}

// trimSnapshot reduces the items of every job the subscriber does not have
// open. The snapshot itself is not touched: the CLI reads the same one in
// process, and a second browser may have other jobs open.
func trimSnapshot(snap Snapshot, open openSet) Snapshot {
	out := snap
	out.Jobs = make([]JobView, len(snap.Jobs))
	for i, job := range snap.Jobs {
		if open[job.ID] || len(job.Items) <= smallJob {
			out.Jobs[i] = job
			continue
		}
		job.Items = slimItems(job.Items)
		out.Jobs[i] = job
	}
	return out
}

// slimItems keeps only what the views spanning the whole queue read.
//
// Name is the file search. Id, status, size and downloaded are the progress
// panel, which counts each finished file once by id and adds up what it
// moved. A job's own totals are aggregates the server has already computed,
// so nothing here is needed to draw a collapsed card.
func slimItems(items []ItemView) []ItemView {
	if len(items) == 0 {
		return items
	}
	out := make([]ItemView, len(items))
	for i, it := range items {
		out[i] = ItemView{
			ID:         it.ID,
			Name:       it.Name,
			Status:     it.Status,
			Size:       it.Size,
			Downloaded: it.Downloaded,
		}
	}
	return out
}
