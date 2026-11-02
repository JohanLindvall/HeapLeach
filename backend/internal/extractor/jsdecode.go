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

// jwSetupHLS reads a single HLS file on the JW Player setup object itself.
// Source lists keep jwSource's ordering and DASH refusal; this is only the
// alternative single-file form. Nested preview/track objects are not media.
func jwSetupHLS(script string, base *url.URL) string {
	players := make(map[string]bool)
	if strings.HasPrefix(strings.TrimSpace(script), "<") || strings.Contains(strings.ToLower(script), "<script") {
		root, err := parseHTML(script)
		if err != nil {
			return ""
		}
		// Page prose is not JavaScript: an apostrophe in a heading must
		// not hide the real script, nor may a quoted example become one.
		// Named players survive into later scripts, while each script's
		// comments and string boundaries stay local to that element.
		for _, text := range scriptTexts(root) {
			if link := jwScriptHLS(text, base, players); link != "" {
				return link
			}
			if setup, ok := unpackJS(text); ok {
				if link := jwScriptHLS(setup, base, players); link != "" {
					return link
				}
			}
		}
		return ""
	}
	return jwScriptHLS(script, base, players)
}

func jwScriptHLS(script string, base *url.URL, players map[string]bool) string {
	r := jsTokens{rest: script}
	var recent []string
	for token := r.next(); token != ""; token = r.next() {
		recent = append(recent, token)
		if len(recent) > 8 {
			recent = recent[1:]
		}
		// Both jwplayer("id").setup({...}) and a named player created
		// with player = jwplayer("id"); player.setup({...}) are common.
		if n := len(recent); n >= 6 && token == ")" {
			v := recent[n-6:]
			if v[1] == "=" && v[2] == "jwplayer" && v[3] == "(" {
				players[v[0]] = true
			}
		}
		if token != "{" || len(recent) < 5 {
			continue
		}
		v := recent[len(recent)-5:]
		if v[1] != "." || v[2] != "setup" || v[3] != "(" {
			continue
		}
		direct := len(recent) == 8 && recent[0] == "jwplayer" && recent[1] == "(" && recent[3] == ")"
		if !direct && !players[v[0]] {
			continue
		}
		if link := resolveRef(base, jsObjectFile(&r)); isPlaylistLink(link) {
			return link
		}
		recent = nil
	}
	return ""
}

// jsObjectFile reads only a literal, direct file property of the object
// whose opening brace was consumed. Balanced containers, quoted strings
// and comments keep nested keys and apparent keys inside text out of it.
func jsObjectFile(r *jsTokens) string {
	stack := []byte{'{'}
	var key, file, pending string
	var sources bool
	state := 0 // property key, colon, value, then the next comma or closing brace
	for token := r.next(); token != ""; token = r.next() {
		if len(stack) == 1 {
			if state == 3 && key == "file" {
				if token == "," || token == "}" {
					file = pending
				}
				pending = ""
			}
			switch state {
			case 0:
				if token == "." || token == "[" {
					return "" // spreads and computed keys can override file or sources
				}
				key, state = token, 1
				if value, ok := jsLiteral(token); ok {
					key = value
				}
			case 1:
				state = 3
				if token == ":" {
					state = 2
				} else if key == "file" || key == "sources" || token != "," && token != "}" {
					// A shorthand media property, method or accessor is not
					// a literal source. Only unrelated scalar shorthand can
					// leave an earlier file value intact.
					return ""
				}
			case 2:
				state = 3
				if key == "sources" {
					sources = true
				}
				if key == "file" {
					pending, _ = jsLiteral(token)
					file = ""
				}
			}
		}
		switch token {
		case "{", "[", "(":
			stack = append(stack, token[0])
		case "}", "]", ")":
			if strings.IndexByte("{[(", stack[len(stack)-1]) != strings.IndexByte("}])", token[0]) {
				return ""
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				if sources {
					return "" // source lists are jwSource's, including DASH-only lists
				}
				return file
			}
		case ",":
			if len(stack) == 1 {
				state = 0
			}
		}
	}
	return "" // an incomplete configuration supplies no trusted file
}

// jsTokens scans just the literals, names and punctuation needed to locate
// a setup object, without evaluating JavaScript. Template strings are opaque.
type jsTokens struct{ rest string }

func (r *jsTokens) next() string {
	for {
		r.rest = strings.TrimLeft(r.rest, " \t\r\n")
		switch {
		case strings.HasPrefix(r.rest, "//"):
			_, r.rest, _ = strings.Cut(r.rest, "\n")
		case strings.HasPrefix(r.rest, "/*"):
			_, r.rest, _ = strings.Cut(r.rest[2:], "*/")
		default:
			if r.rest == "" {
				return ""
			}
			n := 1
			switch quote := r.rest[0]; quote {
			case '\'', '"', '`':
				for n < len(r.rest) && r.rest[n] != quote {
					if r.rest[n] == '\\' {
						n++
					}
					n++
				}
				if n >= len(r.rest) {
					r.rest = ""
					return ""
				}
				n++
			default:
				if isWordByte(quote) || quote == '$' {
					for n < len(r.rest) && (isWordByte(r.rest[n]) || r.rest[n] == '$') {
						n++
					}
				}
			}
			token := r.rest[:n]
			r.rest = r.rest[n:]
			return token
		}
	}
}

func jsLiteral(token string) (string, bool) {
	if len(token) < 2 || token[0] != '\'' && token[0] != '"' {
		return "", false
	}
	return unescapeJSString(token[1 : len(token)-1]), true
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
