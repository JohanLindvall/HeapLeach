// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// registryOrder is the order NewRegistry offers a probed page to its
// extractors, each with the engine it is built on, written out here rather
// than read back from the registry: an expectation derived from the code it
// checks would prove nothing. The first to recognise a page takes it, so a
// change here is a change in which extractor wins an overlapping page, and
// has to be made on purpose.
var registryOrder = []string{
	"links:Links", "gofile:Gofile", "mega:Mega", "bunkr:Share", "erome:Erome", "rajce:Share",
	"pixeldrain:Pixeldrain", "keep2share:Keep2Share", "filejump:Share", "emload:Share", "turbo:Share",
	"dropbox:Share", "mediafire:Share", "drive:GoogleDrive",
	"svtplay:SVTPlay", "ruv:RUV", "ard:ARD", "zdf:ZDF", "srf:SRF", "arte:Arte", "orf:ORF", "yle:Yle",
	"drtv:DRTV", "rtbf:RTBF", "francetv:FranceTV", "radio-canada:RadioCanada", "vrtmax:VRTMax",
	"npo:NPOStart", "raiplay:RaiPlay", "rtve:RTVE", "rtpplay:RTPPlay", "pbs:PBS", "npr:NPR",
	"abclisten:ABCListen", "bbcsounds:BBCSounds",
	"vimeo:Vimeo", "niconico:Niconico", "yandex:Pointer", "4chan:FourChan",
	"redgifs:RedGifs", "kemono:Kemono",
	"pornhub:Player", "xhamster:Player", "tnaflix:Player", "drtuber:Player", "pornone:Player",
	"eporner:Player", "pixhost:Share", "imagepond:Share", "suvobox:Share", "filester:Share",
	"bandcamp:Bandcamp", "cyberdrop:Share", "imgur:Imgur", "civitai:Civitai", "bitchute:BitChute",
	"streamable:Player", "wetransfer:Share", "vidmoly:Player", "nrk:NRK", "pornpics:PornPics",
	"archive.org:ArchiveOrg", "feed:Feeds", "autoindex:Autoindex", "fapello:Fapello",
	"coomerfans:CoomerFans", "celeb:Celeb", "cum:Cum",
	"ok.ru:Player", "streamtape:Player", "doodstream:Player", "mixdrop:Player", "lulustream:Player",
	"vids.st:Player", "sendvid:Player", "knuthing:Player", "noodlemagazine:Player",
	"mediawiki:MediaWiki", "booru:Booru",
	"youtube:Handoff", "odysee:Handoff", "dailymotion:Handoff", "bilibili:Handoff",
	"soundcloud:Handoff", "mixcloud:Handoff", "rumble:Handoff",
	"peertube:PeerTube", "chevereto:Chevereto", "bandzoogle:Bandzoogle", "foolfuuka:FoolFuuka",
	"kvs:KVS", "fediverse:Fediverse", "hls:HLSDirect",
	"alohatube:Pointer", "balbums:Balbums", "voyeurking:VoyeurKing",
}

// registered lists a registry's extractors as registryOrder spells them.
func registered(reg *Registry) []string {
	out := make([]string, 0, len(reg.extractors))
	for _, e := range reg.extractors {
		out = append(out, e.Name()+":"+strings.TrimPrefix(fmt.Sprintf("%T", e), "*extractor."))
	}
	return out
}

// TestRegistryOrder pins the registration order, the engine behind each
// name and the fallback, and that building the registry twice gives the
// same answer.
func TestRegistryOrder(t *testing.T) {
	reg := NewRegistry(&config.Config{}, nil)
	got := registered(reg)
	if !slices.Equal(got, registryOrder) {
		for i := range max(len(got), len(registryOrder)) {
			var g, w string
			if i < len(got) {
				g = got[i]
			}
			if i < len(registryOrder) {
				w = registryOrder[i]
			}
			if g != w {
				t.Errorf("position %d: registered %q, want %q", i, g, w)
			}
		}
	}
	if again := registered(NewRegistry(&config.Config{}, nil)); !slices.Equal(got, again) {
		t.Errorf("registration order changed between builds: %v then %v", got, again)
	}
	if d, ok := reg.fallback.(*Direct); !ok || d.Name() != "direct" || d.registry != reg {
		t.Errorf("fallback %T is not Direct wired to its registry", reg.fallback)
	}
}

// TestRegistryHandsItselfToWhatResolvesThroughIt checks that every
// extractor that follows what it finds back through the registry holds the
// registry it is part of, not none or another.
func TestRegistryHandsItselfToWhatResolvesThroughIt(t *testing.T) {
	reg := NewRegistry(&config.Config{}, nil)
	for _, e := range reg.extractors {
		var held *Registry
		switch x := e.(type) {
		case *Links:
			held = x.registry
		case *Pointer:
			held = x.registry
		case *Balbums:
			held = x.registry
		case *VoyeurKing:
			held = x.registry
		default:
			continue
		}
		if held != reg {
			t.Errorf("%s does not hold the registry it is registered in", e.Name())
		}
	}
}

// playerRowProblem names what a Player row leaves out of what the engine
// calls, or "" for a complete row: exactly one of parse and read, a
// canonical page only for a parsed row, withheld only with signing, no
// peek beside eager signing, and a listing with its shape and exactly one
// of walk and links.
func playerRowProblem(site *playerSite) string {
	switch {
	case site.name == "" || site.recognises == "" || site.shape == nil || site.recognise == nil:
		return "a name, an inventory line, a shape or a recogniser is missing"
	case (site.read == nil) == (site.parse == nil):
		return "it needs exactly one of parse and read"
	case site.parse == nil && site.asks != nil:
		return "asks is set on a row that reads rather than parses"
	case site.withheld && !site.signed:
		return "withheld without signed"
	case site.peek != nil && site.signed:
		return "peek beside eager signing"
	case site.listing != nil && site.listing.shape == nil:
		return "a listing without a shape"
	case site.listing != nil && (site.listing.walk == nil) == (site.listing.links == nil):
		return "a listing needs exactly one of walk and links"
	}
	return ""
}

// shareRowProblem names what a Share row leaves out: a name, an inventory
// line and an extract, and forms that each have a shape, at least one of
// which recognises a page. A form without recognise only redirects to
// another, so a row of nothing but those could never be chosen.
func shareRowProblem(s *Share) string {
	site := s.site
	if site.name == "" || site.recognises == "" || site.extract == nil || len(site.forms) == 0 || s.shape == nil {
		return "a name, an inventory line, an extract or its forms are missing"
	}
	recognising := 0
	for i, f := range site.forms {
		if f.shape == nil {
			return fmt.Sprintf("form %d has no shape", i)
		}
		if f.recognise != nil {
			recognising++
		}
	}
	if recognising == 0 {
		return "it has only redirecting forms"
	}
	return ""
}

// pointerRowProblem names what a pointer row leaves out: every field ends
// up in front of the user or decides what is followed.
func pointerRowProblem(site pointerSite) string {
	if site.name == "" || site.shape == nil || site.recognise == nil || site.recognises == "" ||
		site.links == nil || site.missing == "" {
		return "a field is missing"
	}
	return ""
}

// handoffRowProblem names what a handoff row leaves out: the name opens
// each error and labels the host, and why completes the sentence saying
// what yt-dlp is for.
func handoffRowProblem(site handoffSite) string {
	if site.name == "" || site.why == "" || site.recognise == nil || site.shape == nil || site.recognises == "" {
		return "a name, a reason, a form or a recogniser is missing"
	}
	return ""
}

// TestRegisteredRowsAreComplete validates every row each table-driven
// engine is registered with.
func TestRegisteredRowsAreComplete(t *testing.T) {
	for _, e := range NewRegistry(&config.Config{}, nil).extractors {
		var problem string
		switch x := e.(type) {
		case *Player:
			problem = playerRowProblem(x.site)
		case *Share:
			problem = shareRowProblem(x)
		case *Pointer:
			problem = pointerRowProblem(x.site)
		case *Handoff:
			problem = handoffRowProblem(x.site)
		}
		if problem != "" {
			t.Errorf("row %q: %s", e.Name(), problem)
		}
	}
	for _, site := range pointerSites {
		if NewPointer(nil, nil, site.name) == nil {
			t.Errorf("pointer row %q cannot be built by its name", site.name)
		}
	}
	if p := NewPointer(nil, nil, "no-such-pointer"); p != nil {
		t.Errorf("an unknown pointer name built %v, want nil", p)
	}
}

// declaredRows reads the package's own source for every row it declares:
// each package-level playerSite and shareSite value, and each element of
// the pointer and handoff tables, by the name written in it. It reads the
// source rather than a list because a list kept beside the registry is the
// duplicate bookkeeping a forgotten row hides behind.
func declaredRows(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	add := func(engine string, lit *ast.CompositeLit) {
		name := rowName(lit)
		if name == "" {
			t.Errorf("a %s row declares no literal name", engine)
			return
		}
		if prev, dup := rows[name]; dup {
			t.Errorf("row %q is declared twice (%s and %s)", name, prev, engine)
		}
		rows[name] = engine
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		if f.Name.Name != "extractor" {
			continue
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, value := range spec.(*ast.ValueSpec).Values {
					lit, ok := value.(*ast.CompositeLit)
					if !ok {
						continue
					}
					switch typ := lit.Type.(type) {
					case *ast.Ident:
						switch typ.Name {
						case "playerSite":
							add("Player", lit)
						case "shareSite":
							add("Share", lit)
						}
					case *ast.ArrayType:
						elem, ok := typ.Elt.(*ast.Ident)
						if !ok {
							continue
						}
						engine := map[string]string{"pointerSite": "Pointer", "handoffSite": "Handoff"}[elem.Name]
						if engine == "" {
							continue
						}
						for _, el := range lit.Elts {
							if row, ok := el.(*ast.CompositeLit); ok {
								add(engine, row)
							}
						}
					}
				}
			}
		}
	}
	return rows
}

// rowName is the string a row literal gives its name field, looked for
// breadth-first so a row's own name is found before any nested literal's.
func rowName(lit *ast.CompositeLit) string {
	queue := []*ast.CompositeLit{lit}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, el := range cur.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "name" {
				if s, ok := kv.Value.(*ast.BasicLit); ok && s.Kind == token.STRING {
					if v, err := strconv.Unquote(s.Value); err == nil {
						return v
					}
				}
			}
			if nested, ok := kv.Value.(*ast.CompositeLit); ok {
				queue = append(queue, nested)
			}
		}
	}
	return ""
}

// TestEveryDeclaredRowIsRegistered checks that every row the package
// declares is registered exactly once, on its own engine, and that every
// registered row is one the package declares.
func TestEveryDeclaredRowIsRegistered(t *testing.T) {
	declared := declaredRows(t)
	if len(declared) == 0 {
		t.Fatal("no rows found in the package source")
	}
	count := map[string]int{}
	for _, e := range NewRegistry(&config.Config{}, nil).extractors {
		engine := ""
		switch e.(type) {
		case *Player:
			engine = "Player"
		case *Share:
			engine = "Share"
		case *Pointer:
			engine = "Pointer"
		case *Handoff:
			engine = "Handoff"
		default:
			continue
		}
		count[e.Name()]++
		if declared[e.Name()] != engine {
			t.Errorf("%s is registered as a %s row but declared as %q", e.Name(), engine, declared[e.Name()])
		}
	}
	for name, engine := range declared {
		if count[name] != 1 {
			t.Errorf("%s row %q is registered %d times, want once", engine, name, count[name])
		}
	}
}
