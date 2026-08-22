package extractor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// The refusal as the site actually words it. The tail is the failed database
// lookup printed in full, and it is the trap: matching the whole message, or
// anything past its head, would pin an implementation detail that changes
// without the meaning changing with it.
const wetransferGoneBody = `{"message":"Couldn't find Transfer with [WHERE ` +
	"`transfers`.`public_id`" + ` = ?]"}`

// The other permanent refusal, for a transfer that was addressed rather than
// shared.
const wetransferNoAccessBody = `{"message":"No download access to this transfer"}`

// wetransferServer stands in for the download endpoint. The handler sees the
// transfer id and the decoded request body, so a test can pin what was asked
// as well as what came back.
func wetransferServer(t *testing.T, handle func(id string, body map[string]any) (int, string)) *WeTransfer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(segs) != 2 || segs[1] != "download" {
			t.Errorf("endpoint path = %q, want <id>/download", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		status, payload := handle(segs[0], body)
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	return &WeTransfer{
		client: httpx.New("test-agent", "en-US", 0, 5*time.Second),
		api:    srv.URL,
	}
}

func TestWeTransferParse(t *testing.T) {
	cases := map[string]wetransferLink{
		// Shared by link: the transfer and its security hash.
		"https://wetransfer.com/downloads/aaaa1111/hash2222": {id: "aaaa1111", hash: "hash2222"},
		// Mailed to a named recipient, whose id sits between the two.
		"https://wetransfer.com/downloads/aaaa1111/rcpt3333/hash2222": {
			id: "aaaa1111", recipient: "rcpt3333", hash: "hash2222"},
		// A short link lands on whichever of the site's names it chose.
		"https://www.wetransfer.com/downloads/aaaa1111/hash2222": {id: "aaaa1111", hash: "hash2222"},
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := wetransferParse(u)
		if !ok {
			t.Errorf("wetransferParse(%q) rejected a download link", raw)
			continue
		}
		if got != want {
			t.Errorf("wetransferParse(%q) = %+v, want %+v", raw, got, want)
		}
	}
}

func TestWeTransferParseRejectsEverythingElse(t *testing.T) {
	for _, raw := range []string{
		"https://wetransfer.com/",
		"https://wetransfer.com/downloads",
		"https://wetransfer.com/downloads/aaaa1111",
		"https://wetransfer.com/downloads/aaaa1111/rcpt3333/hash2222/extra",
		"https://www.wetransfer.com/redirect/error",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if link, ok := wetransferParse(u); ok {
			t.Errorf("wetransferParse(%q) = %+v, want no transfer", raw, link)
		}
	}
}

// The storage host is the trap: it is a subdomain of the site, so matching
// the domain alone would capture a signed link the user pasted and then fail
// to find a transfer id in it, when the direct fallback would simply have
// downloaded it.
func TestWeTransferMatch(t *testing.T) {
	w := NewWeTransfer(nil)
	cases := map[string]bool{
		"https://wetransfer.com/downloads/aaaa1111/hash2222":          true,
		"https://www.wetransfer.com/downloads/aaaa1111/rcpt/hash2222": true,
		"https://we.tl/t-abcdef1234":                                  true,
		"https://download.wetransfer.com/eugv/aaaa1111/hash2222":      false,
		"https://wetransfer.com/":                                     false,
		"https://wetransfer.com/help-center":                          false,
		"https://example.test/downloads/aaaa1111/hash2222":            false,
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := w.Match(u); got != want {
			t.Errorf("Match(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestWeTransferReason(t *testing.T) {
	gone := wetransferReason("Couldn't find Transfer with [WHERE `transfers`.`public_id` = ?]")
	if !strings.Contains(gone, "no longer exists") {
		t.Errorf("an expired transfer explained as %q", gone)
	}
	access := wetransferReason("No download access to this transfer")
	if !strings.Contains(access, "named recipients") {
		t.Errorf("a recipients-only transfer explained as %q", access)
	}
	// Anything else is not ours to translate: a message this does not know
	// must reach the user as the site wrote it, not as a guess.
	for _, message := range []string{"", "Missing parameter :security_hash", "(suppressed)"} {
		if got := wetransferReason(message); got != "" {
			t.Errorf("wetransferReason(%q) = %q, want nothing", message, got)
		}
	}
}

// The message arrives on a 404, which the client reports as a status error
// without decoding the body — so reading it back out of that error is the
// only way it is ever seen.
func TestWeTransferFailureReadsTheMessageOutOfTheStatusError(t *testing.T) {
	err := wetransferFailure("aaaa1111", &httpx.StatusError{
		Code: http.StatusNotFound, Status: "404 Not Found",
		URL: wetransferAPI + "/aaaa1111/download", Body: wetransferGoneBody,
	})
	if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("error = %q, want the expired-transfer explanation", err)
	}
	// The database lookup in the tail is noise; it must not reach the user.
	if strings.Contains(err.Error(), "public_id") {
		t.Errorf("error = %q, want the site's internals left out", err)
	}
}

// A failure this does not recognise keeps the underlying error, so nothing is
// lost by the two it does.
func TestWeTransferFailurePassesThroughWhatItCannotExplain(t *testing.T) {
	err := wetransferFailure("aaaa1111", &httpx.StatusError{
		Code: http.StatusBadRequest, Status: "400 Bad Request",
		Body: `{"message":"Missing parameter :security_hash"}`,
	})
	if !strings.Contains(err.Error(), "security_hash") {
		t.Errorf("error = %q, want the site's own message kept", err)
	}
}

func TestWeTransferMintAsksForTheWholeTransfer(t *testing.T) {
	const direct = "https://download.example.test/eugv/aaaa1111/hash2222?token=xyz"

	var seen map[string]any
	w := wetransferServer(t, func(id string, body map[string]any) (int, string) {
		if id != "aaaa1111" {
			t.Errorf("transfer id = %q, want aaaa1111", id)
		}
		seen = body
		return http.StatusOK, `{"direct_link":"` + direct + `"}`
	})

	target, err := w.mint(context.Background(), wetransferLink{id: "aaaa1111", hash: "hash2222"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if seen["intent"] != wetransferIntent {
		t.Errorf("intent = %v, want %q", seen["intent"], wetransferIntent)
	}
	if seen["security_hash"] != "hash2222" {
		t.Errorf("security_hash = %v, want hash2222", seen["security_hash"])
	}
	// A publicly shared link has no recipient, and the field is left out
	// rather than sent empty.
	if _, present := seen["recipient_id"]; present {
		t.Errorf("recipient_id = %v, want the field omitted entirely", seen["recipient_id"])
	}
	if target.URL != direct {
		t.Errorf("url = %q, want %q", target.URL, direct)
	}
	// The size is not the archive's; the API reports none, and a guess here
	// would let the downloader skip a file it had never fetched.
	if target.Size != -1 {
		t.Errorf("size = %d, want -1: the API reports no length", target.Size)
	}
	// No extension, so the server's Content-Disposition names the archive.
	if ext := path.Ext(target.Name); ext != "" {
		t.Errorf("name = %q carries extension %q, want none", target.Name, ext)
	}
}

func TestWeTransferMintEchoesTheRecipient(t *testing.T) {
	var seen map[string]any
	w := wetransferServer(t, func(_ string, body map[string]any) (int, string) {
		seen = body
		return http.StatusOK, `{"direct_link":"https://download.example.test/a"}`
	})

	link := wetransferLink{id: "aaaa1111", recipient: "rcpt3333", hash: "hash2222"}
	if _, err := w.mint(context.Background(), link); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if seen["recipient_id"] != "rcpt3333" {
		t.Errorf("recipient_id = %v, want rcpt3333", seen["recipient_id"])
	}
}

func TestWeTransferMintExplainsAnExpiredTransfer(t *testing.T) {
	w := wetransferServer(t, func(string, map[string]any) (int, string) {
		return http.StatusNotFound, wetransferGoneBody
	})
	_, err := w.mint(context.Background(), wetransferLink{id: "aaaa1111", hash: "hash2222"})
	if err == nil {
		t.Fatal("an expired transfer resolved to a link")
	}
	if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("error = %q, want the expired-transfer explanation", err)
	}
}

func TestWeTransferMintExplainsARecipientsOnlyTransfer(t *testing.T) {
	w := wetransferServer(t, func(string, map[string]any) (int, string) {
		return http.StatusForbidden, wetransferNoAccessBody
	})
	_, err := w.mint(context.Background(), wetransferLink{id: "aaaa1111", hash: "hash2222"})
	if err == nil {
		t.Fatal("a recipients-only transfer resolved to a link")
	}
	if !strings.Contains(err.Error(), "named recipients") {
		t.Errorf("error = %q, want the recipients-only explanation", err)
	}
}

// A 200 that carries no link is still a failure, and one that must not be
// reported as an empty download.
func TestWeTransferMintRejectsAnAnswerWithoutALink(t *testing.T) {
	w := wetransferServer(t, func(string, map[string]any) (int, string) {
		return http.StatusOK, `{"direct_link":""}`
	})
	if _, err := w.mint(context.Background(), wetransferLink{id: "aaaa1111", hash: "hash2222"}); err == nil {
		t.Fatal("an answer with no link resolved to something")
	}
}

// The whole reason for minting at extraction time: a permanently dead
// transfer is reported to the user there, instead of becoming a job that
// fails an item three times first.
func TestWeTransferExtractFailsFastOnADeadTransfer(t *testing.T) {
	w := wetransferServer(t, func(string, map[string]any) (int, string) {
		return http.StatusNotFound, wetransferGoneBody
	})
	u, err := url.Parse("https://wetransfer.com/downloads/aaaa1111/hash2222")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Extract(context.Background(), u, Options{}); err == nil {
		t.Fatal("a dead transfer extracted to a job")
	} else if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("error = %q, want the expired-transfer explanation", err)
	}
}

func TestWeTransferExtractMintsPerAttempt(t *testing.T) {
	var calls int
	w := wetransferServer(t, func(string, map[string]any) (int, string) {
		calls++
		return http.StatusOK, `{"direct_link":"https://download.example.test/a?token=` +
			strings.Repeat("x", calls) + `"}`
	})

	u, err := url.Parse("https://wetransfer.com/downloads/aaaa1111/hash2222")
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("found %d files, want the one archive", len(res.Files))
	}

	file := res.Files[0]
	// The link minted at extraction is deliberately discarded: it is signed,
	// and would be spent by the time the item reached the front of the queue.
	if file.URL != "" {
		t.Errorf("url = %q, want none: the link is minted per attempt", file.URL)
	}
	if file.Resolve == nil {
		t.Fatal("no resolver, so a queued transfer could never mint a fresh link")
	}
	if file.Size != -1 {
		t.Errorf("size = %d, want -1", file.Size)
	}

	first, err := file.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	second, err := file.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if first.URL == second.URL {
		t.Errorf("both attempts got %q, want a freshly minted link each time", first.URL)
	}
}

func TestWeTransferExtractRejectsAPageThatIsNotATransfer(t *testing.T) {
	w := wetransferServer(t, func(string, map[string]any) (int, string) {
		t.Error("the endpoint was called for a URL that names no transfer")
		return http.StatusOK, `{}`
	})
	u, err := url.Parse("https://wetransfer.com/downloads/aaaa1111")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Extract(context.Background(), u, Options{}); err == nil {
		t.Fatal("a URL naming no transfer extracted to a job")
	}
}

// A short link carries nothing itself; the id and hash are in the URL the
// redirect lands on, which is why the destination rather than the page is
// what follow returns.
func TestWeTransferFollowTakesTheRedirectDestination(t *testing.T) {
	const dest = "/downloads/aaaa1111/hash2222"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == dest {
			// The download page is an application shell that states none of
			// this; only the URL it is served at matters.
			_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><div id="root"></div></body></html>`))
			return
		}
		http.Redirect(w, r, dest, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	w := &WeTransfer{client: httpx.New("test-agent", "en-US", 0, 5*time.Second), api: srv.URL}
	short, err := url.Parse(srv.URL + "/t-abcdef1234")
	if err != nil {
		t.Fatal(err)
	}

	landed, err := w.follow(context.Background(), short)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	link, ok := wetransferParse(landed)
	if !ok {
		t.Fatalf("the redirect landed on %s, which names no transfer", landed.Path)
	}
	if link.id != "aaaa1111" || link.hash != "hash2222" {
		t.Errorf("followed to %+v, want the transfer the redirect pointed at", link)
	}
}

// A code that stands for nothing is redirected to an error page, and that
// page names no transfer — which is the answer, not a parse failure to
// report as something obscure.
func TestWeTransferFollowLandsOnAnErrorPageForADeadCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect/error" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Redirect(w, r, "/redirect/error", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	w := &WeTransfer{client: httpx.New("test-agent", "en-US", 0, 5*time.Second), api: srv.URL}
	short, _ := url.Parse(srv.URL + "/t-0000000000")

	landed, err := w.follow(context.Background(), short)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if _, ok := wetransferParse(landed); ok {
		t.Errorf("the error page at %s parsed as a transfer", landed.Path)
	}
}
