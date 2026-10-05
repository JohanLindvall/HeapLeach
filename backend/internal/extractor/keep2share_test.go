package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

const keep2ShareTestInfo = `{"status":"success","code":200,"name":"First Clip.mp4","is_available":true,"is_folder":false,"size":1234,"isAvailableForFree":true}`

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
	k := NewKeep2Share(httpx.New("test-agent", "en-US", 0, time.Second))
	k.api = srv.URL + "/api/v2"
	k.solver = func(ctx context.Context, raw []byte) (string, error) { return "aB3dE7", nil }
	return k
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
	var calls atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = fmt.Fprint(w, `{"status":"error","code":406,"errorCode":31,"message":"Invalid captcha code"}`)
	})
	_, err := keep2ShareTestExtract(t, k).Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CAPTCHA attempts") || calls.Load() != config.Keep2ShareCaptchaAttempts {
		t.Fatalf("calls=%d error=%v; want a bounded failure", calls.Load(), err)
	}
}

func TestKeep2ShareCancellationKeepsAnAcceptedTicket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, in map[string]string) {
		if calls.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `{"status":"success","code":200,"free_download_key":"ticket","time_wait":3600}`)
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
	if d.key != "ticket" || d.ready.IsZero() {
		t.Fatal("cancellation forgot an accepted ticket or its remaining wait")
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "CAPTCHA") || !strings.Contains(notes[1], "1h0m0s") {
		t.Fatalf("progress notes = %v; want CAPTCHA progress and the host's wait", notes)
	}
	// Advance the host's deadline without waiting an hour in the test.
	d.ready = time.Now()
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
	d := &keep2ShareDownload{host: k, id: "test-file", key: "ticket",
		target: &Target{URL: "https://cdn.example.test/expired"}, expires: time.Now().Add(-time.Second)}
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
