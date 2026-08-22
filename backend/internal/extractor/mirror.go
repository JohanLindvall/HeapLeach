package extractor

import (
	"context"
	"sync/atomic"

	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Mirrors: one file offered from several equivalent links.
//
// The downloader calls File.Resolve once before the first attempt and again
// before every retry, which is exactly the cadence a failover needs. So a
// mirror set is just a resolver that hands out the next link each time it is
// asked: a host that refuses, stalls, or answers with a web page costs one
// attempt from the transfer's retry budget, and the following attempt lands
// somewhere else.
//
// Nothing here decides that a mirror is bad and drops it. The alternative —
// remembering failures — would have to distinguish a dead mirror from a
// cancelled job, a paused queue and a stall, and get it right without ever
// leaving a file with no source left to try. Rotating blindly is both
// simpler and strictly more forgiving: a mirror that failed once because the
// host was briefly busy is tried again on the next pass.

// Mirrored returns f served from urls, rotating on each attempt.
//
// The first URL is also stored in File.URL, so a caller that never consults
// Resolve still has a link, and so the item shows a sensible source in the
// UI before it starts. Empty and duplicate URLs are dropped; a set that
// reduces to one link leaves f without a resolver, since there is nothing to
// fail over to.
//
// Any resolver already on f is replaced. A host that mints short-lived
// signed links needs a resolver of its own and should build one directly.
func Mirrored(f File, urls []string) File {
	urls = util.Dedupe(urls)
	if len(urls) == 0 {
		return f
	}

	f.URL = urls[0]
	f.Resolve = nil
	if len(urls) == 1 {
		return f
	}

	name, size, headers := f.Name, f.Size, f.Headers
	var attempt atomic.Int64
	f.Resolve = func(context.Context) (*Target, error) {
		i := attempt.Add(1) - 1
		return &Target{
			URL:     urls[int(i%int64(len(urls)))],
			Headers: headers,
			Size:    size,
			Name:    name,
		}, nil
	}
	return f
}

// MirrorHosts rewrites one link across several hosts, keeping the path and
// query untouched. Hosts that serve the same object from interchangeable
// storage servers — gofile's, say — describe their mirrors this way.
//
// The original link leads, so the host's own choice of server is tried
// first, and hosts that do not parse are skipped rather than guessed at.
func MirrorHosts(link string, hosts []string) []string {
	u, err := ParseURL(link)
	if err != nil || u.Host == "" {
		return []string{link}
	}
	out := make([]string, 0, len(hosts)+1)
	out = append(out, link)
	for _, host := range hosts {
		if host == "" || host == u.Host {
			continue
		}
		alt := *u
		alt.Host = host
		out = append(out, alt.String())
	}
	return util.Dedupe(out)
}
