// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Reading a player's script without running it: the JW Player setup's
// source list, an object literal's extent, and the p,a,c,k,e,d packer. None
// of it is particular to one host.

// jwSources isolates a JW Player setup's source list, and the anchor is the
// point of it: a player can configure its seek-bar thumbnails above the
// media as file: "/api/v1/slides?...jpg", a valid URL answering a real
// body, so a pattern that merely looks for file: lands a JPEG on disk under
// the video's name and reports success. Only what is inside sources: is
// media.
var jwSources = regexp.MustCompile(`(?s)\bsources\s*:\s*\[(.*?)]`)

// jwSourceFile reads one entry's link, tolerating a quoted key.
var jwSourceFile = regexp.MustCompile(`["']?file["']?\s*:\s*["']([^"']+)["']`)

// jwSource is the first media link a JW Player setup lists, resolved
// against the page because players write some links relative. DASH is
// passed over, since nothing here joins it into a file; a page usually
// offers HLS beside it.
func jwSource(setup string, base *url.URL) (string, error) {
	var sawDASH bool
	for _, list := range jwSources.FindAllStringSubmatch(setup, -1) {
		for _, entry := range jwSourceFile.FindAllStringSubmatch(list[1], -1) {
			link := resolveRef(base, entry[1])
			switch {
			case link == "":
			case strings.Contains(strings.ToLower(link), ".mpd"):
				sawDASH = true
			default:
				return link, nil
			}
		}
	}
	if sawDASH {
		return "", fmt.Errorf("this file is offered only as a DASH manifest, which this does not handle")
	}
	return "", fmt.Errorf("no source list in the player configuration " +
		"(the file may have been removed, or the player may have changed)")
}

// scanObject finds the end of the brace-delimited object that opens at
// doc[start], and reports the index just past its closing brace.
//
// String literals delimited by quote are stepped over whole, backslash
// escapes included, so a brace inside a value cannot end the object early.
// The quote character is a parameter because the two configurations read
// here disagree about it: a JSON object quotes with '"', a KVS flashvars
// block with '\”. A document that ends before the object closes reports
// false.
func scanObject(doc string, start int, quote byte) (int, bool) {
	depth, inString, escaped := 0, false, false
	for i := start; i < len(doc); i++ {
		c := doc[i]
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == quote:
				inString = false
			}
		case c == quote:
			inString = true
		case c == '{':
			depth++
		case c == '}':
			if depth--; depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// packedScript matches a script compressed by the p,a,c,k,e,d packer, which
// is what these players ship instead of readable source.
var packedScript = regexp.MustCompile(
	`(?s)\}\s*\(\s*'(.*?)'\s*,\s*(\d+)\s*,\s*(\d+)\s*,\s*'(.*?)'\s*\.\s*split\(\s*'\|'\s*\)`)

// unpackJS reverses that packer.
//
// The scheme is a dictionary substitution: every identifier in the payload
// was replaced by its index in a word list, written in base a. Undoing it is
// walking the payload word by word and putting the words back — no
// evaluation, and nothing host-specific.
func unpackJS(script string) (string, bool) {
	m := packedScript.FindStringSubmatch(script)
	if m == nil {
		return "", false
	}
	payload := unescapeJSString(m[1])
	base, err := strconv.Atoi(m[2])
	if err != nil || base < 2 || base > 62 {
		return "", false
	}
	count, err := strconv.Atoi(m[3])
	if err != nil || count < 0 {
		return "", false
	}
	words := strings.Split(unescapeJSString(m[4]), "|")
	count = min(count, len(words))

	var out strings.Builder
	out.Grow(len(payload))
	for i := 0; i < len(payload); {
		if !isWordByte(payload[i]) {
			out.WriteByte(payload[i])
			i++
			continue
		}
		start := i
		for i < len(payload) && isWordByte(payload[i]) {
			i++
		}
		word := payload[start:i]
		if index, ok := packerIndex(word, base); ok && index < count && words[index] != "" {
			out.WriteString(words[index])
			continue
		}
		out.WriteString(word)
	}
	return out.String(), true
}

// isWordByte reports whether a byte can be part of a packed token.
func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// packerIndexMax bounds the value a token may decode to. A packed index is
// small, while the payload also carries ordinary words whose characters all
// happen to be digits of the base (a long lowercase run, a hex token in a
// URL). Unbounded, those overflow the accumulator, and a wrapped-negative
// "index" would pass the caller's range check and panic the word lookup.
const packerIndexMax = 1 << 30

// packerIndex reads a token as a number in the packer's alphabet: digits,
// then lowercase, then uppercase, the last only once the word list outgrows
// base 36.
func packerIndex(word string, base int) (int, bool) {
	value := 0
	for i := 0; i < len(word); i++ {
		var digit int
		switch c := word[i]; {
		case c >= '0' && c <= '9':
			digit = int(c - '0')
		case c >= 'a' && c <= 'z':
			digit = int(c-'a') + 10
		case c >= 'A' && c <= 'Z':
			digit = int(c-'A') + 36
		default:
			return 0, false
		}
		if digit >= base || value > (packerIndexMax-digit)/base {
			return 0, false
		}
		value = value*base + digit
	}
	return value, len(word) > 0
}
