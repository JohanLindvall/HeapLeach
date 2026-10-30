// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/captcha"
	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Keep2Share and FileBoom are one service on two hosts: the same web app,
// the same public download protocol, each on an API base of its own. One
// extractor serves both, and a file link's page names the service, which
// is what picks the API its file belongs to. File information is available
// without a CAPTCHA; the free download requires an image answer followed by
// a server-specified wait. Both happen at transfer time so a queued file
// does not spend its download ticket before it can use it.
type Keep2Share struct {
	client *httpx.Client
	// services are the hosts the app is served from, told apart by the name
	// their pages give.
	services []*keep2ShareService
	// The production solver uses local OCR. Keeping the image reader apart
	// lets protocol tests exercise refusals and waits without loading the OCR model.
	// It returns at least one reading, most likely first, or an error.
	solver func(context.Context, []byte) ([]string, error)
}

// keep2ShareService is one host of the app: its name, its API and the waits
// it has asked for.
type keep2ShareService struct {
	ex    *Keep2Share
	label string
	api   string
	// files is the newer API the site's own pages ask whether a file can be
	// had free (premiumOnly). The older getFileStatus went on calling files
	// free that the site offers to premium users alone. Empty asks nothing.
	files string

	// cooldown is when the host's wait between free downloads runs out. It
	// holds back every file from this address, not only the one that was
	// told, so the rest learn it here rather than each solving a CAPTCHA to
	// be told again. Each host keeps its own: one's wait says nothing of
	// the other's.
	mu        sync.Mutex
	cooldowns map[string]time.Time
}

func (s *keep2ShareService) cooldownNote() string {
	return s.label + ": waiting between free downloads"
}

// errorf prefixes an error with the service that answered, in lower case.
func (s *keep2ShareService) errorf(format string, args ...any) error {
	return fmt.Errorf(strings.ToLower(s.label)+": "+format, args...)
}

func keep2ShareRoute(ctx context.Context) string {
	if id := httpx.RouteID(ctx); id != "" {
		return id
	}
	return httpx.DirectRoute
}

// wait records the host's wait between free downloads and returns the error
// that puts a file back in the queue until it is over.
func (s *keep2ShareService) wait(ctx context.Context, delay time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cooldowns == nil {
		s.cooldowns = make(map[string]time.Time)
	}
	route := keep2ShareRoute(ctx)
	if until := time.Now().Add(delay); until.After(s.cooldowns[route]) {
		s.cooldowns[route] = until
	}
	return &WaitError{Until: s.cooldowns[route], Reason: s.cooldownNote()}
}

// waiting returns that error while the wait is still on, and nil after.
func (s *keep2ShareService) waiting(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if until := s.cooldowns[keep2ShareRoute(ctx)]; time.Now().Before(until) {
		return &WaitError{Until: until, Reason: s.cooldownNote()}
	}
	return nil
}

func NewKeep2Share(client *httpx.Client) *Keep2Share {
	k := &Keep2Share{client: client.WithTimeout(config.Keep2ShareRequestTimeout)}
	k.services = []*keep2ShareService{
		{ex: k, label: "Keep2Share", api: "https://k2s.cc/api/v2", files: "https://api.k2s.cc/v1/files"},
		{ex: k, label: "FileBoom", api: "https://fboom.me/api/v2", files: "https://api.fboom.me/v1/files"},
	}
	return k
}

func (k *Keep2Share) Name() string { return keep2SharePace.Group }

// keep2ShareShape is a file link, /file/<id>, with or without the filename
// the site appends.
var keep2ShareShape = pathShape(`^/file/[A-Za-z0-9_-]+(/[^/]*)?/?$`)

// The web app shell both services serve for every file link: the entry
// bundle and the client chunk it preloads, mounted on #root.
const (
	keep2ShareEntryScript = "/static/js/entry-index-"
	keep2ShareClientChunk = "/static/js/chunk-client-"
)

// Recognises is the inventory's description.
func (k *Keep2Share) Recognises() string {
	return "file links (`/file/<id>`) whose page is the Keep2Share or FileBoom web app shell " +
		"(its `entry-index` bundle and `chunk-client` preload on `#root`), named Keep2Share or FileBoom; " +
		"the name chooses the service's own API"
}

// Shape reports a file link.
func (k *Keep2Share) Shape(u *url.URL) bool { return keep2ShareShape(u) }

// Recognise takes a file link served by the app shell. The file is
// described by the API and nothing of it is in the page, deleted or not, so
// the shell is what is checked — its entry bundle and client chunk — and
// then the name, which is the one thing that tells the two services apart.
func (k *Keep2Share) Recognise(_ context.Context, p *Page) bool {
	return keep2ShareShape(p.Final) && p.Script(keep2ShareEntryScript) &&
		p.Has(keep2ShareClientChunk, `id="root"`) && k.service(p) != nil
}

// service is the host a page names itself as, or nil.
func (k *Keep2Share) service(p *Page) *keep2ShareService {
	for _, s := range k.services {
		if p.Named(s.label) {
			return s
		}
	}
	return nil
}

// One free download at a time per address, whichever host serves it.
var keep2SharePace = Pace{Streams: 1, Files: 1, Group: config.GroupKeep2Share, PerRoute: true}

// Extract asks the API of the host the link's page names. The registry has
// already probed the page; a caller that has not is answered by a probe of
// its own.
func (k *Keep2Share) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	p := probed(ctx, u)
	if p == nil {
		p = probePage(ctx, k.client, u)
	}
	s := k.service(p)
	if s == nil {
		return nil, errors.New("keep2share: the page names neither Keep2Share nor FileBoom")
	}
	return s.extract(ctx, u)
}

func (s *keep2ShareService) extract(ctx context.Context, u *url.URL) (*Result, error) {
	parts := util.PathSegments(u)
	if len(parts) < 2 || parts[0] != "file" {
		return nil, s.errorf("expected a file link (/file/<id>)")
	}
	id := parts[1]
	info, err := s.call(ctx, "getFileStatus", map[string]string{"id": id})
	if err != nil {
		return nil, err
	}
	if info.IsFolder {
		return nil, s.errorf("folder links are not supported")
	}
	if !info.Available {
		return nil, s.errorf("file is unavailable or has been removed")
	}
	if info.ForFree != nil && !*info.ForFree {
		return nil, s.errorf("this file requires a Premium account")
	}
	if info.Name == "" {
		return nil, s.errorf("file information contains no name")
	}
	size := int64(-1)
	if info.Size != nil && *info.Size >= 0 {
		size = *info.Size
	}
	download := &keep2ShareDownload{host: s, id: id, name: info.Name, size: size}
	return &Result{Title: info.Name, Files: []File{{
		Name: info.Name, Size: size, Pace: &keep2SharePace, Resolve: download.resolve, Check: download.check,
	}}}, nil
}

type keep2ShareResponse struct {
	host      string
	Status    string `json:"status"`
	Code      int    `json:"code"`
	ErrorCode int    `json:"errorCode"`
	Message   string `json:"message"`
	Errors    []struct {
		Code          int       `json:"code"`
		Message       string    `json:"message"`
		TimeRemaining flexValue `json:"timeRemaining"`
	} `json:"errors"`

	Name      string `json:"name"`
	Size      *int64 `json:"size"`
	Available bool   `json:"is_available"`
	IsFolder  bool   `json:"is_folder"`
	ForFree   *bool  `json:"isAvailableForFree"`

	Challenge string `json:"challenge"`
	ImageURL  string `json:"captcha_url"`
	URL       string `json:"url"`
	FreeKey   string `json:"free_download_key"`
	Wait      int64  `json:"time_wait"`
}

func (r *keep2ShareResponse) Error() string {
	message := cmp.Or(r.Message, fmt.Sprintf("API error %d", r.ErrorCode))
	switch {
	case r.ErrorCode == 42 && len(r.Errors) == 0:
		message += ": free downloads are refused from this address"
	case r.ErrorCode == 33:
		message += ": this address is asked for a reCAPTCHA, which cannot be read here"
	case r.ErrorCode == 43:
		message += ": " + keep2SharePremiumOnly
	}
	for _, detail := range r.Errors {
		if detail.Message != "" {
			message += ": " + detail.Message
		} else {
			switch detail.Code {
			case 3, 7:
				message += ": this file requires a Premium account"
			case 5:
				message += ": this address must wait longer than the waiting limit"
			case 6:
				message += ": only one free download may run at a time"
			case 8:
				message += ": this file is private"
			case 9:
				message += ": this file requires a subscription"
			}
		}
	}
	return r.host + ": " + message
}

// refusesAddress reports a refusal of the address asking rather than of the
// file. Asked for the same file through twenty-two public proxies in one
// sample, six were answered errorCode 42 with no reason at all while sixteen
// got tickets, so that answer says nothing about the file. Neither do a wait
// longer than the limit (detail 5, once retryDelay has declined it) or
// another free download running from the same address (detail 6).
// limitsAddress reports a free-download limit on the address that asked
// rather than on the file: its traffic (detail 2, "Traffic limit exceed")
// or its count of downloads (detail 1). Asked for one such file through
// twelve public proxies, seven answered errorCode 21 with "Traffic limit
// exceed" while five, and the direct address, got tickets for it. The
// answer says nothing of when the limit lifts.
func (r *keep2ShareResponse) limitsAddress() bool {
	if r.ErrorCode != 21 && r.ErrorCode != 42 {
		return false
	}
	for _, detail := range r.Errors {
		if detail.Code == 1 || detail.Code == 2 {
			return true
		}
	}
	return false
}

func (r *keep2ShareResponse) refusesAddress() bool {
	// A reCAPTCHA in place of the image is asked of addresses the host
	// mistrusts, and another may be given the image.
	if r.ErrorCode == 33 {
		return true
	}
	if r.ErrorCode != 42 {
		return false
	}
	if len(r.Errors) == 0 {
		return true
	}
	for _, detail := range r.Errors {
		if detail.Code == 5 || detail.Code == 6 {
			return true
		}
	}
	return false
}

func (r *keep2ShareResponse) retryDelay() time.Duration {
	if r.ErrorCode == 41 && r.Wait > 0 && r.Wait <= int64(config.Keep2ShareMaxWait/time.Second) {
		return time.Duration(r.Wait) * time.Second
	}
	for _, detail := range r.Errors {
		if detail.Code != 5 {
			continue
		}
		seconds, err := strconv.ParseFloat(detail.TimeRemaining.String(), 64)
		if err == nil && !math.IsNaN(seconds) && seconds > 0 && seconds <= config.Keep2ShareMaxWait.Seconds() {
			return time.Duration(math.Ceil(seconds)) * time.Second
		}
	}
	return 0
}

// The API carries useful errors (including remaining wait time) in HTTP
// 406 responses. Decode those as well as successes, without losing network
// errors or treating an unrelated HTML error page as an API answer.
func (s *keep2ShareService) call(ctx context.Context, method string, in map[string]string) (*keep2ShareResponse, error) {
	out := keep2ShareResponse{host: strings.ToLower(s.label)}
	err := s.ex.client.PostJSON(ctx, s.api+"/"+method, nil, in, &out)
	if err != nil {
		var status *httpx.StatusError
		if !errors.As(err, &status) || json.Unmarshal([]byte(status.Body), &out) != nil || out.Status != "error" {
			return nil, s.errorf("%s: %w", method, err)
		}
	}
	if out.Status != "success" {
		return &out, &out
	}
	return &out, nil
}

type keep2ShareDownload struct {
	host     *keep2ShareService
	id, name string
	size     int64
	// routes holds the file's free download on each route it is fetched
	// through. A ticket and a link belong to the address that obtained them,
	// and one file may be fetched through several routes at once, as by an
	// extra stream on an idle route. Each has its own lock, so one route's
	// ticket timer holds up no other.
	mu     sync.Mutex
	routes map[string]*keep2ShareFree
	// premium is whether the site offers the file to premium users alone,
	// as it said at checkedAt (checkFree). Guarded by mu.
	premium   bool
	checkedAt time.Time
}

// check is File.Check: it refuses a file kept for premium users before the
// downloader spends a route on it. Through routes, each such file first met
// the dead ones a public feed is full of, five seconds apiece.
func (d *keep2ShareDownload) check(ctx context.Context) error { return d.checkFree(ctx, false) }

// keep2SharePremiumOnly is what the site's own download dialog says of a file
// it offers to premium users alone: "Free download: unavailable".
const keep2SharePremiumOnly = "this file is only for premium users; the site offers no free download of it"

// premiumOnly asks the site whether it offers the file to premium users
// alone. known is false when no answer could be had, and the free flow then
// goes on as it did before the question was asked.
func (s *keep2ShareService) premiumOnly(ctx context.Context, id string) (premium, known bool) {
	if s.files == "" {
		return false, false
	}
	var out struct {
		Free *bool `json:"isAvailableForFree"`
	}
	// The answer is the file's, whoever asks, so it goes over the caller's
	// own connection rather than through a proxy that may be failing: through
	// one, it timed out and the CAPTCHA was spent before the refusal came.
	if err := s.ex.client.GetJSON(httpx.WithoutRoute(ctx), s.files+"/"+url.PathEscape(id)+"?referer=", nil, &out); err != nil || out.Free == nil {
		extractionLogger(ctx).Debug(s.label+": could not ask whether the file is free", "err", err)
		return false, false
	}
	return !*out.Free, true
}

// checkFree refuses a file the site offers to premium users alone before a
// CAPTCHA is spent on it, as the site's own download dialog does. The older
// API called such files free, and the bare refusal every address then got
// was taken for a refusal of each address in turn: a CAPTCHA, and an hour
// set aside, for every route a file was tried on, when nothing could fetch
// it. The answer is kept for Keep2ShareFreeCheck unless again is set.
func (d *keep2ShareDownload) checkFree(ctx context.Context, again bool) error {
	d.mu.Lock()
	premium, fresh := d.premium, !again && !d.checkedAt.IsZero() && time.Since(d.checkedAt) < config.Keep2ShareFreeCheck
	d.mu.Unlock()
	if !fresh {
		var known bool
		if premium, known = d.host.premiumOnly(ctx, d.id); !known {
			return nil
		}
		d.mu.Lock()
		d.premium, d.checkedAt = premium, time.Now()
		d.mu.Unlock()
	}
	if premium {
		extractionLogger(ctx).Info(d.host.label+": the file is only for premium users", "file", d.name)
		return d.host.errorf("%s", keep2SharePremiumOnly)
	}
	return nil
}

// keep2ShareFree is one route's free download of a file.
type keep2ShareFree struct {
	mu      sync.Mutex
	key     string
	ready   time.Time
	target  *Target
	expires time.Time
}

// free is the file's free download on route.
func (d *keep2ShareDownload) free(route string) *keep2ShareFree {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.routes == nil {
		d.routes = make(map[string]*keep2ShareFree)
	}
	st := d.routes[route]
	if st == nil {
		st = &keep2ShareFree{}
		d.routes[route] = st
	}
	return st
}

// prune forgets a route that left nothing behind: a file can try hundreds
// of routes, most of which end without a ticket or a link. Caller holds
// st.mu, which is always taken before d.mu.
func (d *keep2ShareDownload) prune(route string, st *keep2ShareFree) {
	if st.key != "" || st.target != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.routes[route] == st {
		delete(d.routes, route)
	}
}

func (d *keep2ShareDownload) resolve(ctx context.Context) (*Target, error) {
	// CAPTCHA tickets and storage links belong to the public IP that
	// obtained them, so each route has its own free flow.
	route := keep2ShareRoute(ctx)
	st := d.free(route)
	st.mu.Lock()
	defer st.mu.Unlock()
	defer d.prune(route, st)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A transfer retry may resume the same free link. Solving another
	// CAPTCHA would spend a second download and can trigger the hourly
	// limit. The storage URL states its own expiry, so no lifetime is guessed.
	if st.target != nil && time.Now().Add(config.Keep2ShareExpiryMargin).Before(st.expires) {
		copy := *st.target
		return &copy, nil
	}

	log := extractionLogger(ctx).With("file", d.name)
	if st.key == "" {
		if err := d.checkFree(ctx, false); err != nil {
			return nil, err
		}
	}
	var last error
	var waited time.Duration
	// The current image's readings not yet submitted. A wrong answer leaves
	// the challenge open, so the next reading goes to the same image.
	var challenge string
	var readings []string
	for attempts, waits := 0, 0; ; {
		if delay := time.Until(st.ready); delay > 0 {
			if delay > config.Keep2ShareMaxWait-waited {
				return nil, d.host.errorf("free-download wait exceeds the waiting limit; retry later")
			}
			// A ticket's timer is usually half a minute and the transfer
			// follows it, so a short one is sat out here. Tickets have also
			// come with seventy minutes on them, which is the wait between
			// free downloads arriving by another road: that goes back to
			// the queue, as it does without a ticket, and the ticket is
			// redeemed when the item comes round again on this route.
			if delay > config.Keep2ShareInPlaceWait {
				return nil, d.host.wait(ctx, delay)
			}
			ResolveNote(ctx, fmt.Sprintf("%s: waiting %s for the free-download timer", d.host.label, delay.Round(time.Second)))
			if err := util.SleepCtx(ctx, delay); err != nil {
				return nil, err
			}
			waited += delay
		}
		in := map[string]string{"file_id": d.id}
		if st.key != "" {
			in["free_download_key"] = st.key
		} else {
			if len(readings) == 0 {
				if err := d.host.waiting(ctx); err != nil {
					return nil, err
				}
				if attempts >= config.Keep2ShareCaptchaAttempts {
					// Unread or misread images say nothing about the file,
					// and fresh ones may read; another attempt is worth it.
					return nil, &TransientError{Err: d.host.errorf("could not obtain a free download after %d CAPTCHA attempts: %w", attempts, last)}
				}
				attempts++
				var err error
				challenge, readings, err = d.nextChallenge(ctx, attempts)
				if err != nil {
					if !errors.Is(err, errKeep2ShareOCR) {
						return nil, err
					}
					last = err
					continue
				}
			}
			in["captcha_challenge"], in["captcha_response"] = challenge, readings[0]
			readings = readings[1:]
		}
		if st.key != "" {
			ResolveNote(ctx, d.host.label+": requesting download link")
		} else {
			ResolveNote(ctx, fmt.Sprintf("%s: submitting CAPTCHA answer (%d/%d)", d.host.label, attempts, config.Keep2ShareCaptchaAttempts))
		}
		out, err := d.host.call(ctx, "getUrl", in)
		if out == nil || out.ErrorCode != 31 {
			// Anything but a wrong answer spends or outlives the challenge.
			readings = nil
		} else if st.key == "" {
			log.Debug(d.host.label+": CAPTCHA reading was wrong", "attempt", attempts, "readings_left", len(readings))
		}
		if err != nil && out != nil && out.ErrorCode != 31 {
			log.Debug(d.host.label+": host refused the request", "ticket", st.key != "", "code", out.ErrorCode, "err", err)
		}
		if err != nil {
			if out == nil {
				return nil, err
			}
			last = err
			if delay := out.retryDelay(); delay > 0 {
				// With a ticket this is its own timer, which the top of
				// the loop sits out or sends back to the queue by length.
				// Without one, this is the host's wait between free
				// downloads: the best part of an hour, which the file
				// spends back in the queue rather than holding a worker.
				if st.key == "" {
					return nil, d.host.wait(ctx, delay)
				}
				waits++
				if waits > config.Keep2ShareWaits {
					return nil, err
				}
				st.ready = time.Now().Add(delay)
				continue
			}
			switch out.ErrorCode {
			case 30, 31, 40:
				st.key = ""
				continue
			}
			// The address has used up its free traffic or downloads. That
			// is a wait for this route, as long as it is unknown, and the
			// file goes on through another, or waits here without one.
			if out.limitsAddress() {
				return nil, d.host.wait(ctx, config.Keep2ShareAddressLimitWait)
			}
			if out.refusesAddress() {
				// The bare refusal is also all that a file offered to
				// premium users alone gets, from every address: ask again.
				if out.ErrorCode == 42 && len(out.Errors) == 0 {
					if err := d.checkFree(ctx, true); err != nil {
						return nil, err
					}
				}
				return nil, &RefusedError{Err: err}
			}
			return nil, err
		}
		if out.URL != "" {
			u, err := url.Parse(out.URL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return nil, d.host.errorf("invalid download URL")
			}
			st.expires = time.Time{}
			if expiry, err := strconv.ParseInt(u.Query().Get("temp_url_expires"), 10, 64); err == nil {
				st.expires = time.Unix(expiry, 0)
			}
			st.target = &Target{URL: out.URL, Name: d.name, Size: d.size, Discard: st.discard(out.URL)}
			log.Info(d.host.label+": download link granted", "expires", st.expires.Format(time.DateTime))
			copy := *st.target
			return &copy, nil
		}
		if out.FreeKey == "" || out.Wait < 0 || out.Wait > int64(config.Keep2ShareMaxWait/time.Second) {
			return nil, d.host.errorf("no download URL or valid free-download ticket returned")
		}
		st.key = out.FreeKey
		st.ready = time.Now().Add(time.Duration(out.Wait) * time.Second)
		log.Info(d.host.label+": free download ticket granted", "wait", (time.Duration(out.Wait) * time.Second).String())
		waits++
		if waits > config.Keep2ShareWaits {
			return nil, d.host.errorf("free-download wait did not finish")
		}
	}
}

// discard drops a link the storage host refused because another address was
// using it, and the ticket it came from: both belong to the address that
// obtained them, so the next resolve on this route starts afresh. A link
// handed out since is kept.
func (st *keep2ShareFree) discard(link string) func() {
	return func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.target != nil && st.target.URL == link {
			st.key, st.target = "", nil
			st.ready, st.expires = time.Time{}, time.Time{}
		}
	}
}

// nextChallenge requests a CAPTCHA image and reads it, returning the challenge
// and up to Keep2ShareCaptchaGuesses readings, most likely first.
func (d *keep2ShareDownload) nextChallenge(ctx context.Context, attempt int) (string, []string, error) {
	note := func(stage string) {
		ResolveNote(ctx, fmt.Sprintf("%s: %s (%d/%d)", d.host.label, stage, attempt, config.Keep2ShareCaptchaAttempts))
	}
	solver := d.host.ex.solver
	if solver == nil {
		var err error
		solver, err = keep2ShareOCR()
		if err != nil {
			return "", nil, err
		}
	}
	note("requesting CAPTCHA")
	captcha, err := d.host.call(ctx, "requestCaptcha", map[string]string{})
	if err != nil {
		return "", nil, err
	}
	if captcha.Challenge == "" || captcha.ImageURL == "" {
		return "", nil, d.host.errorf("CAPTCHA response contains no image or challenge")
	}
	extractionLogger(ctx).Debug(d.host.label+": CAPTCHA served", "file", d.name, "attempt", attempt)
	note("downloading CAPTCHA image")
	img, err := d.host.captchaImage(ctx, captcha.ImageURL)
	if err != nil {
		return "", nil, err
	}
	// The local reader reports waiting for a slot separately from its
	// bounded run. A supplied solver has no local admission queue.
	if d.host.ex.solver != nil {
		note("reading CAPTCHA")
	}
	readings, err := solver(WithResolveNote(ctx, note), img)
	if err == nil && len(readings) == 0 {
		err = errKeep2ShareOCR
	}
	if err != nil {
		return "", nil, err
	}
	return captcha.Challenge, readings[:min(len(readings), config.Keep2ShareCaptchaGuesses)], nil
}

func (s *keep2ShareService) captchaImage(ctx context.Context, raw string) ([]byte, error) {
	// The API still advertises its image over HTTP. Request HTTPS on the
	// public host, as for every other step of the download.
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, s.errorf("invalid CAPTCHA URL")
	}
	if api, err := url.Parse(s.api); err == nil && u.Scheme == "http" && api.Scheme == "https" &&
		util.HostMatches(u.Host, api.Hostname()) {
		u.Scheme = "https"
	}
	req, err := s.ex.client.NewRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	img, err := s.ex.client.Bytes(req)
	if err != nil {
		return nil, s.errorf("read CAPTCHA: %w", err)
	}
	if len(img) > config.Keep2ShareCaptchaBytes {
		return nil, s.errorf("CAPTCHA image is too large")
	}
	return img, nil
}

var errKeep2ShareOCR = captcha.ErrUnreadable

// Check the runtime and model before requesting a challenge. The shared reader
// owns concurrency, session reuse, inference cancellation and ranked decoding.
func keep2ShareOCR() (func(context.Context, []byte) ([]string, error), error) {
	if err := captcha.Available(); err != nil {
		return nil, err
	}
	return func(ctx context.Context, raw []byte) ([]string, error) {
		return captcha.Read(ctx, raw, func(note string) { ResolveNote(ctx, note) })
	}, nil
}
