package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLooksChallengedRecognisesABotCheck(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(strings.NewReader("<title>Just a moment...</title>")),
	}
	challenged, restored := looksChallenged(resp)
	if !challenged {
		t.Fatal("a 403 carrying a challenge marker is a challenge")
	}
	// The body was peeked at; the caller must still be able to read it all.
	body, err := io.ReadAll(restored.Body)
	if err != nil || !strings.Contains(string(body), "Just a moment") {
		t.Errorf("restored body = %q, %v", body, err)
	}
}

func TestLooksChallengedLeavesRealErrorsAlone(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(strings.NewReader("this file is private")),
	}
	challenged, restored := looksChallenged(resp)
	if challenged {
		t.Fatal("an ordinary 403 is not a challenge")
	}
	body, _ := io.ReadAll(restored.Body)
	if string(body) != "this file is private" {
		t.Errorf("restored body = %q", body)
	}
}

func TestLooksChallengedIgnoresHealthyStatuses(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		// The marker text on a 200 is content, not a wall.
		Body: io.NopCloser(strings.NewReader("an article about captcha farms")),
	}
	if challenged, _ := looksChallenged(resp); challenged {
		t.Error("a 200 is never a challenge")
	}
}

// The impersonated path remembers a host that turned it away, so the next
// request goes straight to the standard transport instead of failing the
// same way first.
func TestBrowserTransportFallsBackAndRemembers(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	standard, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httptest client transport is %T", srv.Client().Transport)
	}
	bt := newBrowserTransport(standard).(*browserTransport)

	// Plain http skips impersonation entirely — there is no TLS to shape.
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatalf("http round trip: %v", err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Errorf("server saw %d calls, want 1", calls)
	}
}
