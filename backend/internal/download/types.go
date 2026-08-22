package download

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
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
	resolve func(context.Context) (*extractor.Target, error)
	// cipher, when set, means this host serves the file encrypted and the
	// bytes are decrypted on their way into the part file. It belongs to
	// the item rather than to a resolved target: the URL is re-minted per
	// attempt, the key never changes.
	cipher *extractor.StreamCipher
	cancel context.CancelFunc

	// inFlight is true from the moment the dispatcher hands this item to a
	// worker until that worker has fully finished with it. Status alone
	// cannot serve this purpose: cancelling flips Status to canceled while
	// the worker is still winding down, and re-dispatching then would give
	// two goroutines the same item and the same .part file.
	inFlight bool
	// retryPending records a retry requested while inFlight; the worker
	// re-queues the item on its way out.
	retryPending bool
	speed        float64
	lastBytes    int64
	lastSample   time.Time
	startedAt    time.Time
	finishedAt   time.Time
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

	// resolving is true between submission and the extractor returning.
	resolving bool
	// canceled records an explicit user cancellation, which outranks the
	// status derived from the items.
	canceled bool
	cancel   context.CancelFunc
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
	URL        string  `json:"url,omitempty"`
	Streams    int     `json:"streams,omitempty"`
	// Skipped is true when the file was already in the destination.
	Skipped bool `json:"skipped,omitempty"`
	// Segment counts are the only honest progress for a playlist transfer.
	SegmentsDone  int     `json:"segmentsDone,omitempty"`
	SegmentsTotal int     `json:"segmentsTotal,omitempty"`
	Elapsed       float64 `json:"elapsed"`
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
	// SizeKnown is false while any item's length is still unknown, so the
	// UI can avoid showing a misleading total.
	SizeKnown bool `json:"sizeKnown"`
}

// Snapshot is the whole application state, broadcast over SSE.
type Snapshot struct {
	Jobs        []JobView `json:"jobs"`
	Concurrency int       `json:"concurrency"`
	MaxConcur   int       `json:"maxConcurrency"`
	Streams     int       `json:"streams"`
	MaxStreams  int       `json:"maxStreams"`
	Active      int       `json:"active"`
	Queued      int       `json:"queued"`
	// Paused holds the whole queue; SpeedLimit caps total throughput in
	// bytes per second, where zero means unlimited.
	Paused      bool    `json:"paused"`
	SpeedLimit  int64   `json:"speedLimit"`
	Speed       float64 `json:"speed"`
	DownloadDir string  `json:"downloadDir"`
	// HostCount is how many hosts have an extractor of their own. The names
	// were once sent too, for a list in the UI that only ever restated what
	// the build supported; the count is all anything still reads.
	HostCount int `json:"hostCount"`
}

// view renders an item for the wire.
func (it *Item) view() ItemView {
	v := ItemView{
		ID:            it.ID,
		Name:          it.Name,
		Dir:           it.Dir,
		Status:        it.Status,
		Size:          it.Size,
		Downloaded:    it.downloaded.Load(),
		Speed:         it.speed,
		Error:         it.Err,
		Note:          it.Note,
		Path:          it.Path,
		URL:           it.URL,
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
func (j *Job) view() JobView {
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
	}
	for _, it := range j.Items {
		iv := it.view()
		v.Items = append(v.Items, iv)

		v.Downloaded += iv.Downloaded
		v.Speed += iv.Speed
		if iv.Size > 0 {
			v.Size += iv.Size
		} else if iv.Status != StatusDone {
			v.SizeKnown = false
		}
		switch iv.Status {
		case StatusDone:
			v.Done++
		case StatusFailed:
			v.Failed++
		case StatusCanceled:
			v.Canceled++
		case StatusRunning:
			v.Active++
		}
	}
	v.Status = j.status()
	return v
}

// status derives the job state from its phase and its items.
func (j *Job) status() Status {
	switch {
	case j.resolving:
		return StatusResolving
	case j.canceled:
		return StatusCanceled
	case j.Err != "" && len(j.Items) == 0:
		return StatusFailed
	case len(j.Items) == 0:
		return StatusQueued
	}

	var done, failed, canceled, running int
	for _, it := range j.Items {
		switch it.Status {
		case StatusDone:
			done++
		case StatusFailed:
			failed++
		case StatusCanceled:
			canceled++
		case StatusRunning:
			running++
		}
	}
	switch {
	case running > 0:
		return StatusRunning
	case done+failed+canceled < len(j.Items):
		return StatusQueued
	case failed > 0:
		return StatusFailed
	case done > 0:
		return StatusDone
	default:
		return StatusCanceled
	}
}
