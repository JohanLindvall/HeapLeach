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

// Keep2Share uses the public download API. File information is available
// without a CAPTCHA; the free download requires an image answer followed by
// a server-specified wait. Both happen at transfer time so a queued file
// does not spend its download ticket before it can use it.
type Keep2Share struct {
	hostSet
	client *httpx.Client
	api    string
	// The production solver uses local OCR. Keeping the image reader apart
	// lets protocol tests exercise refusals and waits without an OCR binary.
	solver func(context.Context, []byte) (string, error)
}

func NewKeep2Share(client *httpx.Client) *Keep2Share {
	return &Keep2Share{
		hostSet: hostSet{"k2s.cc", "keep2share.cc"},
		client:  client,
		api:     "https://k2s.cc/api/v2",
	}
}

func (k *Keep2Share) Name() string { return "keep2share" }

var keep2SharePace = Pace{Streams: 1, Files: 1, Group: "keep2share"}

func (k *Keep2Share) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	parts := util.PathSegments(u)
	if len(parts) < 2 || parts[0] != "file" {
		return nil, errors.New("keep2share: expected a file link (/file/<id>)")
	}
	id := parts[1]
	info, err := k.call(ctx, "getFileStatus", map[string]string{"id": id})
	if err != nil {
		return nil, err
	}
	if info.IsFolder {
		return nil, errors.New("keep2share: folder links are not supported")
	}
	if !info.Available {
		return nil, errors.New("keep2share: file is unavailable or has been removed")
	}
	if info.ForFree != nil && !*info.ForFree {
		return nil, errors.New("keep2share: this file requires a Premium account")
	}
	if info.Name == "" {
		return nil, errors.New("keep2share: file information contains no name")
	}
	size := int64(-1)
	if info.Size != nil && *info.Size >= 0 {
		size = *info.Size
	}
	download := &keep2ShareDownload{host: k, id: id, name: info.Name, size: size}
	return &Result{Title: info.Name, Files: []File{{
		Name: info.Name, Size: size, Pace: &keep2SharePace, Resolve: download.resolve,
	}}}, nil
}

type keep2ShareResponse struct {
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
	for _, detail := range r.Errors {
		if detail.Message != "" {
			message += ": " + detail.Message
		} else {
			switch detail.Code {
			case 3, 7:
				message += ": this file requires a Premium account"
			case 6:
				message += ": only one free download may run at a time"
			case 8:
				message += ": this file is private"
			case 9:
				message += ": this file requires a subscription"
			}
		}
	}
	return "keep2share: " + message
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
	var out keep2ShareResponse
	err := k.client.PostJSON(ctx, k.api+"/"+method, nil, in, &out)
	if err != nil {
		var status *httpx.StatusError
		if !errors.As(err, &status) || json.Unmarshal([]byte(status.Body), &out) != nil || out.Status != "error" {
			return nil, fmt.Errorf("keep2share: %s: %w", method, err)
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
}

func (d *keep2ShareDownload) resolve(ctx context.Context) (*Target, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
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
	for attempts, waits := 0, 0; ; {
		if delay := time.Until(d.ready); delay > 0 {
			if delay > config.Keep2ShareMaxWait-waited {
				return nil, errors.New("keep2share: free-download wait exceeds the waiting limit; retry later")
			}
			resolveNote(ctx, fmt.Sprintf("Keep2Share: waiting %s for the free-download timer", delay.Round(time.Second)))
			if err := util.SleepCtx(ctx, delay); err != nil {
				return nil, err
			}
			waited += delay
		}
		in := map[string]string{"file_id": d.id}
		if d.key != "" {
			in["free_download_key"] = d.key
		} else {
			if attempts >= config.Keep2ShareCaptchaAttempts {
				return nil, fmt.Errorf("keep2share: could not obtain a free download after %d CAPTCHA attempts: %w", attempts, last)
			}
			solver := d.host.solver
			if solver == nil {
				var err error
				solver, err = keep2ShareOCR()
				if err != nil {
					return nil, err
				}
			}
			attempts++
			resolveNote(ctx, fmt.Sprintf("Keep2Share: reading CAPTCHA (%d/%d)", attempts, config.Keep2ShareCaptchaAttempts))
			captcha, err := d.host.call(ctx, "requestCaptcha", map[string]string{})
			if err != nil {
				return nil, err
			}
			if captcha.Challenge == "" || captcha.ImageURL == "" {
				return nil, errors.New("keep2share: CAPTCHA response contains no image or challenge")
			}
			img, err := d.host.captchaImage(ctx, captcha.ImageURL)
			if err != nil {
				return nil, err
			}
			answer, err := solver(ctx, img)
			if err != nil {
				if !errors.Is(err, errKeep2ShareOCR) {
					return nil, err
				}
				last = err
				continue
			}
			in["captcha_challenge"], in["captcha_response"] = captcha.Challenge, answer
		}
		out, err := d.host.call(ctx, "getUrl", in)
		if err != nil {
			if out == nil {
				return nil, err
			}
			last = err
			if delay := out.retryDelay(); delay > 0 {
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
			return nil, err
		}
		if out.URL != "" {
			u, err := url.Parse(out.URL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return nil, errors.New("keep2share: invalid download URL")
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
			return nil, errors.New("keep2share: no download URL or valid free-download ticket returned")
		}
		d.key = out.FreeKey
		d.ready = time.Now().Add(time.Duration(out.Wait) * time.Second)
		waits++
		if waits > config.Keep2ShareWaits {
			return nil, errors.New("keep2share: free-download wait did not finish")
		}
	}
}

func (k *Keep2Share) captchaImage(ctx context.Context, raw string) ([]byte, error) {
	// The API still advertises its image over HTTP. Request HTTPS on the
	// public host, as for every other step of the download.
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("keep2share: invalid CAPTCHA URL")
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
		return nil, fmt.Errorf("keep2share: read CAPTCHA: %w", err)
	}
	if len(img) > config.Keep2ShareCaptchaBytes {
		return nil, errors.New("keep2share: CAPTCHA image is too large")
	}
	return img, nil
}
