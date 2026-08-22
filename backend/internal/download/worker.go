package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// transfer downloads one item to disk, resuming and retrying as needed.
func (m *Manager) transfer(ctx context.Context, it *Item) error {
	if err := m.resolveTarget(ctx, it); err != nil {
		return err
	}

	m.mu.Lock()
	name := SafeName(it.Name)
	rel := it.Dir
	expected := it.Size
	approx := it.SizeApprox
	m.mu.Unlock()

	dir := filepath.Join(m.cfg.DownloadDir, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	// A previous run may already have finished this exact file. A size a
	// listing page rounded for display cannot settle that question, so
	// those wait for the length the server itself reports.
	if expected > 0 && !approx {
		if m.alreadyOnDisk(it, dir, rel, name, expected) {
			return nil
		}
	}

	// The part file is scoped by a hash of the source URL, so an
	// interrupted transfer is picked up again by the next run. If that
	// exact path is already being written — the same URL queued twice —
	// this transfer falls back to a name only it can hold.
	// Keyed on the job's source — the URL the user submitted — because the
	// item's own URL is often a signed link that expires between runs, and
	// a key that changes every run cannot resume anything.
	m.mu.Lock()
	source := it.URL
	if job, found := m.jobs[it.JobID]; found && job.Source != "" {
		source = job.Source
	}
	m.mu.Unlock()

	part := filepath.Join(dir, name+"."+partSuffix(source)+".part")
	release, ok := m.claimPart(part)
	if !ok {
		part = filepath.Join(dir, name+"."+it.ID+".part")
		if release, ok = m.claimPart(part); !ok {
			return fmt.Errorf("part file %s is already in use", part)
		}
	}
	defer release()

	m.mu.Lock()
	isPlaylist := len(it.Segments) > 0
	external := it.External
	encrypted := it.cipher != nil
	m.mu.Unlock()

	// Decryption happens in the byte writer, which neither of the paths
	// below goes through. No host serves an encrypted playlist or hands one
	// to an external tool, so this is an assertion rather than a
	// limitation — but writing ciphertext to disk unremarked would look
	// like a corrupt file rather than like a bug.
	if encrypted && (isPlaylist || external != "") {
		return errors.New("this host serves the file encrypted in a form the downloader cannot decrypt")
	}

	// An external downloader writes the finished file itself, so none of
	// the part-file, resume or rename machinery below applies.
	if external != "" {
		return m.transferExternal(ctx, it, dir, rel)
	}

	var busyWaits int
	for attempt := 0; ; attempt++ {
		var (
			final string
			err   error
		)
		if isPlaylist {
			final, err = m.transferPlaylist(ctx, it, part, name)
		} else {
			final, err = m.transferOnce(ctx, it, part, name)
		}
		if err == nil {
			name = final
			break
		}
		// The file was already in the destination at the right length.
		// alreadyOnDisk has recorded it; there is nothing left to rename,
		// and whatever partial state an earlier run left is dead weight.
		if errors.Is(err, errAlreadyComplete) {
			_ = os.Remove(part)
			clearTransferState(part)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// A busy host has not failed; it has asked us to come back. Those
		// attempts do not spend the retry budget, which exists for
		// transfers that are actually going wrong — but patience only pays
		// where trying again can change the answer. An item with a
		// resolver re-signs its link or rotates to another mirror each
		// time; one without asks the same URL the same way, and if that
		// keeps answering with a web page it is almost always because the
		// URL *is* a web page — a pasted link no extractor recognised — so
		// those attempts are capped rather than retried forever.
		var busy *busyHostError
		if errors.As(err, &busy) {
			m.mu.Lock()
			rotates := it.resolve != nil
			m.mu.Unlock()
			if !rotates && busyWaits >= config.BusyRetryLimit {
				return pageNotAFile(busy)
			}
			busyWaits++
			wait := util.Backoff(busyWaits-1, m.timings.busyBase, m.timings.busyMax)
			m.note(it, fmt.Sprintf("host busy — retrying in %s (attempt %d)",
				wait.Round(time.Second), busyWaits))
			m.log.Info("host busy, waiting", "item", it.ID, "name", name,
				"attempt", busyWaits, "wait", wait)
			if err := util.SleepCtx(ctx, wait); err != nil {
				return err
			}
			m.note(it, "")
			// Gofile hands out several storage servers per file; resolving
			// again rotates to the next one, which may not be busy.
			if err := m.resolveTarget(ctx, it); err != nil {
				return err
			}
			attempt-- // this was not a failure
			continue
		}

		if attempt >= m.cfg.MaxRetries || !retryableTransfer(err) {
			return err
		}
		m.log.Info("retrying download", "item", it.ID, "name", name, "attempt", attempt+1, "err", err)
		if err := util.SleepCtx(ctx, transferRetryDelay(attempt)); err != nil {
			return err
		}
		// Signed links expire; mint a fresh one before trying again.
		if err := m.resolveTarget(ctx, it); err != nil {
			return err
		}
	}

	dest, err := UniquePath(dir, name)
	if err != nil {
		return err
	}
	if err := os.Rename(part, dest); err != nil {
		return fmt.Errorf("finalise %s: %w", name, err)
	}
	// Cleared here rather than inside each transfer path, so the resume
	// sidecar is also removed when an attempt ends early — a 416 for a part
	// file that is already whole, or a restored table with nothing left.
	clearTransferState(part)
	// Any transport stream is worth rewrapping, however it arrived.
	dest = m.remuxToMP4(ctx, dest)
	m.setPath(it, filepath.Join(rel, filepath.Base(dest)))
	return nil
}

// transferOnce runs one attempt: a single request that discovers the file,
// then either a multi-connection transfer when the length is known and the
// server honours ranges, or a plain sequential stream when it is not.
//
// It returns the filename to save under, which the server may refine via
// Content-Disposition.
func (m *Manager) transferOnce(ctx context.Context, it *Item, part, name string) (string, error) {
	m.mu.Lock()
	rawURL := it.URL
	rel := it.Dir
	headers := maps.Clone(it.Headers)
	payload := it.cipher
	maxStreams := m.streams
	// A host that asks to be approached gently only ever lowers the
	// ceiling; it can never raise it above what the user configured.
	if it.pace != nil && it.pace.Streams > 0 {
		maxStreams = min(maxStreams, it.pace.Streams)
	}
	m.mu.Unlock()

	if rawURL == "" {
		return "", errors.New("no download URL")
	}

	// Where to resume from. For a segmented part file the sidecar is
	// authoritative; a part file without one is a plain sequential
	// remnant, whose length is exactly what it holds.
	state := loadTransferState(part)
	var (
		offset     int64
		rangeEnd   int64 = -1
		validator  string
		primaryIdx int
	)
	if state != nil {
		primaryIdx = -1
		for i, seg := range state.Segments {
			if seg.Pos < seg.End {
				primaryIdx, offset, rangeEnd = i, seg.Pos, seg.End
				break
			}
		}
		if primaryIdx < 0 {
			return name, nil // every segment already finished
		}
		validator = state.Validator
	} else if fi, err := os.Stat(part); err == nil {
		offset = fi.Size()
	}

	// A watchdog aborts this attempt alone if every connection goes silent;
	// the caller's context still means the user cancelled.
	attemptCtx, abort := context.WithCancel(ctx)
	defer abort()
	var stalled atomic.Bool

	req, err := m.client.NewRequest(attemptCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	for header, value := range headers {
		req.Header.Set(header, value)
	}
	req.Header.Set(httpx.HeaderAccept, httpx.AcceptAny)
	// Ask for the raw bytes: a transparently compressed body would make the
	// progress total disagree with what lands on disk.
	req.Header.Set(httpx.HeaderAcceptEncoding, httpx.EncodingIdentity)
	// Always ask for a range, even from byte zero. The reply is how we
	// learn whether this server supports ranges at all: 206 means the
	// transfer can be split later, 200 means it cannot.
	if rangeEnd > 0 {
		req.Header.Set(httpx.HeaderRange,
			"bytes="+strconv.FormatInt(offset, 10)+"-"+strconv.FormatInt(rangeEnd-1, 10))
	} else {
		req.Header.Set(httpx.HeaderRange, "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	if validator != "" {
		req.Header.Set(httpx.HeaderIfRange, validator)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// The server ignored the range, or If-Range did not match. Either
		// way what is on disk is unusable: start over.
		offset, state = 0, nil
	case http.StatusPartialContent:
		// Resuming as asked.
	case http.StatusRequestedRangeNotSatisfiable:
		// We already hold every byte the server has.
		if offset > 0 {
			return name, nil
		}
		return "", statusError(req.URL, resp)
	default:
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", statusError(req.URL, resp)
		}
	}

	if err := rejectWebPage(resp, name); err != nil {
		return "", err
	}
	// The host's own dead end, which no general rule catches — see
	// extractor.File.Reject. Checked against the URL redirects landed on,
	// since that is usually what gives it away.
	if err := m.rejectByHost(it, resp); err != nil {
		return "", err
	}

	total := totalSize(resp, offset)
	if state != nil && total <= 0 {
		total = state.Size
	}
	if disp := resp.Header.Get(httpx.HeaderContentDisposition); disp != "" {
		if fromServer := filenameFromDisposition(disp); fromServer != "" {
			name = chooseName(name, SafeName(fromServer))
		}
	}
	if v := util.FirstNonEmpty(resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")); v != "" {
		validator = v
	}

	if total > 0 {
		m.mu.Lock()
		it.Size = total
		it.SizeApprox = false
		m.mu.Unlock()

		// The length is authoritative now, and the server may just have
		// corrected the name too. If that file is already sitting in the
		// destination, this transfer has nothing to do — the only check
		// that works for hosts whose listings carry no size at all, or
		// only a rounded one. Checked before the part file is opened, so
		// skipping leaves nothing behind.
		if offset == 0 && m.alreadyOnDisk(it, filepath.Dir(part), rel, name, total) {
			return name, errAlreadyComplete
		}
	}

	// O_APPEND is deliberately absent: segments write at explicit offsets.
	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 && state == nil {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filepath.Base(part), err)
	}
	defer f.Close()

	// A host that serves ciphertext decrypts on the way in, so the part
	// file holds plaintext and everything downstream — resume, the segment
	// sidecar, the finished length — keeps meaning what it says.
	dst, err := writerFor(f, payload)
	if err != nil {
		return "", err
	}

	go watchForStall(attemptCtx, it.downloaded.Load, abort, &stalled, m.stallTimeout(), m.throttle.isPaused)

	// Without a known length there is nothing to divide up.
	if total <= 0 {
		if err := m.streamSequential(attemptCtx, it, dst, resp.Body, offset); err != nil {
			return "", annotateTransfer(name, err, &stalled, m.stallTimeout())
		}
		return name, closeFile(f, name)
	}

	table := newSegmentTable(total, offset)
	if state != nil && state.Size == total {
		table = restoreSegmentTable(total, state.Segments)
	} else {
		primaryIdx = 0
	}
	segs := table.snapshot()
	if primaryIdx >= len(segs) {
		primaryIdx = 0
	}

	m.setProgress(it, table.written())

	transfer := &segmentedTransfer{
		manager:    m,
		item:       it,
		file:       dst,
		table:      table,
		part:       part,
		name:       name,
		validator:  validator,
		maxStreams: streamsFor(maxStreams, total, resp),
		slowBelow:  m.cfg.SlowSpeed,

		pollInterval:  m.timings.poll,
		probeInterval: m.timings.probe,
		addCooldown:   m.timings.cooldown,
		saveInterval:  m.timings.save,
	}
	transfer.withDefaults()
	transfer.saveState()

	if err := transfer.run(attemptCtx, segs[primaryIdx], resp.Body); err != nil {
		return "", annotateTransfer(name, err, &stalled, m.stallTimeout())
	}
	if err := closeFile(f, name); err != nil {
		return "", err
	}
	return name, nil
}

// streamSequential copies a whole response body onto the end of the file,
// for servers that never told us how long it is.
func (m *Manager) streamSequential(ctx context.Context, it *Item, dst io.WriterAt, body io.Reader, offset int64) error {
	m.setProgress(it, offset)

	writer := &offsetWriter{dst: dst, at: offset}
	buf, release := borrowChunk()
	defer release()
	_, err := io.CopyBuffer(&progressWriter{item: it, w: writer}, m.throttled(ctx, body), buf)
	return err
}

// setProgress resets the byte counter and the rate-sampling baseline.
func (m *Manager) setProgress(it *Item, done int64) {
	m.mu.Lock()
	it.downloaded.Store(done)
	it.lastBytes = done
	it.lastSample = time.Now()
	m.mu.Unlock()
}

// streamsFor caps the connection count for one file. Anything the server
// will not let us range over, or that is too small to be worth dividing,
// gets a single connection.
func streamsFor(configured int, total int64, resp *http.Response) int {
	if configured < 1 {
		return 1
	}
	if total < config.MinSplitFileSize {
		return 1
	}
	if resp.StatusCode != http.StatusPartialContent {
		return 1 // the server ignored our range, so it will ignore the rest
	}
	if strings.EqualFold(resp.Header.Get("Accept-Ranges"), "none") {
		return 1
	}
	return configured
}

// offsetWriter appends at a tracked offset, so the sequential path can share
// a file handle opened for positional writes.
type offsetWriter struct {
	dst io.WriterAt
	at  int64
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	n, err := w.dst.WriteAt(p, w.at)
	w.at += int64(n)
	return n, err
}

// annotateTransfer turns a failed transfer into a reportable error, naming a
// stall as such rather than as the cancellation it manifests as.
func annotateTransfer(name string, err error, stalled *atomic.Bool, after time.Duration) error {
	if stalled.Load() {
		// Deliberately not wrapping: the underlying error is the context
		// cancellation the watchdog raised, and reporting that as the
		// cause would make a stall look like the user cancelling.
		return fmt.Errorf("transfer %s stalled: no data for %s", name, after)
	}
	return fmt.Errorf("transfer %s: %w", name, err)
}

// stallTimeout is how long a transfer may sit still before its attempt is
// abandoned. Configured, because "not moving" means different things on a
// throttled host and on a dead connection.
func (m *Manager) stallTimeout() time.Duration {
	if m.cfg != nil && m.cfg.StallTimeout > 0 {
		return m.cfg.StallTimeout
	}
	return config.StallTimeout
}

// closeFile surfaces a deferred-write failure, which only shows up on close.
func closeFile(f *os.File, name string) error {
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// watchForStall aborts the attempt when the sampled byte counter stops
// moving for StallTimeout. It exits as soon as the attempt context is done,
// so a completed transfer leaves nothing running.
//
// progress is whichever counter proves bytes are arriving: the item's own
// for an HTTP transfer, and a fetched-bytes counter for a playlist, whose
// item counter only advances when a whole part lands in order.
func watchForStall(ctx context.Context, progress func() int64, abort context.CancelFunc, stalled *atomic.Bool,
	timeout time.Duration, paused func() bool) {
	interval := timeout / 3
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	last := progress()
	var idle time.Duration

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A paused transfer is idle on purpose; holding that against
			// it would abort exactly the transfers the user asked to keep.
			if paused != nil && paused() {
				idle = 0
				continue
			}
			current := progress()
			if current != last {
				last, idle = current, 0
				continue
			}
			if idle += interval; idle >= timeout {
				stalled.Store(true)
				abort()
				return
			}
		}
	}
}

// resolveTarget runs an item's lazy resolver, if it has one, and folds the
// result back into the item.
func (m *Manager) resolveTarget(ctx context.Context, it *Item) error {
	m.mu.Lock()
	resolve := it.resolve
	m.mu.Unlock()
	if resolve == nil {
		return nil
	}

	target, err := resolve(ctx)
	if err != nil {
		return err
	}
	if target == nil || target.URL == "" {
		return errors.New("resolver returned no download URL")
	}

	m.mu.Lock()
	it.URL = target.URL
	if len(target.Headers) > 0 {
		it.Headers = target.Headers
	}
	if target.Name != "" {
		it.Name = target.Name
	}
	if target.Size > 0 {
		it.Size = target.Size
	}
	m.mu.Unlock()
	return nil
}

// note records a short, transient explanation of what an item is waiting
// for, so a download that is deliberately pausing does not look stalled.
func (m *Manager) note(it *Item, text string) {
	m.mu.Lock()
	it.Note = text
	m.mu.Unlock()
	m.markDirty()
}

// setPath records where the finished file landed, relative to the root.
func (m *Manager) setPath(it *Item, rel string) {
	m.mu.Lock()
	it.Path = filepath.ToSlash(rel)
	m.mu.Unlock()
}

// progressWriter counts bytes on their way to disk.
type progressWriter struct {
	item *Item
	w    io.Writer
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 {
		p.item.downloaded.Add(int64(n))
	}
	return n, err
}

// totalSize works out the full length of the resource from the response.
func totalSize(resp *http.Response, offset int64) int64 {
	if cr := resp.Header.Get(httpx.HeaderContentRange); cr != "" {
		if total, ok := parseContentRangeTotal(cr); ok {
			return total
		}
	}
	if resp.ContentLength >= 0 {
		return offset + resp.ContentLength
	}
	return -1
}

// parseContentRangeTotal reads the total from "bytes 0-1023/4096".
func parseContentRangeTotal(v string) (int64, bool) {
	_, rest, ok := strings.Cut(strings.TrimSpace(v), "/")
	if !ok {
		return 0, false
	}
	rest = strings.TrimSpace(rest)
	if rest == "*" {
		return 0, false
	}
	total, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || total < 0 {
		return 0, false
	}
	return total, true
}

// filenameFromDisposition extracts the filename, preferring the RFC 5987
// encoded form that Go's mime parser folds into "filename" for us.
func filenameFromDisposition(v string) string {
	_, params, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	name := params["filename"]
	if name == "" {
		return ""
	}
	// Some servers percent-encode without saying so.
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	return strings.TrimSpace(name)
}

// chooseName keeps the extractor's name unless the server's is strictly
// more informative — that is, it carries an extension and ours does not.
func chooseName(current, fromServer string) string {
	switch {
	case fromServer == "":
		return current
	case current == "" || current == "download":
		return fromServer
	case filepath.Ext(current) == "" && filepath.Ext(fromServer) != "":
		return fromServer
	default:
		return current
	}
}

// busyHostError means a request for file bytes was answered with a web page.
//
// For a host that hands out several storage servers, or signs a link per
// visit, that is transient and asking again lands somewhere else. For a URL
// with nothing to re-resolve it usually means the opposite — the URL is a
// web page, and no extractor recognised the site — so the two are worded
// apart once the retries have been spent.
type busyHostError struct{ url string }

func (e *busyHostError) Error() string {
	return fmt.Sprintf("%s: the host returned a web page instead of the file "+
		"(its storage server is busy, or the link has expired)", e.url)
}

// pageNotAFile explains the same answer for a URL that cannot be resolved
// again, where repeating the request can only produce the same page.
func pageNotAFile(err *busyHostError) error {
	return fmt.Errorf("%s: this URL serves a web page, not a file — no extractor here "+
		"recognises the site, so there is nothing to read the media location out of",
		err.url)
}

// rejectWebPage catches a host answering a file request with a web page.
//
// Gofile does exactly this when the storage server holding a file is busy:
// the download link 302s back to its own web page, and because redirects are
// followed that arrives as a few kilobytes of HTML with a 200. Written to
// disk unchallenged it would replace a multi-gigabyte video with the page
// shell — and, since Content-Length matches what was written, be recorded as
// a complete download.
func rejectWebPage(resp *http.Response, name string) error {
	// A ranged answer is the real thing; only a plain 200 is suspect.
	if resp.StatusCode == http.StatusPartialContent {
		return nil
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get(httpx.HeaderContentType))
	if mediaType != "text/html" && mediaType != "application/xhtml+xml" {
		return nil
	}
	// Unless a web page is genuinely what was asked for.
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html", ".htm", ".xhtml":
		return nil
	}
	return &busyHostError{url: resp.Request.URL.Redacted()}
}

// errDeadResource marks a rejection by the extractor's own Reject: the host
// answered with something that is not the file and never will be.
var errDeadResource = errors.New("the host says this file is gone")

// rejectByHost applies the extractor's own response guard, if it set one.
func (m *Manager) rejectByHost(it *Item, resp *http.Response) error {
	m.mu.Lock()
	reject := it.reject
	m.mu.Unlock()
	if reject == nil {
		return nil
	}

	final := ""
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	if err := reject(final, resp.Header); err != nil {
		return fmt.Errorf("%w: %w", errDeadResource, err)
	}
	return nil
}

// retryableTransfer reports whether a failed pass is worth repeating. A
// definitive client error (404, 403, ...) is not.
func retryableTransfer(err error) bool {
	if httpx.IsCanceled(err) {
		return false
	}
	// The host said the resource is gone. Asking again gets the same answer.
	if errors.Is(err, errDeadResource) {
		return false
	}
	var busy *busyHostError
	if errors.As(err, &busy) {
		return true
	}
	var se *httpx.StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusRequestTimeout, http.StatusTooManyRequests:
			return true
		}
		return se.Code < 400 || se.Code >= 500
	}
	// I/O and connection failures mid-body: worth resuming.
	return true
}

// transferRetryDelay backs off between whole-transfer retries, which are
// costlier than a single request and so start from a longer base.
func transferRetryDelay(attempt int) time.Duration {
	return util.Backoff(attempt, config.TransferRetryBase, config.TransferRetryMax)
}

// statusError builds an error carrying a short slice of the response body,
// which is usually where these hosts put the real reason.
func statusError(u *url.URL, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return &httpx.StatusError{
		Code:   resp.StatusCode,
		Status: resp.Status,
		URL:    u.Redacted(),
		Body:   string(body),
	}
}

// errAlreadyComplete reports that the destination already holds this exact
// file, so the transfer stopped before moving any bytes.
var errAlreadyComplete = errors.New("already downloaded")

// alreadyOnDisk reports whether name is already in the destination at
// exactly this length, and if so marks the item finished against it.
//
// Length is the whole test. Comparing contents would mean reading the file
// the transfer is trying to avoid reading, and every host worth skipping
// for serves the same bytes under the same name at the same length.
func (m *Manager) alreadyOnDisk(it *Item, dir, rel, name string, size int64) bool {
	if size <= 0 {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil || fi.IsDir() || fi.Size() != size {
		return false
	}
	it.downloaded.Store(size)
	m.mu.Lock()
	it.Size = size
	it.SizeApprox = false
	it.Skipped = true
	m.mu.Unlock()
	m.setPath(it, filepath.Join(rel, name))
	return true
}
