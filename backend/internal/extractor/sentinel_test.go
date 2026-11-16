// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func sentinelFixture(image, nonce string) sentinelChallenge {
	return sentinelChallenge{N: nonce, M: "synthetic-proof", B: 8, I: base64.StdEncoding.EncodeToString([]byte(image)), V: "/.well-known/checkpoint", P: [][]json.RawMessage{{json.RawMessage(`8`), json.RawMessage(`"navigator.userAgent"`)}}}
}
func sentinelHTML(c sentinelChallenge) string {
	b, _ := json.Marshal(c)
	return `<html><title>Checkpoint | Sentinel</title><script>var C=` + string(b) + `,rounds=0;</script></html>`
}

func TestSentinelSolvesOnOneSessionAndRoute(t *testing.T) {
	for _, routed := range []bool{false, true} {
		for _, status := range []int{200, 403} {
			t.Run(fmt.Sprintf("routed=%t/status=%d", routed, status), func(t *testing.T) {
				posts, reads := 0, 0
				challenge := sentinelFixture("first-image", "nonce-one")
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/.well-known/checkpoint" {
						if cookie, err := r.Cookie("checkpoint"); err == nil && cookie.Value == "synthetic-pass" {
							fmt.Fprint(w, `<html>Search results</html>`)
							return
						}
						w.Header().Set(httpx.HeaderContentType, "text/html")
						w.WriteHeader(status)
						fmt.Fprint(w, sentinelHTML(challenge))
						return
					}
					posts++
					var answer struct {
						N  string
						C  int
						T  *string
						A  map[string]string
						FP map[string]any
					}
					if err := json.NewDecoder(r.Body).Decode(&answer); err != nil {
						t.Error(err)
					}
					if answer.N != challenge.N || answer.A["8"] != r.UserAgent() || answer.FP["ua"] != r.UserAgent() {
						t.Errorf("identity changed: %+v", answer)
					}
					sum := sha256.Sum256([]byte(challenge.M + ":" + strconv.Itoa(answer.C)))
					if binary.BigEndian.Uint32(sum[:4])>>uint(32-challenge.B) != 0 {
						t.Error("invalid browser proof")
					}
					switch posts {
					case 1:
						if answer.T == nil || *answer.T != "FIRST" {
							t.Error("missing first answer")
						}
						challenge = sentinelFixture("second-image", "nonce-two")
					case 2:
						if answer.T == nil || *answer.T != "NEXT2" {
							t.Error("reused rejected image answer")
						}
						challenge = sentinelFixture("", "nonce-three")
						challenge.B = 10
					case 3:
						if answer.T != nil {
							t.Error("image answer leaked into proof-only round")
						}
						http.SetCookie(w, &http.Cookie{Name: "checkpoint", Value: "synthetic-pass", Path: "/"})
						w.WriteHeader(http.StatusNoContent)
						return
					default:
						t.Error("unbounded verification")
					}
					w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
					json.NewEncoder(w).Encode(map[string]any{"retry": challenge})
				})
				srv := httptest.NewServer(handler)
				defer srv.Close()
				client := testClient()
				defer client.CloseIdleConnections()
				ctx, captured := httpx.CaptureChallenges(t.Context())
				raw := srv.URL + "/?search=Synthetic"
				if routed {
					route, err := client.ThroughProxy(srv.URL)
					if err != nil {
						t.Fatal(err)
					}
					defer route.CloseIdleConnections()
					ctx = httpx.WithRouteAgent(ctx, "test-route", route, nil, "Synthetic browser")
					raw = "http://index.example.test/?search=Synthetic"
				}
				jar, _ := cookiejar.New(nil)
				ctx = httpx.WithCookieJar(ctx, jar)
				reader := func(_ context.Context, b []byte, length int, note func(string)) ([]string, error) {
					reads++
					if length != config.SentinelCaptchaLength {
						t.Errorf("length=%d", length)
					}
					if reads == 1 && string(b) == "first-image" {
						return []string{"first"}, nil
					}
					if reads == 2 && string(b) == "second-image" {
						return []string{"next2"}, nil
					}
					t.Fatalf("unexpected image %q", b)
					return nil, nil
				}
				doc, _, err := sentinelPage(ctx, client, raw, nil, reader)
				if err != nil || doc != `<html>Search results</html>` || posts != 3 || reads != 2 || captured() != nil {
					t.Fatalf("doc=%s err=%v posts=%d reads=%d captured=%v", doc, err, posts, reads, captured())
				}
			})
		}
	}
}

func TestSentinelRefusesUnsupportedOrExhaustedChecks(t *testing.T) {
	for _, mode := range []string{"foreign-endpoint", "unknown-question", "excessive-work", "repeated-check", "cookie-not-kept", "cancelled", "missing-ocr"} {
		t.Run(mode, func(t *testing.T) {
			c := sentinelFixture("", "nonce")
			posts := 0
			switch mode {
			case "foreign-endpoint":
				c.V = "https://other.example.test/.well-known/checkpoint"
			case "unknown-question":
				c.P = [][]json.RawMessage{{json.RawMessage(`0`), json.RawMessage(`"fetch('/secrets')"`)}}
			case "excessive-work":
				c.B = config.SentinelMaxProofBits + 1
			case "missing-ocr":
				c.I = base64.StdEncoding.EncodeToString([]byte("image"))
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
					if mode == "cookie-not-kept" {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
					json.NewEncoder(w).Encode(map[string]any{"retry": c})
					return
				}
				w.Header().Set(httpx.HeaderContentType, "text/html")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, sentinelHTML(c))
			}))
			defer srv.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			reader := func(context.Context, []byte, int, func(string)) ([]string, error) {
				return nil, errors.New("OCR unavailable")
			}
			_, _, err := sentinelPage(ctx, testClient(), srv.URL, nil, reader)
			if err == nil {
				t.Fatal("accepted failed check")
			}
			want := 0
			if mode == "repeated-check" {
				want = config.SentinelRounds
			}
			if mode == "cookie-not-kept" {
				want = 1
			}
			if posts != want {
				t.Fatalf("posts=%d want=%d error=%v", posts, want, err)
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if _, ok := errors.AsType[*httpx.ChallengeError](err); mode != "foreign-endpoint" && ok {
				t.Fatalf("failed local solve restarts proxy cycling: %v", err)
			}
		})
	}
}

func TestSentinelProofCancellationAndIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sentinelProof(ctx, "synthetic", 20); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	fp, values := sentinelIdentity("Mozilla/5.0 (Macintosh)")
	if fp["platform"] != "MacIntel" || values["navigator.platform"] != "MacIntel" {
		t.Fatal("browser identity disagrees")
	}
}

func TestBalbumsLateSolveFailureIsNotPartialSuccess(t *testing.T) {
	c := sentinelFixture("", "nonce")
	c.B = config.SentinelMaxProofBits + 1
	index, root, _ := balbumsIndex(t, &linksStub{hostClaim: "albums.example.test", files: 1, title: "Album"}, map[int]string{
		1: balbumsPageHTML("albums.example.test", 1, 2, "first"), 2: sentinelHTML(c),
	})
	_, err := balbumsExtract(t, index, root+"/?search=Synthetic")
	if _, ok := errors.AsType[*sentinelSolveError](err); !ok {
		t.Fatalf("late checkpoint became a partial search: %v", err)
	}
}

func TestSentinelCancellationDuringOCRRemainsCancellation(t *testing.T) {
	c := sentinelFixture("image", "nonce")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, sentinelHTML(c))
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := func(context.Context, []byte, int, func(string)) ([]string, error) {
		cancel()
		return nil, context.Canceled
	}
	_, _, err := sentinelPage(ctx, testClient(), srv.URL, nil, reader)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}
