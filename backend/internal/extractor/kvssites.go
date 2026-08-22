package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Kernel Video Sharing installs.
//
// KVS is sold as a turnkey tube site rather than run as one, so a great many
// hosts serve the identical player over the identical page: one flashvars
// object, with the media URL scrambled by the page's own license code. The
// arithmetic that undoes that lives in kvs.go and none of it is per-host, so
// one extractor covers every install and adding a host costs one line.
//
// A host list has two problems, and each is answered here rather than by
// making the list longer:
//
//   - It rots. Sites move platforms, new ones appear, and a list compiled
//     into a binary cannot follow. HEAPLEACH_KVS_HOSTS extends it at runtime.
//   - It will never be complete. There are far more KVS installs than anyone
//     will enumerate — so the direct-link fallback sniffs for the page shape
//     on any URL laid out like a KVS video page, which covers the tail
//     without naming a single host. See kvsSniff.
type KVS struct {
	client *httpx.Client
	host   string
}

// kvsKnownHosts are the installs this build ships knowing about.
//
// Only the presence of the player matters, and the live tests are what
// establish that: a host whose pages stop parsing belongs out of this list
// rather than patched around, since the sniff will still pick it up if it is
// only the domain that moved.
var kvsKnownHosts = []string{
	"thisvid.com",
	"pornzog.com",
	"camwhores.tv",
	"sexvid.xxx",
	"porntrex.com",
	"pornhits.com",
	"fapster.xyz",
}

// kvsMaxAlternates bounds the numbered alternate renditions a page is read
// for. Installs list a handful at most; the bound is against a malformed
// page, not against any real one.
const kvsMaxAlternates = 6

// NewKVSSites builds one extractor per known host, so each is matched on its
// host alone and named for itself in the UI, exactly as a hand-written host
// would be.
func NewKVSSites(cfg *config.Config, client *httpx.Client) []Extractor {
	hosts := kvsKnownHosts
	if extra := cfg.ExtraHostsFor(config.FamilyKVS); len(extra) > 0 {
		hosts = append(append([]string(nil), hosts...), extra...)
	}
	out := make([]Extractor, 0, len(hosts))
	for _, host := range util.Dedupe(hosts) {
		out = append(out, &KVS{client: client, host: strings.ToLower(host)})
	}
	return out
}

// Name labels the host by its registrable name, matching how the
// hand-written extractors name themselves.
func (k *KVS) Name() string {
	name, _, _ := strings.Cut(k.host, ".")
	return name
}

func (k *KVS) Match(u *url.URL) bool { return util.HostMatches(u.Host, k.host) }

// Extract resolves a video page, or a member's whole public catalogue.
func (k *KVS) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	if listing, ok := kvsMemberPath(u); ok {
		return kvsMember(ctx, k.client, listing, k.Name())
	}
	return kvsExtract(ctx, k.client, u, k.Name())
}

// kvsExtract fetches a video page and resolves it. Everything needed is
// inline in the page, so one request resolves a video.
func kvsExtract(ctx context.Context, client *httpx.Client, u *url.URL, label string) (*Result, error) {
	doc, err := client.GetString(ctx, u.String(), httpx.Referer(util.Origin(u)+"/"))
	if err != nil {
		return nil, fmt.Errorf("%s: fetch %s: %w", label, u.Redacted(), err)
	}
	return kvsResult(doc, u, label)
}

// kvsResult turns a fetched page into a result, and is where the host-shaped
// error messages come from. Kept apart from the fetch so it can be tested
// against a page rather than a site.
func kvsResult(doc string, u *url.URL, label string) (*Result, error) {
	vars, err := parseFlashvars(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w (the video may be private or removed, "+
			"or this host may not run the player this handles)", label, err)
	}

	renditions, err := kvsRenditions(vars)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	// KVS serves plain files. An install that has moved to an adaptive
	// stream is reported rather than handled: saving the playlist under the
	// video's name would produce a few kilobytes that look like a finished
	// download and play as nothing.
	direct := make([]mediaCandidate, 0, len(renditions))
	for _, c := range renditions {
		if !c.IsHLS {
			direct = append(direct, c)
		}
	}
	best, ok := bestCandidate(direct)
	if !ok {
		if len(renditions) > 0 {
			return nil, fmt.Errorf("%s: this page offers only an adaptive stream, "+
				"which this host is not expected to and so is not handled", label)
		}
		return nil, fmt.Errorf("%s: no media URL in the player configuration", label)
	}

	title := util.FirstNonEmpty(
		vars["video_title"],
		trimSiteSuffix(firstTitleOf(doc)),
		strings.Trim(u.Path, "/"),
	)

	return &Result{
		Title: title,
		Files: []File{{
			Name:    title + kvsExtension(vars, best.URL),
			URL:     best.URL,
			Size:    -1,
			Headers: httpx.Referer(u.String()),
		}},
	}, nil
}

// kvsRenditions collects and unscrambles every rendition a player
// configuration lists.
//
// video_url is the main one; alternates are numbered video_alt_url,
// video_alt_url2, ... and each may carry a matching _text label naming its
// resolution. Which of them is actually the largest varies by install, so
// they are all read and the labels decide, rather than trusting the order.
func kvsRenditions(vars map[string]string) ([]mediaCandidate, error) {
	license := vars["license_code"]
	var (
		out      []mediaCandidate
		firstErr error
	)
	for i := 0; i <= kvsMaxAlternates; i++ {
		key := "video_url"
		if i > 0 {
			key = "video_alt_url"
			if i > 1 {
				key += strconv.Itoa(i)
			}
		}
		scrambled := vars[key]
		if scrambled == "" {
			continue
		}
		direct, err := kvsRealURL(scrambled, license)
		if err != nil {
			// One rendition that will not unscramble is not fatal while
			// another still might; the first failure is kept in case none do.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, mediaCandidate{
			URL:     direct,
			Quality: qualityOf(util.FirstNonEmpty(vars[key+"_text"], direct)),
			IsHLS:   strings.Contains(direct, ".m3u8"),
		})
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// kvsExtension picks the file extension the player advertises, falling back
// to the one in the URL path.
func kvsExtension(vars map[string]string, direct string) string {
	if postfix := vars["postfix"]; strings.HasPrefix(postfix, ".") {
		return postfix
	}
	if ext := path.Ext(util.NameFromURL(direct)); ext != "" {
		return ext
	}
	return ".mp4"
}

// kvsSniff resolves a URL that is shaped like a KVS video page but whose
// host nobody registered.
//
// The shape is the whole argument for doing this. KVS lays every install out
// as /videos/<id>/<slug>/, which is specific enough to be worth one GET and
// cheap to be wrong about: a page that carries no player configuration falls
// straight through to the direct-link handling that would have run anyway.
// It is what makes the host list a convenience rather than the boundary of
// what works.
func kvsSniff(ctx context.Context, client *httpx.Client, u *url.URL) (*Result, bool) {
	if !kvsVideoPath(u) {
		return nil, false
	}
	res, err := kvsExtract(ctx, client, u, "kvs")
	if err != nil || res == nil || len(res.Files) == 0 {
		return nil, false
	}
	return res, true
}

// kvsVideoPath reports whether a path looks like a KVS video page: a /videos/
// segment somewhere above a final segment that is a slug rather than a file.
func kvsVideoPath(u *url.URL) bool {
	segs := util.PathSegments(u)
	if len(segs) < 2 {
		return false
	}
	if path.Ext(segs[len(segs)-1]) != "" {
		return false // a file, not a page
	}
	for _, seg := range segs[:len(segs)-1] {
		if seg == "videos" || seg == "video" {
			return true
		}
	}
	return false
}
