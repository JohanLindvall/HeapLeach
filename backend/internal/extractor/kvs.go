package extractor

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Kernel Video Sharing (KVS) is an off-the-shelf tube-site platform. Its
// player is configured by a `flashvars` object, and on most installs the
// media URL is scrambled: the first path segment after /get_file/<n>/ has
// its leading 32 characters permuted, with the permutation derived from the
// page's own `license_code`. Undoing it is pure arithmetic — no request to
// any extra endpoint.

// kvsHashLength is how many leading characters of the path segment are
// permuted.
const kvsHashLength = 32

// kvsScrambledPrefix marks a URL that needs unscrambling.
const kvsScrambledPrefix = "function/0/"

// flashvarsStart finds the player configuration's opening brace.
var flashvarsStart = regexp.MustCompile(`flashvars\s*=\s*\{`)

// flashvarsBlock returns the body of the player configuration object.
//
// The object is scanned rather than matched against a closing pattern,
// because installs disagree about where the brace goes: some put it on a
// line of its own, others pack the whole object onto one line and close it
// with "};" immediately behind the last value. Scanning also means a brace
// inside a quoted value cannot end the object early, which is the way any
// terminator pattern eventually gets it wrong.
func flashvarsBlock(doc string) (string, bool) {
	loc := flashvarsStart.FindStringIndex(doc)
	if loc == nil {
		return "", false
	}
	// The match ends on the opening brace; the scan starts there and the
	// body handed back is what sits between the braces.
	end, ok := scanObject(doc, loc[1]-1, '\'')
	if !ok {
		return "", false
	}
	return doc[loc[1] : end-1], true
}

// flashvarEntry matches one `key: 'value'` pair, tolerating escaped quotes.
var flashvarEntry = regexp.MustCompile(`(\w+)\s*:\s*'((?:[^'\\]|\\.)*)'`)

// parseFlashvars pulls the player configuration out of a KVS page.
func parseFlashvars(doc string) (map[string]string, error) {
	block, ok := flashvarsBlock(doc)
	if !ok {
		return nil, fmt.Errorf("no player configuration found")
	}
	out := make(map[string]string)
	for _, m := range flashvarEntry.FindAllStringSubmatch(block, -1) {
		out[m[1]] = unescapeJSString(m[2])
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("player configuration is empty")
	}
	return out, nil
}

// unescapeJSString undoes the backslash escapes of a single-quoted JS string.
func unescapeJSString(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default:
			// \' \" \\ \/ and anything else: keep the literal character.
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// kvsRealURL undoes the permutation applied to a scrambled media URL. A URL
// without the marker prefix is already direct and is returned unchanged.
func kvsRealURL(videoURL, licenseCode string) (string, error) {
	if !strings.HasPrefix(videoURL, kvsScrambledPrefix) {
		return videoURL, nil
	}
	raw := strings.TrimPrefix(videoURL, kvsScrambledPrefix)

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("bad media url: %w", err)
	}

	token, err := kvsLicenseToken(licenseCode)
	if err != nil {
		return "", err
	}
	if len(token) < kvsHashLength {
		return "", fmt.Errorf("license token is %d values, need %d", len(token), kvsHashLength)
	}

	parts := strings.Split(u.Path, "/")
	if len(parts) < 4 {
		return "", fmt.Errorf("unexpected media path %q", u.Path)
	}
	segment := parts[3]
	if len(segment) < kvsHashLength {
		return "", fmt.Errorf("media path segment is too short to unscramble")
	}

	// Replay the player's swap sequence: walking backwards, each position is
	// exchanged with one derived from the running sum of the token.
	hash := segment[:kvsHashLength]
	order := make([]int, kvsHashLength)
	for i := range order {
		order[i] = i
	}
	accum := 0
	for src := kvsHashLength - 1; src >= 0; src-- {
		accum += token[src]
		dst := (src + accum) % kvsHashLength
		order[src], order[dst] = order[dst], order[src]
	}

	var unscrambled strings.Builder
	unscrambled.Grow(len(segment))
	for _, index := range order {
		unscrambled.WriteByte(hash[index])
	}
	unscrambled.WriteString(segment[kvsHashLength:])

	parts[3] = unscrambled.String()
	u.Path = strings.Join(parts, "/")
	return u.String(), nil
}

// kvsLicenseToken expands the page's license code into the digit sequence
// that drives the permutation.
func kvsLicenseToken(code string) ([]int, error) {
	code = strings.ReplaceAll(code, "$", "")
	if code == "" {
		return nil, fmt.Errorf("empty license code")
	}
	digits := make([]int, 0, len(code))
	for _, r := range code {
		if r < '0' || r > '9' {
			return nil, fmt.Errorf("license code contains %q", r)
		}
		digits = append(digits, int(r-'0'))
	}

	// Zeros are mapped to ones so both halves stay parseable numbers.
	modified := strings.ReplaceAll(code, "0", "1")
	center := len(modified) / 2
	front, err := strconv.ParseInt(modified[:center+1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("license code front half: %w", err)
	}
	back, err := strconv.ParseInt(modified[center:], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("license code back half: %w", err)
	}

	seed := strconv.FormatInt(4*abs64(front-back), 10)
	if len(seed) > center+1 {
		seed = seed[:center+1]
	}

	token := make([]int, 0, len(seed)*4)
	for i := 0; i < len(seed); i++ {
		current := int(seed[i] - '0')
		for offset := range 4 {
			if i+offset >= len(digits) {
				return nil, fmt.Errorf("license code is too short for its seed")
			}
			token = append(token, (digits[i+offset]+current)%10)
		}
	}
	return token, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
