package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Recovering gofile's signing secret from the script that holds it.
//
// Every content request is signed with a secret baked into gofile's
// wt.obf.js, and gofile rotates it. Pinning a copy in the binary means the
// host stops working the day it changes, so the script is read instead and
// the secret taken out of it.
//
// The script is javascript-obfuscator output, which hides its strings in a
// table rather than in the code: each entry is RC4-encrypted under a
// per-call-site key and base64-encoded with a shuffled alphabet, and the
// table is rotated by a startup loop so an entry's index is not its
// position. Undoing that needs no interpreter, only the three constants the
// file carries in the clear — the alphabet, the table, and the call sites —
// and a search for the one rotation that makes every call site decode.
//
// The rotation search is what makes this self-checking, and self-checking is
// what makes it safe to use. A wrong answer here would be signed and sent,
// and gofile blocks an address that signs badly, so the extraction refuses
// to guess: it takes the secret only when the whole file decodes and exactly
// one decoded string has the shape of a secret. Anything less falls back to
// the configured value.

// gofileScriptPath is where the site loads its signing code from.
const gofileScriptPath = "/js/wt.obf.js"

// The shapes the script's own constants take. They survive regeneration of
// the obfuscation because they are what the scheme is made of, not names it
// chose: a 64-character alphabet with its padding character, a table of
// encoded strings, and calls that pair an index with a key.
var (
	gofileAlphabetPattern  = regexp.MustCompile(`'([A-Za-z0-9+/]{64}=)'`)
	gofileTablePattern     = regexp.MustCompile(`\[((?:'[A-Za-z0-9+/=]*'\s*,\s*){8,}'[A-Za-z0-9+/=]*')\]`)
	gofileTableItemPattern = regexp.MustCompile(`'([A-Za-z0-9+/=]*)'`)
	gofileCallPattern      = regexp.MustCompile(`\(\s*0x([0-9a-fA-F]+)\s*,\s*'([^']{1,32})'\s*\)`)
	gofileHexEscape        = regexp.MustCompile(`\\x([0-9a-fA-F]{2})`)

	// gofileSecretShape is what a secret looks like: hex, and long enough
	// not to be a counter or a colour.
	gofileSecretShape = regexp.MustCompile(`^[0-9a-f]{10,40}$`)
)

// How long a recovered secret is kept when the script says nothing useful
// about it, and the bounds any answer it does give is held to. The floor
// stops a no-store header turning every link into another fetch; the ceiling
// stops a year-long max-age outliving a rotation.
const (
	gofileSecretTTL    = time.Hour
	gofileSecretMinTTL = 5 * time.Minute
	gofileSecretMaxTTL = 12 * time.Hour
)

// ensureSecret returns the secret to sign with, recovering it from gofile's
// own script when what is held has expired.
//
// The lock is held across the fetch on purpose. Several jobs resolving at
// once would otherwise each pull the script; serialising them means the
// first one pays for it and the rest read what it found.
func (g *Gofile) ensureSecret(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// A refused signature means what we hold is wrong, and gofile blocks an
	// address that keeps proving it. Nothing is sent until that has aged out.
	if since := time.Since(g.rejectedAt); !g.rejectedAt.IsZero() && since < gofileRejectionCooldown {
		return "", fmt.Errorf("gofile: the last request was refused as wrongly signed, and "+
			"repeating that gets this address blocked — waiting %s before trying again; "+
			"set HEAPLEACH_GOFILE_SECRET if you know the current secret",
			(gofileRejectionCooldown - since).Round(time.Second))
	}

	// A secret the user configured is their answer, not a guess to improve.
	if g.fallback != config.FallbackGofileSecret {
		return g.fallback, nil
	}

	now := time.Now()
	if g.secret != "" && now.Before(g.secretUntil) {
		return g.secret, nil
	}

	secret, until, err := g.fetchSecret(ctx)
	if err != nil {
		// Falling back is what the pinned copy is for. Holding it briefly
		// keeps a failing script from being re-read once per link.
		g.secret, g.secretUntil = g.fallback, now.Add(gofileSecretMinTTL)
		return g.secret, nil
	}
	g.secret, g.secretUntil = secret, until
	return secret, nil
}

// signatureRejected records that gofile refused a signature, and explains it.
//
// Whatever signed is wrong, so it is dropped rather than reused: the next
// attempt, once the cooldown has passed, reads the script again.
func (g *Gofile) signatureRejected() error {
	g.mu.Lock()
	g.rejectedAt = time.Now()
	g.secret, g.secretUntil = "", time.Time{}
	g.mu.Unlock()

	return fmt.Errorf("gofile: request signature rejected — the secret read from gofile's " +
		"script did not sign correctly, so it has been dropped and will be read again; " +
		"set HEAPLEACH_GOFILE_SECRET to override it")
}

// fetchSecret reads the signing secret out of gofile's own script, and says
// how long the answer may be reused.
func (g *Gofile) fetchSecret(ctx context.Context) (string, time.Time, error) {
	req, err := g.client.NewRequest(ctx, http.MethodGet, g.scriptURL, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	for header, value := range httpx.Referer(gofileRoot + "/") {
		req.Header.Set(header, value)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("fetch signing script: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, config.MaxResponseBytes))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read signing script: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf("fetch signing script: %s", resp.Status)
	}

	secret, err := gofileSecretFrom(string(body))
	if err != nil {
		return "", time.Time{}, err
	}
	return secret, gofileFreshUntil(resp.Header, time.Now()), nil
}

// gofileFreshUntil reads how long the script may be reused from the caching
// headers it was served with.
//
// Asking the file how long it is good for is better than picking an interval
// here, because the answer tracks whatever gofile does with it: a rotation is
// a new file, and a new file is served with its own freshness. The bounds are
// there because a served answer is advice, not a promise.
func gofileFreshUntil(header http.Header, now time.Time) time.Time {
	clamp := func(d time.Duration) time.Time {
		return now.Add(min(max(d, gofileSecretMinTTL), gofileSecretMaxTTL))
	}

	control := strings.ToLower(header.Get("Cache-Control"))
	if strings.Contains(control, "no-store") || strings.Contains(control, "no-cache") {
		return clamp(0) // as short as this is willing to go
	}
	if maxAge, ok := gofileMaxAge(control); ok {
		// Time already spent in a cache along the way counts against it.
		if age, err := strconv.Atoi(strings.TrimSpace(header.Get("Age"))); err == nil && age > 0 {
			maxAge -= time.Duration(age) * time.Second
		}
		return clamp(maxAge)
	}
	if expires := header.Get("Expires"); expires != "" {
		if when, err := http.ParseTime(expires); err == nil {
			return clamp(when.Sub(now))
		}
	}
	return clamp(gofileSecretTTL)
}

// gofileMaxAge reads the max-age a Cache-Control header states.
func gofileMaxAge(control string) (time.Duration, bool) {
	for part := range strings.SplitSeq(control, ",") {
		value, ok := strings.CutPrefix(strings.TrimSpace(part), "max-age=")
		if !ok {
			continue
		}
		seconds, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	return 0, false
}

// gofileSecretFrom extracts the secret from a fetched script.
//
// Kept apart from the fetch so it can be tested against a script rather than
// against the site — which matters more here than usual, since the way to
// test this against the site is to sign something wrongly.
func gofileSecretFrom(script string) (string, error) {
	// The file writes most of its characters as \xNN escapes. Undoing that
	// first means every pattern below can look for what the string says
	// rather than for how it was spelled.
	script = gofileHexEscape.ReplaceAllStringFunc(script, func(esc string) string {
		return string(rune(hexByte(esc[2], esc[3])))
	})

	alphabet, err := gofileAlphabet(script)
	if err != nil {
		return "", err
	}
	table := gofileTable(script)
	if len(table) == 0 {
		return "", fmt.Errorf("no string table in the signing script")
	}
	calls := gofileCalls(script)
	if len(calls) == 0 {
		return "", fmt.Errorf("no decodable references in the signing script")
	}

	decoded, ok := gofileDecodeAll(table, calls, alphabet)
	if !ok {
		return "", fmt.Errorf("the signing script did not decode; its obfuscation has changed")
	}
	return gofilePickSecret(decoded)
}

// gofileAlphabet finds the base64 alphabet the script decodes with, which is
// shuffled rather than standard.
func gofileAlphabet(script string) (string, error) {
	m := gofileAlphabetPattern.FindStringSubmatch(script)
	if m == nil {
		return "", fmt.Errorf("no base64 alphabet in the signing script")
	}
	return m[1], nil
}

// gofileTable finds the encoded strings, taking the longest table in the
// file so a stray short array cannot be mistaken for it.
func gofileTable(script string) []string {
	var table []string
	for _, m := range gofileTablePattern.FindAllStringSubmatch(script, -1) {
		items := gofileTableItemPattern.FindAllStringSubmatch(m[1], -1)
		if len(items) <= len(table) {
			continue
		}
		table = table[:0]
		for _, item := range items {
			table = append(table, item[1])
		}
	}
	return table
}

// gofileCall is one reference to the table: an index and the key its entry
// was encrypted under.
type gofileCall struct {
	index int
	key   string
}

// gofileCalls finds the references, which is where the keys live.
func gofileCalls(script string) []gofileCall {
	var calls []gofileCall
	seen := make(map[gofileCall]bool)
	for _, m := range gofileCallPattern.FindAllStringSubmatch(script, -1) {
		index := 0
		for i := 0; i < len(m[1]); i++ {
			index = index*16 + int(hexDigit(m[1][i]))
		}
		call := gofileCall{index: index, key: m[2]}
		if !seen[call] {
			seen[call] = true
			calls = append(calls, call)
		}
	}
	return calls
}

// gofileDecodeAll finds the table's rotation and decodes every reference.
//
// The script's own startup loop rotates the table, and the decoder subtracts
// a constant from every index, so an index does not name a position. Neither
// value has to be parsed out: they only ever appear added together, and
// there are just as many possible sums as there are table entries, so the
// right one is found by trying them. The right one is unmistakable — it is
// the offset at which every reference in the file decodes to readable text,
// where a wrong one produces noise almost immediately.
func gofileDecodeAll(table []string, calls []gofileCall, alphabet string) ([]string, bool) {
	for offset := range table {
		decoded := make([]string, 0, len(calls))
		ok := true
		for _, call := range calls {
			text, err := gofileDecodeEntry(table[((call.index+offset)%len(table)+len(table))%len(table)], call.key, alphabet)
			if err != nil || !printableASCII(text) {
				ok = false
				break
			}
			decoded = append(decoded, text)
		}
		if ok {
			return decoded, true
		}
	}
	return nil, false
}

// gofileDecodeEntry undoes one entry: base64 under the shuffled alphabet,
// then RC4 under the call site's key.
//
// The bytes are read as UTF-8 before RC4 runs, because that is the order the
// script does it in — it decodes to text first and enciphers over character
// codes, so decoding over the raw bytes instead produces nothing but noise
// the moment a character needs more than one byte.
func gofileDecodeEntry(encoded, key, alphabet string) (string, error) {
	raw, err := gofileBase64(encoded, alphabet)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("entry is not text")
	}
	return rc4Runes(string(raw), key), nil
}

// gofileBase64 decodes under a shuffled alphabet, six bits at a time as the
// script does, so a value whose length implies no padding still decodes.
func gofileBase64(encoded, alphabet string) ([]byte, error) {
	out := make([]byte, 0, len(encoded)*3/4)
	acc, bits := 0, 0
	for i := 0; i < len(encoded); i++ {
		value := strings.IndexByte(alphabet, encoded[i])
		if value < 0 || value >= 64 {
			continue // padding, or a character this alphabet does not use
		}
		acc = acc<<6 | value
		if bits += 6; bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>bits))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("entry decoded to nothing")
	}
	return out, nil
}

// rc4Runes enciphers text against a key, over characters rather than bytes.
func rc4Runes(text, key string) string {
	keyRunes := []rune(key)
	if len(keyRunes) == 0 {
		return ""
	}

	var state [256]byte
	for i := range state {
		state[i] = byte(i)
	}
	for i, j := 0, 0; i < 256; i++ {
		j = (j + int(state[i]) + int(keyRunes[i%len(keyRunes)])) % 256
		state[i], state[j] = state[j], state[i]
	}

	var out strings.Builder
	out.Grow(len(text))
	i, j := 0, 0
	for _, r := range text {
		i = (i + 1) % 256
		j = (j + int(state[i])) % 256
		state[i], state[j] = state[j], state[i]
		out.WriteRune(r ^ rune(state[(int(state[i])+int(state[j]))%256]))
	}
	return out.String()
}

// gofilePickSecret chooses the secret from everything the script says.
//
// It refuses to choose between candidates. One string of the right shape is
// an answer; several is a guess, and a guess that is wrong gets the user's
// address blocked, so the caller is left to fall back instead.
func gofilePickSecret(decoded []string) (string, error) {
	seen := make(map[string]bool)
	var candidates []string
	for _, text := range decoded {
		if !gofileSecretShape.MatchString(text) || seen[text] || uniformString(text) {
			continue
		}
		seen[text] = true
		candidates = append(candidates, text)
	}

	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return "", fmt.Errorf("the signing script holds nothing shaped like a secret")
	default:
		return "", fmt.Errorf("the signing script holds %d strings shaped like a secret, "+
			"so which one signs is a guess", len(candidates))
	}
}

// uniformString reports a string of one repeated character, which is a
// placeholder rather than a secret.
func uniformString(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}

// printableASCII reports whether text is the sort of string the script would
// have put in its table, which is how a wrong rotation is recognised.
func printableASCII(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < 0x20 || text[i] > 0x7e {
			return false
		}
	}
	return true
}

func hexByte(hi, lo byte) byte { return hexDigit(hi)<<4 | hexDigit(lo) }

func hexDigit(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}
