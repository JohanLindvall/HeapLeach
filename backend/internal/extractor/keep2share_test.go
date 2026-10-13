// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

const keep2ShareTestInfo = `{"status":"success","code":200,"name":"First Clip.mp4","is_available":true,"is_folder":false,"size":1234,"isAvailableForFree":true}`

func TestKeep2ShareDirectTicketsAndCooldownSurviveProxyToggle(t *testing.T) {
	var calls atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, _ map[string]string) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "url": fmt.Sprintf("https://cdn.example.test/first-clip?temp_url_expires=%d", time.Now().Add(time.Hour).Unix())})
	})
	f := keep2ShareTestExtract(t, k)
	plain := context.Background()
	routed := httpx.WithRoute(plain, httpx.DirectRoute, k.client, nil)
	for _, ctx := range []context.Context{plain, routed, plain} {
		if _, err := f.Resolve(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("toggling proxies spent another direct-address ticket")
	}
	k.wait(routed, time.Hour)
	if err := k.waiting(plain); !isWait(err) {
		t.Fatal("disabled pool lost its direct-address cooldown")
	}
}

func TestKeep2ShareCooldownIsPerRoute(t *testing.T) {
	var calls atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusNotAcceptable)
			fmt.Fprint(w, `{"status":"error","errorCode":41,"time_wait":3600}`)
			return
		}
		fmt.Fprint(w, `{"status":"success","url":"https://cdn.example.test/first-clip"}`)
	})
	first := httpx.WithRoute(context.Background(), "first", k.client, nil)
	second := httpx.WithRoute(context.Background(), "second", k.client, nil)
	f := keep2ShareTestExtract(t, k)
	if _, err := f.Resolve(first); err == nil {
		t.Fatal("first route was not cooled")
	}
	f = keep2ShareTestExtract(t, k)
	if _, err := f.Resolve(first); err == nil || calls.Load() != 1 {
		t.Fatal("another file spent a CAPTCHA on the same cooling route")
	}
	if _, err := f.Resolve(second); err != nil {
		t.Fatalf("independent route was blocked: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("getUrl calls=%d", calls.Load())
	}
}

func TestKeep2ShareSignedLinkIsNeverReusedOnAnotherRoute(t *testing.T) {
	var calls atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "url": fmt.Sprintf("https://cdn.example.test/first-clip?temp_url_expires=%d", time.Now().Add(time.Hour).Unix())})
	})
	f := keep2ShareTestExtract(t, k)
	first := httpx.WithRoute(context.Background(), "first", k.client, nil)
	second := httpx.WithRoute(context.Background(), "second", k.client, nil)
	for _, ctx := range []context.Context{first, first, second, second} {
		if _, err := f.Resolve(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d; want one redemption per route", calls.Load())
	}
}

func TestKeep2ShareRejectsRestrictedOrMissingFilesBeforeRequestingCaptcha(t *testing.T) {
	for _, tc := range []struct{ name, info, want string }{
		{"removed", `{"status":"success","is_available":false}`, "unavailable"},
		{"premium", `{"status":"success","is_available":true,"isAvailableForFree":false}`, "Premium"},
		{"folder", `{"status":"success","is_available":true,"is_folder":true}`, "folder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/getFileStatus" {
					t.Errorf("requested a download for a restricted file: %s", r.URL.Path)
				}
				_, _ = fmt.Fprint(w, tc.info)
			}))
			defer srv.Close()
			k := NewKeep2Share(httpx.New("test-agent", "en-US", 0, time.Second))
			k.api = srv.URL
			u, _ := url.Parse("https://example.test/file/test-file")
			if _, err := k.Extract(context.Background(), u, Options{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want %q", err, tc.want)
			}
		})
	}
}

func keep2ShareTestSite(t *testing.T, getURL func(http.ResponseWriter, map[string]string)) *Keep2Share {
	t.Helper()
	return keep2ShareProtocolTestSite(t, NewKeep2Share, getURL)
}

func keep2ShareProtocolTestSite(t *testing.T, create func(*httpx.Client) *Keep2Share, getURL func(http.ResponseWriter, map[string]string)) *Keep2Share {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/getFileStatus", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, keep2ShareTestInfo)
	})
	mux.HandleFunc("POST /api/v2/requestCaptcha", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success", "code": 200, "challenge": "test-challenge",
			"captcha_url": "http://" + r.Host + "/captcha",
		})
	})
	mux.HandleFunc("GET /captcha", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "synthetic image, read by the fixture's solver")
	})
	mux.HandleFunc("GET /v1/files/test-file", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"test-file","isAvailableForFree":true}`)
	})
	mux.HandleFunc("POST /api/v2/getUrl", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode download request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get(httpx.HeaderContentType) != httpx.ContentTypeJSON || in["file_id"] != "test-file" {
			t.Errorf("bad download request: headers=%v, fields=%v", r.Header, in)
		}
		getURL(w, in)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	k := create(httpx.New("test-agent", "en-US", 0, time.Second))
	k.api = srv.URL + "/api/v2"
	k.files = srv.URL + "/v1/files"
	k.solver = func(ctx context.Context, raw []byte) ([]string, error) { return []string{"aB3dE7"}, nil }
	return k
}

// keep2ShareFreeAnswers has the site answer whether the file can be had free
// with each of answers in turn, the last one from then on, and counts the
// questions.
func keep2ShareFreeAnswers(t *testing.T, k *Keep2Share, answers ...string) *atomic.Int32 {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(asked.Add(1)) - 1
		_, _ = fmt.Fprint(w, answers[min(n, len(answers)-1)])
	}))
	t.Cleanup(srv.Close)
	k.files = srv.URL
	return &asked
}

const (
	freeFileAnswer    = `{"id":"test-file","isAvailableForFree":true}`
	premiumFileAnswer = `{"id":"test-file","isAvailableForFree":false}`
)

// A file the site offers to premium users alone fails at once, as the site's
// own dialog says, without a CAPTCHA being asked for it: the older API called
// such files free, and the refusal every address then got was taken for each
// address in turn, a CAPTCHA and an hour of a route apiece.
func TestKeep2ShareFailsAPremiumOnlyFileBeforeRequestingACaptcha(t *testing.T) {
	var calls atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"status":"success","code":200,"url":"https://storage.example.test/file"}`)
	})
	keep2ShareFreeAnswers(t, k, premiumFileAnswer)
	_, err := keep2ShareTestExtract(t, k).Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "only for premium users") {
		t.Fatalf("error = %v; want the file reported as premium only", err)
	}
	if _, refused := errors.AsType[*RefusedError](err); refused {
		t.Error("a file nothing can fetch free was taken for a refusal of the address")
	}
	if _, transient := errors.AsType[*TransientError](err); transient {
		t.Error("a file nothing can fetch free was taken for a passing failure")
	}
	if calls.Load() != 0 {
		t.Errorf("asked for a free download %d times; want none", calls.Load())
	}
}

// The bare refusal is all a file gets once the site keeps it for premium
// users, from every address, so the site is asked again before the refusal
// is taken for the address's. Still free, it is the address's.
func TestKeep2ShareAsksAgainBeforeTakingABareRefusalForTheAddresses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []string
		premium bool
	}{
		{"since kept for premium users", []string{freeFileAnswer, premiumFileAnswer}, true},
		{"still free", []string{freeFileAnswer}, false},
		{"no answer to be had", []string{`not json`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
				w.WriteHeader(http.StatusNotAcceptable)
				_, _ = fmt.Fprint(w, `{"status":"error","code":406,"message":"Download is not available","errorCode":42}`)
			})
			asked := keep2ShareFreeAnswers(t, k, tc.answers...)
			_, err := keep2ShareTestExtract(t, k).Resolve(context.Background())
			_, refused := errors.AsType[*RefusedError](err)
			if premium := err != nil && strings.Contains(err.Error(), "only for premium users"); premium != tc.premium || refused == tc.premium {
				t.Fatalf("error = %v (refused %v); want premium only %v", err, refused, tc.premium)
			}
			if asked.Load() != 2 {
				t.Errorf("asked whether the file is free %d times; want before the CAPTCHA and after the refusal", asked.Load())
			}
		})
	}
}

// The API's own codes for a premium-only file and for an address it wants a
// reCAPTCHA from: the first is the file's, the second the address's.
func TestKeep2ShareTellsAPremiumOnlyAnswerFromAReCaptchaDemand(t *testing.T) {
	for _, tc := range []struct {
		code    int
		refused bool
		want    string
	}{
		{43, false, "only for premium users"},
		{33, true, "reCAPTCHA"},
	} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
				w.WriteHeader(http.StatusNotAcceptable)
				_, _ = fmt.Fprintf(w, `{"status":"error","code":406,"message":"Download not available","errorCode":%d}`, tc.code)
			})
			_, err := keep2ShareTestExtract(t, k).Resolve(context.Background())
			if _, refused := errors.AsType[*RefusedError](err); refused != tc.refused || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v (refused %v); want %q, refused %v", err, refused, tc.want, tc.refused)
			}
		})
	}
}

// The same question is the file's Check, which the downloader asks before it
// spends a route on the file, over its own connection: through a failing
// proxy the answer did not come, and the CAPTCHA was spent before the
// refusal. A route in the context is passed by.
func TestKeep2ShareChecksAFileOverTheCallersOwnConnection(t *testing.T) {
	for _, tc := range []struct {
		answer  string
		premium bool
	}{{premiumFileAnswer, true}, {freeFileAnswer, false}} {
		k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
			t.Error("asked for a download link while checking the file")
		})
		asked := keep2ShareFreeAnswers(t, k, tc.answer)
		f := keep2ShareTestExtract(t, k)
		if f.Check == nil {
			t.Fatal("no check offered for the file")
		}
		dead, err := httpx.New("test-agent", "en-US", 0, time.Second).ThroughProxy("http://127.0.0.1:1")
		if err != nil {
			t.Fatal(err)
		}
		err = f.Check(httpx.WithRoute(context.Background(), "dead", dead, nil))
		if got := err != nil && strings.Contains(err.Error(), "only for premium users"); got != tc.premium || (err != nil && !tc.premium) {
			t.Fatalf("check = %v; want premium only %v", err, tc.premium)
		}
		if asked.Load() != 1 {
			t.Fatalf("asked %d times; want once, past the route", asked.Load())
		}
	}
}

// A site that cannot say whether the file is free leaves the free flow as it
// was: the question is a shortcut, never a gate.
func TestKeep2ShareGoesOnWhenTheSiteCannotSayWhetherAFileIsFree(t *testing.T) {
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		_, _ = fmt.Fprint(w, `{"status":"success","code":200,"url":"https://storage.example.test/file"}`)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	k.files = srv.URL
	target, err := keep2ShareTestExtract(t, k).Resolve(context.Background())
	if err != nil || target == nil || target.URL != "https://storage.example.test/file" {
		t.Fatalf("target = %+v, error = %v; want the link", target, err)
	}
}

func keep2ShareTestExtract(t *testing.T, k *Keep2Share) File {
	t.Helper()
	u, _ := url.Parse("https://example.test/file/test-file/obsolete-name.mp4")
	result, err := k.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Title != "First Clip.mp4" || len(result.Files) != 1 {
		t.Fatalf("result = %+v", result)
	}
	f := result.Files[0]
	if f.Name != "First Clip.mp4" || f.Size != 1234 || f.SizeApprox || f.URL != "" || f.Resolve == nil {
		t.Fatalf("file = %+v; want exact metadata with the download deferred", f)
	}
	if f.Pace == nil || f.Pace.Streams != 1 || f.Pace.Files != 1 || f.Pace.Group != "keep2share" {
		t.Fatalf("pace = %+v; free downloads must use one connection", f.Pace)
	}
	return f
}

func TestKeep2ShareSolvesAtTransferTimeAndReusesTheDownload(t *testing.T) {
	var calls atomic.Int32
	link := fmt.Sprintf("https://cdn.example.test/first-clip?temp_url_expires=%d", time.Now().Add(time.Hour).Unix())
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		switch calls.Add(1) {
		case 1:
			if in["captcha_response"] != "aB3dE7" || in["captcha_challenge"] != "test-challenge" {
				t.Errorf("lost the CAPTCHA's case or challenge: %v", in)
			}
			_, _ = fmt.Fprint(w, `{"status":"success","code":200,"free_download_key":"ticket","time_wait":0}`)
		case 2:
			if in["free_download_key"] != "ticket" || in["captcha_response"] != "" || in["captcha_challenge"] != "" {
				t.Errorf("want only the accepted ticket after the wait: %v", in)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "code": 200, "url": link})
		default:
			t.Error("a transfer retry spent another download ticket")
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	f := keep2ShareTestExtract(t, k)
	if calls.Load() != 0 {
		t.Fatal("listing the file started a free download")
	}
	for range 2 {
		target, err := f.Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if target.URL != link || target.Size != 1234 || target.Name != "First Clip.mp4" {
			t.Fatalf("target = %+v", target)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("download calls = %d, want CAPTCHA submission and ticket redemption", calls.Load())
	}
}

func TestKeep2ShareAcceptsTheLastCaptchaAttempt(t *testing.T) {
	var answers atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		if in["free_download_key"] != "" {
			_, _ = fmt.Fprint(w, `{"status":"success","code":200,"url":"https://cdn.example.test/first-clip"}`)
			return
		}
		if answers.Add(1) < int32(config.Keep2ShareCaptchaAttempts) {
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = fmt.Fprint(w, `{"status":"error","code":406,"errorCode":31,"message":"Invalid captcha code"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"status":"success","code":200,"free_download_key":"ticket","time_wait":0}`)
	})
	if _, err := keep2ShareTestExtract(t, k).Resolve(context.Background()); err != nil {
		t.Fatalf("the last allowed answer was accepted but could not finish its wait: %v", err)
	}
}

func TestKeep2ShareBoundsWrongCaptchaAnswers(t *testing.T) {
	var calls, solves atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = fmt.Fprint(w, `{"status":"error","code":406,"errorCode":31,"message":"Invalid captcha code"}`)
	})
	// More readings than may be tried, so both bounds are what stops it.
	k.solver = func(context.Context, []byte) ([]string, error) {
		solves.Add(1)
		return []string{"aB3dE1", "aB3dE2", "aB3dE3", "aB3dE4", "aB3dE5"}, nil
	}
	_, err := keep2ShareTestExtract(t, k).Resolve(context.Background())
	want := config.Keep2ShareCaptchaAttempts * config.Keep2ShareCaptchaGuesses
	if err == nil || !strings.Contains(err.Error(), "CAPTCHA attempts") || calls.Load() != int32(want) ||
		solves.Load() != config.Keep2ShareCaptchaAttempts {
		t.Fatalf("answers=%d images=%d error=%v; want %d answers to %d images, then a bounded failure",
			calls.Load(), solves.Load(), err, want, config.Keep2ShareCaptchaAttempts)
	}
}

func TestKeep2ShareTriesTheNextReadingOnTheSameImage(t *testing.T) {
	var answers []string
	var solves atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		if in["free_download_key"] != "" {
			_, _ = fmt.Fprint(w, `{"status":"success","code":200,"url":"https://cdn.example.test/first-clip"}`)
			return
		}
		answers = append(answers, in["captcha_response"])
		if in["captcha_response"] != "aB3dEl" {
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = fmt.Fprint(w, `{"status":"error","code":406,"errorCode":31,"message":"Invalid captcha code"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"status":"success","code":200,"free_download_key":"ticket","time_wait":0}`)
	})
	k.solver = func(context.Context, []byte) ([]string, error) {
		solves.Add(1)
		return []string{"aB3dEI", "aB3dEl", "aB3dE1"}, nil
	}
	if _, err := keep2ShareTestExtract(t, k).Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if solves.Load() != 1 || !slices.Equal(answers, []string{"aB3dEI", "aB3dEl"}) {
		t.Fatalf("images=%d answers=%q; want the second reading of the same image", solves.Load(), answers)
	}
}

func TestKeep2ShareNeverAnswersASpentChallengeAgain(t *testing.T) {
	var answers []string
	var solves atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		switch {
		case in["free_download_key"] != "":
			// The accepted ticket has lapsed; only a new image can replace it.
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = fmt.Fprint(w, `{"status":"error","code":406,"errorCode":40,"message":"Invalid free download key"}`)
		case in["captcha_response"] == "first1":
			answers = append(answers, in["captcha_response"])
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = fmt.Fprint(w, `{"status":"error","code":406,"errorCode":31,"message":"Invalid captcha code"}`)
		case in["captcha_response"] == "first2":
			answers = append(answers, in["captcha_response"])
			_, _ = fmt.Fprint(w, `{"status":"success","code":200,"free_download_key":"ticket","time_wait":0}`)
		default:
			answers = append(answers, in["captcha_response"])
			_, _ = fmt.Fprint(w, `{"status":"success","code":200,"url":"https://cdn.example.test/first-clip"}`)
		}
	})
	k.solver = func(context.Context, []byte) ([]string, error) {
		if solves.Add(1) == 1 {
			return []string{"first1", "first2", "first3"}, nil
		}
		return []string{"secnd1", "secnd2", "secnd3"}, nil
	}
	if _, err := keep2ShareTestExtract(t, k).Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if solves.Load() != 2 || !slices.Equal(answers, []string{"first1", "first2", "secnd1"}) {
		t.Fatalf("images=%d answers=%q; an accepted challenge's leftover reading was submitted", solves.Load(), answers)
	}
}

func TestKeep2ShareCancellationKeepsAnAcceptedTicket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		if calls.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `{"status":"success","code":200,"free_download_key":"ticket","time_wait":30}`)
			return
		}
		if in["free_download_key"] != "ticket" {
			t.Errorf("lost the accepted ticket on cancellation: %v", in)
		}
		_, _ = fmt.Fprint(w, `{"status":"success","code":200,"url":"https://cdn.example.test/first-clip"}`)
	})
	d := &keep2ShareDownload{host: k, id: "test-file"}
	var notes []string
	ctx = WithResolveNote(ctx, func(note string) {
		notes = append(notes, note)
		// Cancel only once the API has accepted the answer and the resolver
		// reports its wait. No wall-clock race with the HTTP response.
		if strings.Contains(note, "free-download timer") {
			cancel()
		}
	})
	if _, err := d.resolve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolve = %v, want cancellation of the wait", err)
	}
	if st := d.free(httpx.DirectRoute); st.key != "ticket" || st.ready.IsZero() {
		t.Fatal("cancellation forgot an accepted ticket or its remaining wait")
	}
	if len(notes) < 2 || !strings.Contains(notes[0], "CAPTCHA") || !strings.Contains(notes[len(notes)-1], "30s") {
		t.Fatalf("progress notes = %v; want CAPTCHA progress and the host's wait", notes)
	}
	// Advance the host's deadline without waiting it out in the test.
	d.free(httpx.DirectRoute).ready = time.Now()
	if _, err := d.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestKeep2ShareReportsRestrictionsInsteadOfRetryingCaptchas(t *testing.T) {
	var calls atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = fmt.Fprint(w, `{"status":"error","code":406,"errorCode":42,"message":"Download not available","errors":[{"code":7}]}`)
	})
	_, err := keep2ShareTestExtract(t, k).Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Premium") || calls.Load() != 1 {
		t.Fatalf("calls=%d error=%v; want the actual access restriction", calls.Load(), err)
	}
	if _, refused := errors.AsType[*RefusedError](err); refused {
		t.Error("a restriction on the file was taken for a refusal of the address")
	}
}

// "Download is not available" with no reason given is about the address:
// through public proxies the same file got that answer from some and
// tickets from the rest. The downloader can then try another route.
func TestKeep2ShareTakesAnUnexplainedRefusalForTheAddresses(t *testing.T) {
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = fmt.Fprint(w, `{"status":"error","code":406,"message":"Download is not available","errorCode":42}`)
	})
	_, err := keep2ShareTestExtract(t, k).Resolve(context.Background())
	if _, refused := errors.AsType[*RefusedError](err); !refused {
		t.Fatalf("error = %v; want a refusal of the address", err)
	}
	if !strings.Contains(err.Error(), "refused from this address") {
		t.Errorf("error = %q; want it to say the address was refused", err)
	}
}

// A ticket can arrive with seventy minutes on it, which is the wait between
// free downloads by another road. It goes back to the queue rather than
// holding a worker, and the ticket is redeemed when the file comes round.
func TestKeep2ShareSendsALongTicketWaitBackToTheQueue(t *testing.T) {
	var solves atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		if in["free_download_key"] == "ticket" {
			_, _ = fmt.Fprint(w, `{"status":"success","code":200,"url":"https://cdn.example.test/first-clip"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"status":"success","code":200,"free_download_key":"ticket","time_wait":4300}`)
	})
	k.solver = func(context.Context, []byte) ([]string, error) {
		solves.Add(1)
		return []string{"aB3dE7"}, nil
	}
	d := &keep2ShareDownload{host: k, id: "test-file"}
	start := time.Now()
	_, err := d.resolve(context.Background())
	wait, deferred := errors.AsType[*WaitError](err)
	if !deferred || time.Since(start) > 5*time.Second {
		t.Fatalf("resolve = %v after %s; want the ticket's wait handed back to the queue", err, time.Since(start))
	}
	if left := time.Until(wait.Until); left < 71*time.Minute || left > 72*time.Minute {
		t.Errorf("the wait ends in %s, want the ticket's seventy-odd minutes", left)
	}
	if d.free(httpx.DirectRoute).key != "ticket" {
		t.Fatal("the ticket was dropped on the way back to the queue")
	}

	// When it comes round again, the ticket is redeemed: no second CAPTCHA.
	d.free(httpx.DirectRoute).ready = time.Now()
	k.mu.Lock()
	clear(k.cooldowns)
	k.mu.Unlock()
	if target, err := d.resolve(context.Background()); err != nil || target.URL != "https://cdn.example.test/first-clip" {
		t.Fatalf("target=%+v error=%v", target, err)
	}
	if solves.Load() != 1 {
		t.Errorf("solved %d CAPTCHAs, want only the one the ticket came from", solves.Load())
	}
}

func TestKeep2ShareRefreshesExpiredLinksWithTheAcceptedTicket(t *testing.T) {
	var calls atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		calls.Add(1)
		if in["free_download_key"] != "ticket" {
			t.Errorf("a retry should reuse the ticket: %v", in)
		}
		_, _ = fmt.Fprint(w, `{"status":"success","code":200,"url":"https://cdn.example.test/fresh"}`)
	})
	d := &keep2ShareDownload{host: k, id: "test-file"}
	st := d.free(httpx.DirectRoute)
	st.key, st.target, st.expires = "ticket", &Target{URL: "https://cdn.example.test/expired"}, time.Now().Add(-time.Second)
	target, err := d.resolve(context.Background())
	if err != nil || target.URL != "https://cdn.example.test/fresh" || calls.Load() != 1 {
		t.Fatalf("target=%+v error=%v calls=%d", target, err, calls.Load())
	}
}

func TestKeep2ShareReadsTheHostsCooldown(t *testing.T) {
	var response keep2ShareResponse
	if err := json.Unmarshal([]byte(`{"status":"error","errorCode":42,"errors":[{"code":5,"timeRemaining":"123.500000"}]}`), &response); err != nil {
		t.Fatal(err)
	}
	if got := response.retryDelay(); got != 124*time.Second {
		t.Fatalf("delay=%s, want to wait through the stated cooldown", got)
	}
}

func TestKeep2ShareBoundsRepeatedDownloadTimers(t *testing.T) {
	var calls atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"status":"success","code":200,"free_download_key":"ticket","time_wait":0}`)
	})
	_, err := keep2ShareTestExtract(t, k).Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "wait did not finish") || calls.Load() != config.Keep2ShareWaits+1 {
		t.Fatalf("calls=%d error=%v; want a bounded wait when the host keeps returning timers", calls.Load(), err)
	}
}

// The host's wait between free downloads is the best part of an hour. It
// goes back to the queue rather than being sat out in a worker, and it holds
// back every file from this address: the rest learn it without each solving
// a CAPTCHA to be told.
func TestKeep2ShareSendsTheWaitBetweenFreeDownloadsBackToTheQueue(t *testing.T) {
	var answers, solves atomic.Int32
	var cooling atomic.Bool
	cooling.Store(true)
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		answers.Add(1)
		if cooling.Load() {
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = fmt.Fprint(w, `{"status":"error","code":406,"errorCode":42,"message":"Download not available","errors":[{"code":5,"timeRemaining":"3600.000000"}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"status":"success","code":200,"url":"https://cdn.example.test/first-clip"}`)
	})
	k.solver = func(context.Context, []byte) ([]string, error) {
		solves.Add(1)
		return []string{"aB3dE7"}, nil
	}

	start := time.Now()
	_, err := keep2ShareTestExtract(t, k).Resolve(context.Background())
	wait, ok := errors.AsType[*WaitError](err)
	if !ok {
		t.Fatalf("resolve = %v; want the wait handed back to the queue", err)
	}
	if spent := time.Since(start); spent > 5*time.Second {
		t.Errorf("resolve sat out %s of the wait instead of handing it back", spent)
	}
	if left := time.Until(wait.Until); left < 59*time.Minute || left > time.Hour {
		t.Errorf("the wait ends in %s, want the host's hour", left)
	}

	next := keep2ShareTestExtract(t, k)
	if _, err := next.Resolve(context.Background()); !isWait(err) {
		t.Fatalf("another file resolved to %v during the wait; want it held back too", err)
	}
	if solves.Load() != 1 || answers.Load() != 1 {
		t.Errorf("images=%d answers=%d; another file solved a CAPTCHA only to be told to wait",
			solves.Load(), answers.Load())
	}

	// Once it is over, the next turn goes ahead.
	k.mu.Lock()
	k.cooldowns[httpx.DirectRoute] = time.Now().Add(-time.Second)
	k.mu.Unlock()
	cooling.Store(false)
	target, err := next.Resolve(context.Background())
	if err != nil || target.URL != "https://cdn.example.test/first-clip" {
		t.Fatalf("after the wait: target=%+v error=%v", target, err)
	}
}

func isWait(err error) bool {
	_, ok := errors.AsType[*WaitError](err)
	return ok
}

// "Traffic limit exceed" is about the address that asked: through public
// proxies one file got it from seven while five, and the direct address,
// got tickets. It is a long wait for that route, not a failure of the file,
// and other files on the route learn it without solving a CAPTCHA.
func TestKeep2ShareTakesAnAddressLimitForThatRoutesWait(t *testing.T) {
	for name, answer := range map[string]string{
		"traffic": `{"status":"error","code":406,"message":"Download is not available","errorCode":21,"errors":[{"code":2,"message":"Traffic limit exceed"}]}`,
		"count":   `{"status":"error","code":406,"message":"Download is not available","errorCode":42,"errors":[{"code":1,"message":"Download count files exceed"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var solves atomic.Int32
			k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
				w.WriteHeader(http.StatusNotAcceptable)
				_, _ = fmt.Fprint(w, answer)
			})
			k.solver = func(context.Context, []byte) ([]string, error) {
				solves.Add(1)
				return []string{"aB3dE7"}, nil
			}
			_, err := keep2ShareTestExtract(t, k).Resolve(context.Background())
			wait, ok := errors.AsType[*WaitError](err)
			if !ok {
				t.Fatalf("resolve = %v; want the address's wait, not a failure", err)
			}
			if left := time.Until(wait.Until); left < config.Keep2ShareAddressLimitWait-time.Minute || left > config.Keep2ShareAddressLimitWait {
				t.Errorf("the wait ends in %s, want about %s", left, config.Keep2ShareAddressLimitWait)
			}
			if _, err := keep2ShareTestExtract(t, k).Resolve(context.Background()); !isWait(err) || solves.Load() != 1 {
				t.Errorf("another file on the route: error=%v after %d CAPTCHAs; want the same wait, no new CAPTCHA", err, solves.Load())
			}
		})
	}
}

// A storage link is bound to the address that obtained it. Refused because
// another address is using it, it is dropped with its ticket, and the next
// resolve runs the free flow again. Discarding a link already replaced keeps
// its replacement.
func TestKeep2ShareDiscardedLinkIsMintedAgain(t *testing.T) {
	var calls atomic.Int32
	expires := time.Now().Add(time.Hour).Unix()
	links := []string{
		fmt.Sprintf("https://cdn.example.test/first-clip?temp_url_expires=%d&n=1", expires),
		fmt.Sprintf("https://cdn.example.test/first-clip?temp_url_expires=%d&n=2", expires),
	}
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		n := calls.Add(1)
		if n%2 == 1 {
			_, _ = fmt.Fprint(w, `{"status":"success","code":200,"free_download_key":"ticket","time_wait":0}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "code": 200, "url": links[n/2-1]})
	})
	f := keep2ShareTestExtract(t, k)
	first, err := f.Resolve(context.Background())
	if err != nil || first.URL != links[0] || first.Discard == nil {
		t.Fatalf("first = %+v, %v; want the first link, which can be discarded", first, err)
	}
	first.Discard()
	second, err := f.Resolve(context.Background())
	if err != nil || second.URL != links[1] || calls.Load() != 4 {
		t.Fatalf("after discarding: %+v, %v, %d calls; want a new free download", second, err, calls.Load())
	}
	first.Discard() // late, for a link already replaced
	again, err := f.Resolve(context.Background())
	if err != nil || again.URL != links[1] || calls.Load() != 4 {
		t.Fatalf("discarding a replaced link dropped its replacement: %+v, %v, %d calls", again, err, calls.Load())
	}
}

// A file fetched through several routes at once has a free download on each:
// a link per route, kept per route, and one route's ticket timer holds up no
// other route.
func TestKeep2ShareKeepsAFreeDownloadPerRoute(t *testing.T) {
	var calls atomic.Int32
	var slow atomic.Bool
	expires := time.Now().Add(time.Hour).Unix()
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		n := calls.Add(1)
		if in["free_download_key"] == "" { // an answer to a CAPTCHA: a ticket
			wait := 0
			if slow.Load() {
				wait = 2
			}
			_, _ = fmt.Fprintf(w, `{"status":"success","code":200,"free_download_key":"ticket-%d","time_wait":%d}`, n, wait)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "code": 200,
			"url": fmt.Sprintf("https://cdn.example.test/first-clip?temp_url_expires=%d&for=%s", expires, in["free_download_key"])})
	})
	f := keep2ShareTestExtract(t, k)
	client := httpx.New("test", "en", 0, time.Second)
	on := func(route string) context.Context {
		return httpx.WithRoute(context.Background(), route, client, nil)
	}
	a, err := f.Resolve(on("route-a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Resolve(on("route-b"))
	if err != nil || b.URL == a.URL {
		t.Fatalf("second route: %+v, %v; want a link of its own", b, err)
	}
	if again, err := f.Resolve(on("route-a")); err != nil || again.URL != a.URL || calls.Load() != 4 {
		t.Fatalf("first route again: %+v, %v after %d calls; want its own link kept", again, err, calls.Load())
	}

	slow.Store(true)
	waiting, done := make(chan struct{}), make(chan error, 1)
	var once sync.Once
	ctx := WithResolveNote(on("route-c"), func(note string) {
		if strings.Contains(note, "free-download timer") {
			once.Do(func() { close(waiting) })
		}
	})
	go func() {
		_, err := f.Resolve(ctx)
		done <- err
	}()
	<-waiting
	start := time.Now()
	if again, err := f.Resolve(on("route-a")); err != nil || again.URL != a.URL || time.Since(start) > time.Second {
		t.Fatalf("first route during another's ticket timer: %+v, %v after %s", again, err, time.Since(start))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
