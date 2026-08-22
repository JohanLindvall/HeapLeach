package extractor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// The obfuscation is reproduced here rather than a copy of gofile's script
// being checked in: the scheme is what is under test, the fixture is built
// from it, and nothing in this file came off the site. That also means the
// extraction can be tested at all — the only way to test it against gofile
// is to sign something wrongly, which is what gets an address blocked.

// The alphabet a real bundle shuffles; any 64 characters plus padding will do.
const testAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/="

// encodeEntry produces one table entry: enciphered under the call site's key,
// then base64 under the shuffled alphabet.
//
// rc4Runes enciphers by XOR, so it is its own inverse and serves for both
// directions — which is also a small check that the decoder is symmetric.
func encodeEntry(plain, key string) string {
	enciphered := rc4Runes(plain, key)
	return base64.NewEncoding(testAlphabet[:64]).
		WithPadding(rune(testAlphabet[64])).
		EncodeToString([]byte(enciphered))
}

// buildScript writes a bundle in the shape the real one has: an alphabet, a
// table rotated so an index is not a position, and call sites pairing an
// index with the key its entry was enciphered under.
func buildScript(strings_ []string, keys []string, rotate, base int) string {
	n := len(strings_)
	encoded := make([]string, n)
	for i, plain := range strings_ {
		encoded[i] = encodeEntry(plain, keys[i])
	}

	return assembleScript(encoded, keys, testAlphabet, rotate, base)
}

// assembleScript lays already-encoded entries out the way a real bundle does,
// so the same shape serves a fixture built here and one taken from the site.
func assembleScript(encoded, keys []string, alphabet string, rotate, base int) string {
	n := len(encoded)
	rotated := make([]string, n)
	for i := range rotated {
		rotated[i] = encoded[(i+rotate)%n]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "function a0_0x5789(){var _0x1f=['%s'];return _0x1f;}\n",
		strings.Join(rotated, "','"))
	fmt.Fprintf(&b, "function a0_0x4ed8(_0xa,_0xb){_0xa=_0xa-0x%x;var _0xc='%s';}\n",
		base, alphabet)
	for i, key := range keys {
		// An index names the string's own position, before rotation.
		fmt.Fprintf(&b, "var _0x%d=_0xalias(0x%x,'%s');\n", i, base+i, key)
	}
	return b.String()
}

func TestGofileSecretFrom(t *testing.T) {
	const secret = "9c1de4a70b2f83"
	table := []string{
		"userAgent", "createElement", "application/json", "X-Website-Token",
		secret, "getTime", "toString", "substring", "addEventListener",
		"querySelector", "innerHTML", "https://api.gofile.io",
	}
	keys := []string{"C@M)", "XMs%", "J)Qr", "Du4t", "kL2#", "9zPq", "aB!7",
		"mN$3", "wE6^", "rT8&", "yU0*", "iO5("}

	got, err := gofileSecretFrom(buildScript(table, keys, 7, 0x1de))
	if err != nil {
		t.Fatalf("gofileSecretFrom: %v", err)
	}
	if got != secret {
		t.Errorf("recovered %q, want %q", got, secret)
	}
}

// TestGofileSecretFromFindsAnyRotation covers the search that stands in for
// the script's own startup shuffle: the offset is never parsed, so every
// rotation has to come out the same.
func TestGofileSecretFromFindsAnyRotation(t *testing.T) {
	const secret = "0af31c9e5d7b26"
	table := []string{
		"userAgent", "createElement", "application/json", "X-Website-Token",
		secret, "getTime", "toString", "substring", "addEventListener", "innerHTML",
	}
	keys := []string{"aaaa", "bbbb", "cccc", "dddd", "eeee", "ffff", "gggg", "hhhh", "iiii", "jjjj"}

	for rotate := range table {
		got, err := gofileSecretFrom(buildScript(table, keys, rotate, 0x100))
		if err != nil {
			t.Fatalf("rotation %d: %v", rotate, err)
		}
		if got != secret {
			t.Errorf("rotation %d recovered %q, want %q", rotate, got, secret)
		}
	}
}

// TestGofileSecretFromRefusesToGuess is the safety property. A wrong secret
// is signed and sent, and gofile blocks an address that signs badly, so
// anything short of one unambiguous answer has to fail rather than choose.
func TestGofileSecretFromRefusesToGuess(t *testing.T) {
	keys := []string{"aaaa", "bbbb", "cccc", "dddd", "eeee", "ffff", "gggg", "hhhh", "iiii", "jjjj"}
	filler := []string{"userAgent", "createElement", "application/json", "X-Website-Token",
		"getTime", "toString", "substring", "addEventListener", "innerHTML"}

	t.Run("two candidates", func(t *testing.T) {
		table := append([]string{"9c1de4a70b2f83", "0af31c9e5d7b26"}, filler[:8]...)
		if _, err := gofileSecretFrom(buildScript(table, keys, 3, 0x1de)); err == nil {
			t.Fatal("two secret-shaped strings were resolved to one answer")
		}
	})

	t.Run("no candidate", func(t *testing.T) {
		table := append([]string{"getElementById"}, filler...)
		if _, err := gofileSecretFrom(buildScript(table, keys, 3, 0x1de)); err == nil {
			t.Fatal("a script with no secret produced one")
		}
	})

	t.Run("not the expected obfuscation", func(t *testing.T) {
		if _, err := gofileSecretFrom(`var x = 1; console.log("nothing to see");`); err == nil {
			t.Fatal("an unrelated script produced a secret")
		}
	})

	t.Run("table that does not decode", func(t *testing.T) {
		script := buildScript(append([]string{"9c1de4a70b2f83"}, filler...), keys, 3, 0x1de)
		// Same shape, wrong keys: nothing decodes at any rotation.
		script = strings.ReplaceAll(script, "'aaaa'", "'zzzz'")
		script = strings.ReplaceAll(script, "'bbbb'", "'yyyy'")
		if _, err := gofileSecretFrom(script); err == nil {
			t.Fatal("a table that does not decode produced a secret")
		}
	})
}

// TestGofileFreshUntil covers reading the reuse window off the script's own
// response, which is what keeps the cache tracking gofile's rotations
// instead of an interval guessed here.
func TestGofileFreshUntil(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{
			name:   "max-age",
			header: http.Header{"Cache-Control": {"public, max-age=7200"}},
			want:   2 * time.Hour,
		},
		{
			// Time already spent in a cache along the way is time gone.
			name:   "max-age minus age",
			header: http.Header{"Cache-Control": {"max-age=7200"}, "Age": {"3600"}},
			want:   time.Hour,
		},
		{
			name:   "no-store falls to the floor",
			header: http.Header{"Cache-Control": {"no-store"}},
			want:   gofileSecretMinTTL,
		},
		{
			name:   "a long max-age is capped",
			header: http.Header{"Cache-Control": {"max-age=31536000"}},
			want:   gofileSecretMaxTTL,
		},
		{
			name:   "expires",
			header: http.Header{"Expires": {now.Add(90 * time.Minute).Format(http.TimeFormat)}},
			want:   90 * time.Minute,
		},
		{
			name:   "nothing said",
			header: http.Header{},
			want:   gofileSecretTTL,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := gofileFreshUntil(tc.header, now).Sub(now)
			// Expires is only second-accurate once formatted.
			if diff := got - tc.want; diff > time.Second || diff < -time.Second {
				t.Errorf("fresh for %s, want %s", got, tc.want)
			}
		})
	}
}

// --- the signature itself ---------------------------------------------------

// TestGofileWebsiteToken pins the shape of what gets signed. The expected
// value is built here from the parts rather than taken from the code, so a
// reordered field or a changed separator fails instead of agreeing with
// itself — and getting this wrong is what gofile blocks addresses for.
func TestGofileWebsiteToken(t *testing.T) {
	g := &Gofile{client: httpx.New("test-agent", "en-US", 0, time.Second), lang: "en-US"}
	when := time.Unix(1787378460, 0)

	want := sha256.Sum256([]byte(strings.Join([]string{
		"test-agent", "en-US", "account-token",
		strconv.FormatInt(when.Unix()/int64(gofileSlot/time.Second), 10),
		"9c1de4a70b2f83",
	}, "::")))

	got := g.websiteToken("account-token", "9c1de4a70b2f83", when)
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("signature = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

// TestGofileWebsiteTokenFollowsTheSlot covers the time window mixed into the
// signature: stable inside a slot, different across one.
func TestGofileWebsiteTokenFollowsTheSlot(t *testing.T) {
	g := &Gofile{client: httpx.New("test-agent", "en-US", 0, time.Second), lang: "en-US"}
	base := time.Unix(1787378460, 0)

	if a, b := g.websiteToken("t", "s", base), g.websiteToken("t", "s", base.Add(time.Minute)); a != b {
		t.Error("the signature changed within one slot")
	}
	if a, b := g.websiteToken("t", "s", base), g.websiteToken("t", "s", base.Add(gofileSlot)); a == b {
		t.Error("the signature did not change across a slot")
	}
	if a, b := g.websiteToken("t", "one", base), g.websiteToken("t", "two", base); a == b {
		t.Error("the secret does not affect the signature")
	}
}

// --- the cache, the fallback, and the refusal to keep signing badly ---------

// scriptServer serves a synthetic bundle holding secret, and counts how often
// it is asked for it.
func scriptServer(t *testing.T, secret string, header map[string]string, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	table := []string{
		"userAgent", "createElement", "application/json", "X-Website-Token",
		secret, "getTime", "toString", "substring", "addEventListener", "innerHTML",
	}
	keys := []string{"aaaa", "bbbb", "cccc", "dddd", "eeee", "ffff", "gggg", "hhhh", "iiii", "jjjj"}
	body := buildScript(table, keys, 4, 0x1de)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		for k, v := range header {
			w.Header().Set(k, v)
		}
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func testGofile(t *testing.T, scriptURL string) *Gofile {
	t.Helper()
	return &Gofile{
		client:    httpx.New("test-agent", "en-US", 0, 5*time.Second),
		fallback:  config.FallbackGofileSecret,
		lang:      "en-US",
		scriptURL: scriptURL,
	}
}

func TestGofileEnsureSecretReadsAndCaches(t *testing.T) {
	const secret = "9c1de4a70b2f83"
	srv, hits := scriptServer(t, secret, map[string]string{"Cache-Control": "max-age=3600"}, 0)
	g := testGofile(t, srv.URL)

	for i := range 3 {
		got, err := g.ensureSecret(context.Background())
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		if got != secret {
			t.Fatalf("call %d returned %q, want the script's secret %q", i+1, got, secret)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("read the script %d times, want once — the rest should come from the cache", n)
	}
}

// TestGofileEnsureSecretRereadsWhenStale covers the other half of caching:
// once the freshness the response granted has passed, the script is read
// again rather than a rotation being missed.
func TestGofileEnsureSecretRereadsWhenStale(t *testing.T) {
	srv, hits := scriptServer(t, "9c1de4a70b2f83", map[string]string{"Cache-Control": "max-age=3600"}, 0)
	g := testGofile(t, srv.URL)

	if _, err := g.ensureSecret(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.secretUntil = time.Now().Add(-time.Second)
	g.mu.Unlock()

	if _, err := g.ensureSecret(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("read the script %d times, want twice — the cached answer had expired", n)
	}
}

// TestGofileEnsureSecretFallsBack is the behaviour asked for: a script that
// cannot be read leaves the pinned secret signing rather than nothing.
func TestGofileEnsureSecretFallsBack(t *testing.T) {
	srv, hits := scriptServer(t, "9c1de4a70b2f83", nil, http.StatusInternalServerError)
	g := testGofile(t, srv.URL)

	got, err := g.ensureSecret(context.Background())
	if err != nil {
		t.Fatalf("a failed lookup should fall back, not fail: %v", err)
	}
	if got != config.FallbackGofileSecret {
		t.Errorf("signed with %q, want the pinned fallback %q", got, config.FallbackGofileSecret)
	}

	// And it should not re-read the script for every link while it is failing.
	if _, err := g.ensureSecret(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("read the failing script %d times, want once until the retry window passes", n)
	}
}

// TestGofileEnsureSecretRespectsAnOverride covers the escape hatch: a secret
// the user configured is the answer, and no lookup happens at all.
func TestGofileEnsureSecretRespectsAnOverride(t *testing.T) {
	srv, hits := scriptServer(t, "9c1de4a70b2f83", nil, 0)
	g := testGofile(t, srv.URL)
	g.fallback = "0af31c9e5d7b26" // as HEAPLEACH_GOFILE_SECRET would set it

	got, err := g.ensureSecret(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "0af31c9e5d7b26" {
		t.Errorf("signed with %q, want the configured secret", got)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("read the script %d times, want not at all when the secret was given", n)
	}
}

// TestGofileSignatureRejectedStopsSigning is the guard against the failure
// that costs the most: gofile blocks an address that signs badly, so one
// rejection must not become one rejection per link in the queue.
func TestGofileSignatureRejectedStopsSigning(t *testing.T) {
	srv, hits := scriptServer(t, "9c1de4a70b2f83", nil, 0)
	g := testGofile(t, srv.URL)

	if _, err := g.ensureSecret(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := g.signatureRejected(); err == nil {
		t.Fatal("a rejected signature produced no error")
	}

	before := hits.Load()
	_, err := g.ensureSecret(context.Background())
	if err == nil {
		t.Fatal("signing continued after gofile refused a signature")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("error %q does not say why it stopped", err)
	}
	if hits.Load() != before {
		t.Error("the script was read again during the cooldown, which means a request was on its way")
	}

	// The secret that signed badly is dropped rather than reused.
	g.mu.Lock()
	cached := g.secret
	g.mu.Unlock()
	if cached != "" {
		t.Errorf("kept %q after it was refused", cached)
	}

	// Once the cooldown has passed, it tries again.
	g.mu.Lock()
	g.rejectedAt = time.Now().Add(-gofileRejectionCooldown - time.Second)
	g.mu.Unlock()
	if _, err := g.ensureSecret(context.Background()); err != nil {
		t.Fatalf("still refusing after the cooldown: %v", err)
	}
}

// --- the pieces the decoding is built from ---------------------------------

// TestRC4RunesIsItsOwnInverse is why the fixture can be built with the same
// function that reads it, and why enciphering is symmetric at all.
func TestRC4RunesIsItsOwnInverse(t *testing.T) {
	for _, plain := range []string{"userAgent", "X-Website-Token", "9c1de4a70b2f83", "a", ""} {
		if got := rc4Runes(rc4Runes(plain, "k3y!"), "k3y!"); got != plain {
			t.Errorf("round trip of %q gave %q", plain, got)
		}
	}
	if rc4Runes("anything", "") != "" {
		t.Error("an empty key should encipher to nothing rather than panic")
	}
}

func TestGofileBase64UsesTheShuffledAlphabet(t *testing.T) {
	encoded := base64.NewEncoding(testAlphabet[:64]).
		WithPadding(rune(testAlphabet[64])).EncodeToString([]byte("hello"))

	got, err := gofileBase64(encoded, testAlphabet)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("decoded %q, want %q", got, "hello")
	}
	// Standard base64 orders its alphabet differently, so the same text
	// under it must not decode to the same thing.
	if same, err := gofileBase64(encoded, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="); err == nil && string(same) == "hello" {
		t.Error("the alphabet made no difference, so it is not being used")
	}
	if _, err := gofileBase64("", testAlphabet); err == nil {
		t.Error("an empty entry decoded to something")
	}
}

func TestGofilePickSecret(t *testing.T) {
	tests := []struct {
		name    string
		decoded []string
		want    string
	}{
		{name: "one candidate", decoded: []string{"userAgent", "9c1de4a70b2f83"}, want: "9c1de4a70b2f83"},
		{name: "the same one twice", decoded: []string{"9c1de4a70b2f83", "9c1de4a70b2f83"}, want: "9c1de4a70b2f83"},
		{name: "placeholder ignored", decoded: []string{"00000000000000", "9c1de4a70b2f83"}, want: "9c1de4a70b2f83"},
		{name: "too short to be one", decoded: []string{"abc123", "userAgent"}},
		{name: "not hex", decoded: []string{"getElementById", "userAgent"}},
		{name: "two candidates", decoded: []string{"9c1de4a70b2f83", "0af31c9e5d7b26"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := gofilePickSecret(tc.decoded)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("got %q, want a refusal", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("wanted %q, got error %v", tc.want, err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- against the script gofile actually serves -------------------------------

// The secret held by the saved copy in testdata. Pinned to that file rather
// than to config.FallbackGofileSecret: this asserts what the fixture contains,
// so it keeps catching a broken decoder after gofile rotates and the two
// values part company.
const savedScriptSecret = "12af056dacea0b"

func loadSavedScript(t *testing.T) string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("testdata", "gofile-wt.obf.js"))
	if err != nil {
		t.Fatal(err)
	}
	return string(script)
}

// TestGofileSecretFromSavedScript is the end-to-end check against a real
// bundle: patterns finding the alphabet, the table and the call sites amid
// the code they are buried in, the rotation search settling on one offset,
// and the decode producing the secret.
//
// The synthetic fixtures above cannot cover the first of those, because they
// lay the script out themselves rather than finding it.
func TestGofileSecretFromSavedScript(t *testing.T) {
	got, err := gofileSecretFrom(loadSavedScript(t))
	if err != nil {
		t.Fatalf("gofileSecretFrom: %v", err)
	}
	if got != savedScriptSecret {
		t.Errorf("recovered %q, want %q", got, savedScriptSecret)
	}
}

// TestGofileParsesSavedScript covers the extraction step by step, so a break
// says which part gave way rather than only that the secret did not come out.
func TestGofileParsesSavedScript(t *testing.T) {
	script := gofileHexEscape.ReplaceAllStringFunc(loadSavedScript(t), func(esc string) string {
		return string(rune(hexByte(esc[2], esc[3])))
	})

	alphabet, err := gofileAlphabet(script)
	if err != nil {
		t.Fatalf("alphabet: %v", err)
	}
	if len(alphabet) != 65 {
		t.Errorf("alphabet is %d characters, want 64 plus padding", len(alphabet))
	}

	table := gofileTable(script)
	if len(table) < 100 {
		t.Errorf("found %d table entries, want the whole table", len(table))
	}

	calls := gofileCalls(script)
	if len(calls) < 50 {
		t.Errorf("found %d call sites, want the ones the file makes", len(calls))
	}

	decoded, ok := gofileDecodeAll(table, calls, alphabet)
	if !ok {
		t.Fatal("no rotation decoded every call site")
	}

	// Everything decodes to text, and to the sort of text a browser script
	// is made of rather than to noise that merely happens to be printable.
	found := make(map[string]bool, len(decoded))
	for _, text := range decoded {
		found[text] = true
	}
	for _, want := range []string{"userAgent", "language", savedScriptSecret} {
		if !found[want] {
			t.Errorf("the decoded strings do not include %q", want)
		}
	}
}
