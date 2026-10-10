// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Keep2Share and FileBoom use the same public download protocol on separate
// API hosts. File information is available
// without a CAPTCHA; the free download requires an image answer followed by
// a server-specified wait. Both happen at transfer time so a queued file
// does not spend its download ticket before it can use it.
type Keep2Share struct {
	hostSet
	client *httpx.Client
	api    string
	label  string
	pace   Pace
	// The production solver uses local OCR. Keeping the image reader apart
	// lets protocol tests exercise refusals and waits without an OCR binary.
	// It returns at least one reading, most likely first, or an error.
	solver func(context.Context, []byte) ([]string, error)

	// cooldown is when the host's wait between free downloads runs out. It
	// holds back every file from this address, not only the one that was
	// told, so the rest learn it here rather than each solving a CAPTCHA to
	// be told again.
	mu        sync.Mutex
	cooldowns map[string]time.Time
}

func (k *Keep2Share) cooldownNote() string { return k.label + ": waiting between free downloads" }

func (k *Keep2Share) errorf(format string, args ...any) error {
	return fmt.Errorf(k.Name()+": "+format, args...)
}

func keep2ShareRoute(ctx context.Context) string {
	if id := httpx.RouteID(ctx); id != "" {
		return id
	}
	return httpx.DirectRoute
}

// wait records the host's wait between free downloads and returns the error
// that puts a file back in the queue until it is over.
func (k *Keep2Share) wait(ctx context.Context, delay time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cooldowns == nil {
		k.cooldowns = make(map[string]time.Time)
	}
	route := keep2ShareRoute(ctx)
	if until := time.Now().Add(delay); until.After(k.cooldowns[route]) {
		k.cooldowns[route] = until
	}
	return &WaitError{Until: k.cooldowns[route], Reason: k.cooldownNote()}
}

// waiting returns that error while the wait is still on, and nil after.
func (k *Keep2Share) waiting(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if until := k.cooldowns[keep2ShareRoute(ctx)]; time.Now().Before(until) {
		return &WaitError{Until: until, Reason: k.cooldownNote()}
	}
	return nil
}

func NewKeep2Share(client *httpx.Client) *Keep2Share {
	return &Keep2Share{
		hostSet: hostSet{"k2s.cc", "keep2share.cc"},
		client:  client,
		api:     "https://k2s.cc/api/v2",
		label:   "Keep2Share",
		pace:    keep2SharePace,
	}
}

func NewFileBoom(client *httpx.Client) *Keep2Share {
	return &Keep2Share{
		hostSet: hostSet{"fboom.me"},
		client:  client,
		api:     "https://fboom.me/api/v2",
		label:   "FileBoom",
		pace:    fileBoomPace,
	}
}

func (k *Keep2Share) Name() string { return k.pace.Group }

var keep2SharePace = Pace{Streams: 1, Files: 1, Group: "keep2share", PerRoute: true}
var fileBoomPace = Pace{Streams: 1, Files: 1, Group: "fileboom", PerRoute: true}

func (k *Keep2Share) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	parts := util.PathSegments(u)
	if len(parts) < 2 || parts[0] != "file" {
		return nil, k.errorf("expected a file link (/file/<id>)")
	}
	id := parts[1]
	info, err := k.call(ctx, "getFileStatus", map[string]string{"id": id})
	if err != nil {
		return nil, err
	}
	if info.IsFolder {
		return nil, k.errorf("folder links are not supported")
	}
	if !info.Available {
		return nil, k.errorf("file is unavailable or has been removed")
	}
	if info.ForFree != nil && !*info.ForFree {
		return nil, k.errorf("this file requires a Premium account")
	}
	if info.Name == "" {
		return nil, k.errorf("file information contains no name")
	}
	size := int64(-1)
	if info.Size != nil && *info.Size >= 0 {
		size = *info.Size
	}
	download := &keep2ShareDownload{host: k, id: id, name: info.Name, size: size}
	return &Result{Title: info.Name, Files: []File{{
		Name: info.Name, Size: size, Pace: &k.pace, Resolve: download.resolve,
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
	message := util.FirstNonEmpty(r.Message, fmt.Sprintf("API error %d", r.ErrorCode))
	if r.ErrorCode == 42 && len(r.Errors) == 0 {
		message += ": free downloads are refused from this address"
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
func (r *keep2ShareResponse) refusesAddress() bool {
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
func (k *Keep2Share) call(ctx context.Context, method string, in map[string]string) (*keep2ShareResponse, error) {
	out := keep2ShareResponse{host: k.Name()}
	err := k.client.PostJSON(ctx, k.api+"/"+method, nil, in, &out)
	if err != nil {
		var status *httpx.StatusError
		if !errors.As(err, &status) || json.Unmarshal([]byte(status.Body), &out) != nil || out.Status != "error" {
			return nil, k.errorf("%s: %w", method, err)
		}
	}
	if out.Status != "success" {
		return &out, &out
	}
	return &out, nil
}

type keep2ShareDownload struct {
	host     *Keep2Share
	id, name string
	size     int64
	mu       sync.Mutex
	key      string
	ready    time.Time
	target   *Target
	expires  time.Time
	route    string
}

func (d *keep2ShareDownload) resolve(ctx context.Context) (*Target, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if route := keep2ShareRoute(ctx); route != d.route {
		// CAPTCHA tickets and storage links belong to the public IP that
		// obtained them. A different route must start its own free flow.
		d.route, d.key, d.target = route, "", nil
		d.ready, d.expires = time.Time{}, time.Time{}
	}
	// A transfer retry may resume the same free link. Solving another
	// CAPTCHA would spend a second download and can trigger the hourly
	// limit. The storage URL states its own expiry, so no lifetime is guessed.
	if d.target != nil && time.Now().Add(config.Keep2ShareExpiryMargin).Before(d.expires) {
		copy := *d.target
		return &copy, nil
	}

	var last error
	var waited time.Duration
	// The current image's readings not yet submitted. A wrong answer leaves
	// the challenge open, so the next reading goes to the same image.
	var challenge string
	var readings []string
	for attempts, waits := 0, 0; ; {
		if delay := time.Until(d.ready); delay > 0 {
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
			resolveNote(ctx, fmt.Sprintf("%s: waiting %s for the free-download timer", d.host.label, delay.Round(time.Second)))
			if err := util.SleepCtx(ctx, delay); err != nil {
				return nil, err
			}
			waited += delay
		}
		in := map[string]string{"file_id": d.id}
		if d.key != "" {
			in["free_download_key"] = d.key
		} else {
			if len(readings) == 0 {
				if err := d.host.waiting(ctx); err != nil {
					return nil, err
				}
				if attempts >= config.Keep2ShareCaptchaAttempts {
					return nil, d.host.errorf("could not obtain a free download after %d CAPTCHA attempts: %w", attempts, last)
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
		out, err := d.host.call(ctx, "getUrl", in)
		if out == nil || out.ErrorCode != 31 {
			// Anything but a wrong answer spends or outlives the challenge.
			readings = nil
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
				if d.key == "" {
					return nil, d.host.wait(ctx, delay)
				}
				waits++
				if waits > config.Keep2ShareWaits {
					return nil, err
				}
				d.ready = time.Now().Add(delay)
				continue
			}
			switch out.ErrorCode {
			case 30, 31, 40:
				d.key = ""
				continue
			}
			if out.refusesAddress() {
				return nil, &RefusedError{Err: err}
			}
			return nil, err
		}
		if out.URL != "" {
			u, err := url.Parse(out.URL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return nil, d.host.errorf("invalid download URL")
			}
			d.expires = time.Time{}
			if expiry, err := strconv.ParseInt(u.Query().Get("temp_url_expires"), 10, 64); err == nil {
				d.expires = time.Unix(expiry, 0)
			}
			d.target = &Target{URL: out.URL, Name: d.name, Size: d.size}
			copy := *d.target
			return &copy, nil
		}
		if out.FreeKey == "" || out.Wait < 0 || out.Wait > int64(config.Keep2ShareMaxWait/time.Second) {
			return nil, d.host.errorf("no download URL or valid free-download ticket returned")
		}
		d.key = out.FreeKey
		d.ready = time.Now().Add(time.Duration(out.Wait) * time.Second)
		waits++
		if waits > config.Keep2ShareWaits {
			return nil, d.host.errorf("free-download wait did not finish")
		}
	}
}

// nextChallenge requests a CAPTCHA image and reads it, returning the challenge
// and up to Keep2ShareCaptchaGuesses readings, most likely first.
func (d *keep2ShareDownload) nextChallenge(ctx context.Context, attempt int) (string, []string, error) {
	solver := d.host.solver
	if solver == nil {
		var err error
		solver, err = keep2ShareOCR()
		if err != nil {
			return "", nil, err
		}
	}
	resolveNote(ctx, fmt.Sprintf("%s: reading CAPTCHA (%d/%d)", d.host.label, attempt, config.Keep2ShareCaptchaAttempts))
	captcha, err := d.host.call(ctx, "requestCaptcha", map[string]string{})
	if err != nil {
		return "", nil, err
	}
	if captcha.Challenge == "" || captcha.ImageURL == "" {
		return "", nil, d.host.errorf("CAPTCHA response contains no image or challenge")
	}
	img, err := d.host.captchaImage(ctx, captcha.ImageURL)
	if err != nil {
		return "", nil, err
	}
	readings, err := solver(ctx, img)
	if err == nil && len(readings) == 0 {
		err = errKeep2ShareOCR
	}
	if err != nil {
		return "", nil, err
	}
	return captcha.Challenge, readings[:min(len(readings), config.Keep2ShareCaptchaGuesses)], nil
}

func (k *Keep2Share) captchaImage(ctx context.Context, raw string) ([]byte, error) {
	// The API still advertises its image over HTTP. Request HTTPS on the
	// public host, as for every other step of the download.
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, k.errorf("invalid CAPTCHA URL")
	}
	if u.Scheme == "http" && k.Match(u) {
		u.Scheme = "https"
	}
	req, err := k.client.NewRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	img, err := k.client.Bytes(req)
	if err != nil {
		return nil, k.errorf("read CAPTCHA: %w", err)
	}
	if len(img) > config.Keep2ShareCaptchaBytes {
		return nil, k.errorf("CAPTCHA image is too large")
	}
	return img, nil
}
