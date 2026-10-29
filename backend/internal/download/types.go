// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

// Status is the lifecycle state of a job or an item.
type Status string

const (
	// StatusResolving means the source page is still being scraped.
	StatusResolving Status = "resolving"
	// StatusQueued means the item is waiting for a free worker slot.
	StatusQueued Status = "queued"
	// StatusRunning means bytes are moving.
	StatusRunning Status = "running"
	// StatusDone means the file is complete and renamed into place.
	StatusDone Status = "done"
	// StatusFailed means the transfer gave up after its retries.
	StatusFailed Status = "failed"
	// StatusCanceled means a user cancelled it.
	StatusCanceled Status = "canceled"
)

// Terminal reports whether no further work will happen in this state.
func (s Status) Terminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCanceled
}

// Item is one file within a job.
//
// Every field except downloaded is guarded by Manager.mu; downloaded is
// atomic so the progress ticker can sample a running transfer without
// contending with the workers.
type Item struct {
	ID      string
	JobID   string
	Name    string
	Dir     string
	URL     string
	Headers httpx.Header
	Size    int64
	// SizeApprox marks Size as a rounded figure from a listing page, which
	// must not be compared against a file already on disk.
	SizeApprox bool
	// Skipped records that the destination already held this file, so no
	// bytes were moved. Unlike Note it survives completion, because it
	// describes the outcome rather than what is happening right now.
	Skipped bool
	// Segments, when set, are the ordered parts this file is joined from.
	Segments []string
	// SegmentKey is the AES-128 key the segments are encrypted under, or
	// nil for a playlist in the clear. See playlist.go.
	SegmentKey *extractor.SegmentKey
	// Audio is the soundtrack of a playlist whose Segments carry video
	// alone: each track is fetched into a part file of its own, and all of
	// them are joined to the video when the last lands (mux.go).
	Audio []extractor.Track
	// Subtitles are the playlist's WebVTT tracks, fetched at the start of
	// each attempt into subtitleFiles and joined with the audio
	// (subtitles.go).
	Subtitles     []extractor.Track
	subtitleFiles []subtitleFile
	// External, when set, is a page handed to the helper script instead of
	// being fetched over HTTP.
	External string
	// SegmentsDone and SegmentsTotal track a playlist transfer, which has
	// no known byte total to measure against until it finishes.
	SegmentsDone  int
	SegmentsTotal int
	Status        Status
	Err           string
	Note          string
	Path          string

	downloaded atomic.Int64
	// streams is how many connections are currently fetching this item.
	streams atomic.Int32
	// extraBytes counts the part of downloaded that came through extra
	// streams on routes of their own, and routeExtras how many such streams
	// are running (multiroute.go). Neither belongs to the item's own route.
	extraBytes  atomic.Int64
	routeExtras atomic.Int32
	resolve     func(context.Context) (*extractor.Target, error)
	refresh     func(context.Context) (*extractor.Target, error)
	// check is extractor.File.Check, asked before a route is spent.
	check         func(context.Context) error
	proxyFallback bool
	// discard is the last resolved target's Discard: set for a link bound
	// to the address that obtained it.
	discard func()
	// cipher, when set, means this host serves the file encrypted and the
	// bytes are decrypted on their way into the part file. It belongs to
	// the item rather than to a resolved target: the URL is re-minted per
	// attempt, the key never changes.
	cipher *extractor.StreamCipher
	// pace, when set, holds this item back from a host that punishes
	// parallelism rather than refusing it. See extractor.Pace.
	pace *extractor.Pace
	// routePace is a DirectFirst pace the item has not moved onto yet: it
	// runs unpaced on the normal connection until the host makes that
	// connection wait, and then this becomes its pace (moveOntoRoutes).
	routePace *extractor.Pace
	// A proxy lease spans one worker's ownership. The preferred route stays
	// after release so resuming can reuse that address's download ticket.
	route          *proxy.Lease
	preferredRoute string
	proxyRetries   int
	// routeAttempts includes proxy faults exempt from the retry budget, so
	// another CAPTCHA on another connection is visible as a new attempt.
	routeAttempts  int
	routeCancel    context.CancelFunc
	routeUpgrade   *proxy.Lease
	routeResumable bool
	// reject, when set, recognises this host's way of answering a dead
	// resource with a valid-looking body. See extractor.File.Reject.
	reject func(string, http.Header) error
	// refused, when set, is the host's own reading of a refused transfer.
	// See extractor.File.Refused.
	refused func(context.Context, int, http.Header) error
	// fixedName protects a filename allocated to separate same-named entries
	// in one listing. A later resolver or disposition header must not merge
	// those entries back into the same destination.
	fixedName bool
	// listed is the name the item's listing gave it, before a resolver, a
	// Content-Disposition header or an external downloader renamed it. A
	// listing read again names the item so again, so the job's record of
	// what it saved keeps a finished file under both names
	// (finishedLocked). Kept under the new name alone, every file a resolver
	// had renamed was fetched again after a restart.
	listed string
	// lastView is what was last broadcast for this item, so the next frame
	// can carry only the rows that actually changed. Guarded by
	// Manager.mu, and written only on the broadcast path — a snapshot read
	// by the terminal or by /api/state settles nothing about what a
	// browser has been sent.
	lastView ItemView
	// waitingFor is the host this item is queued behind, when it is. It is
	// kept rather than baked into the note because what that host is taking
	// changes while the item waits, and a note written once would go on
	// stating whatever was true when it was written.
	waitingFor string
	// notBefore holds a queued item back until a time its host named. See
	// extractor.WaitError. The note gives the time left from it, for the
	// same reason waitingFor is kept apart from the note.
	notBefore time.Time
	// hostKey is the remote this item is charged against while it runs. It
	// is fixed at dispatch rather than read from URL later, because a
	// resolver may point the item at a different host mid-flight and the
	// release must match the reservation.
	hostKey string
	// limiter is its job's speed limit, shared by every file of the job and
	// fixed when the item is made, so readers need no lock to find it.
	limiter *throttle
	// overloadWaits counts the turns this item has been sent away from an
	// overloaded host. It lives on the item rather than in the transfer
	// because the waiting happens in the queue: the item loses its slot
	// each time, so a counter local to one transfer would start over on
	// every turn and the patience would never run out.
	//
	// Only refusals in a row count: a turn after which the host got some
	// other transfer going starts the count over (refusedAt, against the
	// host's lastServed). An overloaded host that is still working through
	// its queue will reach this file; one that has stopped serving
	// altogether is what the patience is for.
	overloadWaits int
	refusedAt     time.Time
	cancel        context.CancelFunc

	// inFlight is true from the moment the dispatcher hands this item to a
	// worker until that worker has fully finished with it. Status alone
	// cannot serve this purpose: cancelling flips Status to canceled while
	// the worker is still winding down, and re-dispatching then would give
	// two goroutines the same item and the same .part file.
	inFlight bool
	// retryPending records a retry requested while inFlight; the worker
	// re-queues the item on its way out.
	retryPending bool
	// stallDefers counts the times a stall has sent this item to the back
	// of the queue. Bounded by the retry budget, so a host that never
	// resumes serving still fails the item rather than cycling it forever;
	// reset whenever the item is queued afresh on a user's say-so.
	stallDefers int
	speed       float64
	lastBytes   int64
	lastSample  time.Time
	// lastExtraBytes is extraBytes when lastBytes was taken.
	lastExtraBytes int64
	startedAt      time.Time
	finishedAt     time.Time
}

// Job is one submitted URL and everything behind it.
type Job struct {
	ID        string
	Source    string
	Title     string
	Host      string
	Password  string
	Err       string
	CreatedAt time.Time
	Items     []*Item
	// Limit is the job's own limits beneath the queue's (joblimits.go),
	// replaced whole whenever it changes.
	Limit JobLimit

	// limiter is the bucket the job's speed limit is kept in. It lives as
	// long as the job and is changed in place, so a transfer already reading
	// through it is held to a new limit at once.
	limiter *throttle
	// resolving is true between submission and the extractor returning.
	resolving   bool
	resolveNote string // transient source progress, guarded by Manager.mu
	resolveID   uint64 // identifies the only extractor result allowed to publish
	// canceled records an explicit user cancellation, which outranks the
	// status derived from the items.
	canceled bool
	cancel   context.CancelFunc
	// lastCount is how many items this job had in the last frame broadcast.
	// A list that has changed length cannot be patched into what a browser
	// holds — items may have gone as well as arrived — so it is sent whole
	// instead. Guarded by Manager.mu.
	lastCount int
	// restored marks a job read back from the state file with work left in
	// it. Its items describe what was found last time, which is enough to
	// show but not to fetch: the links they were reached by have expired, or
	// were never written down at all because a resolver mints them per
	// attempt. So the source is read again before anything is transferred,
	// and the part files on disk supply the bytes already fetched.
	restored bool
	// finished records what the job had already saved when its items were
	// last dropped for a re-read, keyed by destination (destinationKey). A
	// file whose length is unknown until it arrives (no size in the listing,
	// no Content-Length) cannot be recognised on disk before it is fetched,
	// so without this every restart fetched it again and kept it as "(2)".
	// Set by rereadLocked, used and cleared by applyResultLocked.
	finished map[string]finishedFile
	// unfetchable marks a job read back from the state file at all, whether
	// or not it was held: its items carry no URL, for the reason above, so
	// retrying any of them means reading the source again. restored alone
	// cannot say so — a job whose files were all done or cancelled comes
	// back unheld, and re-queueing a cancelled one on its own failed with
	// "no download URL".
	unfetchable bool
}

// finishedFile is where a job saved one file, and at what length.
type finishedFile struct {
	path    string
	size    int64
	skipped bool
}

// ItemView is the JSON shape of an item sent to the browser.
type ItemView struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Dir        string  `json:"dir,omitempty"`
	Status     Status  `json:"status"`
	Size       int64   `json:"size"`
	Downloaded int64   `json:"downloaded"`
	Speed      float64 `json:"speed"`
	Error      string  `json:"error,omitempty"`
	Note       string  `json:"note,omitempty"`
	Path       string  `json:"path,omitempty"`
	// URL is deliberately absent from the wire. It was the single largest
	// thing in a snapshot — a signed storage link runs to a couple of
	// hundred characters, and there is one per item — and no part of the
	// UI has ever read it. See the note on Snapshot about what the payload
	// is allowed to carry.
	Streams int `json:"streams,omitempty"`
	// Skipped is true when the file was already in the destination.
	Skipped bool `json:"skipped,omitempty"`
	// Segment counts are the only honest progress for a playlist transfer.
	SegmentsDone  int `json:"segmentsDone,omitempty"`
	SegmentsTotal int `json:"segmentsTotal,omitempty"`
	// Elapsed is for the terminal display, which reads a Snapshot in
	// process. It is kept off the wire for the same reason as URL: the
	// browser has never read it.
	Elapsed float64 `json:"-"`
}

// JobView is the JSON shape of a job, with its aggregates precomputed so the
// browser renders straight from the payload.
type JobView struct {
	ID         string     `json:"id"`
	Source     string     `json:"source"`
	Title      string     `json:"title"`
	Host       string     `json:"host"`
	Status     Status     `json:"status"`
	Error      string     `json:"error,omitempty"`
	Note       string     `json:"note,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	Items      []ItemView `json:"items"`
	Total      int        `json:"total"`
	Done       int        `json:"done"`
	Failed     int        `json:"failed"`
	Canceled   int        `json:"canceled"`
	Active     int        `json:"active"`
	Size       int64      `json:"size"`
	Downloaded int64      `json:"downloaded"`
	Speed      float64    `json:"speed"`
	// Patch marks Items as only the rows that changed since this
	// subscriber's last frame, to be merged into what it already has
	// rather than replacing it. Absent means the list is complete. See
	// Manager.patchLocked.
	Patch bool `json:"patch,omitempty"`
	// SizeKnown is false while any item's length is still unknown, so the
	// UI can avoid showing a misleading total.
	SizeKnown bool `json:"sizeKnown"`
	// Held marks a job restored from the last run and waiting to be told to
	// go. Its items read "queued", which alone says something is about to
	// start; without this the UI had nothing to offer that would start it.
	Held bool `json:"held,omitempty"`
	// The job's own limits, as flat fields (fileLimit, streamLimit,
	// speedLimit): an object would be a new one in every frame, and the
	// browser's cards compare a job's fields to skip rendering it again.
	JobLimit
}

// Snapshot is the whole application state, broadcast over SSE.
type Snapshot struct {
	Jobs        []JobView `json:"jobs"`
	Concurrency int       `json:"concurrency"`
	MaxConcur   int       `json:"maxConcurrency"`
	Streams     int       `json:"streams"`
	MaxStreams  int       `json:"maxStreams"`
	Proxies     bool      `json:"proxies"`
	// ProxyType is the protocol every proxy group draws its routes from, and
	// ProxyTypes the groups' own, where they have one.
	ProxyType  string            `json:"proxyType"`
	ProxyTypes map[string]string `json:"proxyTypes"`
	Active     int               `json:"active"`
	Queued     int               `json:"queued"`
	// Paused holds the whole queue; SpeedLimit caps total throughput in
	// bytes per second, where zero means unlimited.
	Paused      bool    `json:"paused"`
	SpeedLimit  int64   `json:"speedLimit"`
	Speed       float64 `json:"speed"`
	DownloadDir string  `json:"downloadDir"`
	// DiskFree is what may still be written at DownloadDir, and DiskTotal
	// the size of the filesystem holding it. Both are zero when the
	// destination could not be measured, which is why free is not reported
	// on its own: a genuinely full disk reads as zero too, and only the
	// total tells the two apart.
	DiskFree  int64 `json:"diskFree"`
	DiskTotal int64 `json:"diskTotal"`
	// DiskMinFree is the room that must be left before another transfer is
	// started. The UI needs it to say why a queue is sitting still, which is
	// otherwise indistinguishable from one that is merely slow.
	DiskMinFree int64 `json:"diskMinFree"`
	// HostCount is how many hosts have an extractor of their own. The names
	// were once sent too, for a list in the UI that only ever restated what
	// the build supported; the count is all anything still reads.
	HostCount int `json:"hostCount"`
	// Held counts the jobs waiting on a Resume, and their items are left
	// out of Queued: a queue that is not going to move by itself should not
	// read as one about to.
	Held int `json:"held"`
	// Version is the server's build. The browser shows it, and a version
	// that changes under an open tab is a server replaced by a deploy while
	// the page goes on running the interface it was loaded with.
	Version string `json:"version,omitempty"`
}

// storedNote is the note an item carries, for callers with nothing better.
// The manager has something better — see Manager.itemNoteLocked.
func storedNote(it *Item) string { return it.Note }

// view renders an item for the wire.
//
// The note is supplied rather than read off the item because one kind of
// note goes stale where it stands: an item waiting for a host records what
// that host was taking at the moment it was turned away, and the answer
// changes while it waits. Rows waiting on the same host for the same reason
// then disagree with each other, which is exactly how it looked.
func (it *Item) view(note func(*Item) string) ItemView {
	v := ItemView{
		ID:            it.ID,
		Name:          it.Name,
		Dir:           it.Dir,
		Status:        it.Status,
		Size:          it.Size,
		Downloaded:    it.downloaded.Load(),
		Speed:         it.speed,
		Error:         it.Err,
		Note:          note(it),
		Path:          it.Path,
		Streams:       int(it.streams.Load()),
		Skipped:       it.Skipped,
		SegmentsDone:  it.SegmentsDone,
		SegmentsTotal: it.SegmentsTotal,
	}
	if !it.startedAt.IsZero() {
		end := it.finishedAt
		if end.IsZero() {
			end = time.Now()
		}
		v.Elapsed = end.Sub(it.startedAt).Seconds()
	}
	if it.Status != StatusRunning {
		v.Speed = 0
		v.Streams = 0
	}
	return v
}

// view renders a job and its aggregates for the wire.
func (j *Job) view(note func(*Item) string) JobView {
	v := JobView{
		ID:        j.ID,
		Source:    j.Source,
		Title:     j.Title,
		Host:      j.Host,
		Error:     j.Err,
		CreatedAt: j.CreatedAt,
		Items:     make([]ItemView, 0, len(j.Items)),
		Total:     len(j.Items),
		SizeKnown: true,
		Held:      j.restored,
		JobLimit:  j.Limit,
	}
	if j.resolving && !j.canceled {
		v.Note = j.resolveNote
	}
	var t itemTally
	for _, it := range j.Items {
		iv := it.view(note)
		v.Items = append(v.Items, iv)

		v.Downloaded += iv.Downloaded
		v.Speed += iv.Speed
		if iv.Size > 0 {
			v.Size += iv.Size
		} else if iv.Status != StatusDone {
			v.SizeKnown = false
		}
		t.count(iv.Status)
	}
	v.Done, v.Failed, v.Canceled, v.Active = t.done, t.failed, t.canceled, t.running
	v.Status = j.statusFrom(t)
	return v
}

// itemTally is how many of a job's items are in each state that decides
// anything. The view and the status both need it, and the view is built on
// every progress tick for every item, so it is counted once and shared
// rather than walked twice.
type itemTally struct {
	done, failed, canceled, running int
}

func (t *itemTally) count(s Status) {
	switch s {
	case StatusDone:
		t.done++
	case StatusFailed:
		t.failed++
	case StatusCanceled:
		t.canceled++
	case StatusRunning:
		t.running++
	}
}

// tally counts the job's items.
func (j *Job) tally() itemTally {
	var t itemTally
	for _, it := range j.Items {
		t.count(it.Status)
	}
	return t
}

// status derives the job state from its phase and its items.
func (j *Job) status() Status { return j.statusFrom(j.tally()) }

// complete requires every file to be done and the source to have no error.
// A done status alone can also include canceled files or a partial listing.
func (j *Job) complete() bool {
	t := j.tally()
	return j.statusFrom(t) == StatusDone && j.Err == "" && t.done == len(j.Items)
}

// statusFrom derives the job state from its phase and an already counted
// tally of its items.
func (j *Job) statusFrom(t itemTally) Status {
	switch {
	case j.canceled:
		return StatusCanceled
	case j.resolving:
		return StatusResolving
	case j.Err != "" && len(j.Items) == 0:
		return StatusFailed
	case len(j.Items) == 0:
		return StatusQueued
	case t.running > 0:
		return StatusRunning
	case t.done+t.failed+t.canceled < len(j.Items):
		return StatusQueued
	case t.failed > 0:
		return StatusFailed
	case t.done > 0:
		return StatusDone
	default:
		return StatusCanceled
	}
}
