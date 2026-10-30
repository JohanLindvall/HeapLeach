// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Helpers the public broadcasters share. Their APIs have nothing in common,
// so each keeps its own extractor; what they do share is the shape of what
// comes back — a programme refused for a reason, a series whose episodes are
// resolved when their transfers start, and a note on what a listing left
// out.

// broadcastRefusal is a broadcaster declining a programme: gone, live,
// protected, or shown only somewhere else. It is a type so that a listing
// can leave such programmes out, saying why once, rather than queue them all
// to fail. site is the extractor's prefix and title the programme's name, or
// its id; a refusal that names neither leaves title empty.
type broadcastRefusal struct{ site, title, reason string }

func (e *broadcastRefusal) Error() string {
	if e.title == "" {
		return e.site + ": " + e.reason
	}
	return e.site + ": " + e.title + ": " + e.reason
}

// refusalReason is the reason a broadcaster gave, when err is its refusal.
func refusalReason(err error) (string, bool) {
	if refusal, ok := errors.AsType[*broadcastRefusal](err); ok {
		return refusal.reason, true
	}
	return "", false
}

// resolveListed resolves a listing's items several at a time, keeping the
// files in the listing's order. One item refused or failing must not sink
// the rest; but when nothing resolves, why is the whole answer, and a whole
// series refused is refused for one reason. So the broadcaster's reasons are
// kept in order, and the first other failure beside them, for the caller to
// report: a refusal outranks a failure, being the one the caller can act on.
func resolveListed[T any](ctx context.Context, items []T, resolve func(context.Context, T) (*File, error)) (files []File, refusals []string, failed error) {
	type outcome struct {
		file *File
		err  error
	}
	// FanOut drops an item whose fetch errored, so the error travels as a
	// value instead.
	outcomes := FanOut(ctx, items, func(ctx context.Context, item T) ([]outcome, error) {
		file, err := resolve(ctx, item)
		return []outcome{{file: file, err: err}}, nil
	})
	for _, out := range outcomes {
		switch reason, refused := refusalReason(out.err); {
		case out.err == nil && out.file != nil:
			files = append(files, *out.file)
		case refused:
			refusals = append(refusals, reason)
		case failed == nil:
			failed = out.err
		}
	}
	return files, refusals, failed
}

// lazyEpisode is a listed episode resolved when its transfer starts: a
// series can run to hundreds, each several requests, and a signed stream
// read at listing time can expire in the queue. It keeps the name it is
// listed under, so that a listing read again finds what was saved under it.
func lazyEpisode(name, dir string, headers httpx.Header, resolve func(context.Context) (*Target, error)) File {
	return File{Name: name, Size: -1, Dir: dir, Headers: headers, Resolve: resolve}
}

// refreshing is a resolved programme as its file, read again by resolve
// when its transfer starts, for a stream whose links are signed for less
// time than a queue can take.
func refreshing(target *Target, resolve func(context.Context) (*Target, error)) File {
	file := target.file("")
	file.Resolve = resolve
	return file
}

// listingNote joins what a listing says of itself, leaving out the parts
// that are empty.
func listingNote(parts ...string) string {
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "; ")
}

// cappedNote says a listing of shown items was cut to kept, or nothing when
// it was not.
func cappedNote(kept, shown int, unit string) string {
	if shown <= kept {
		return ""
	}
	return fmt.Sprintf("%d of %d %s", kept, shown, unit)
}

// leftOutNote says n items were left out, and why, or nothing when none
// were. why reads on from "left out": ": <reason>" or " that ...".
func leftOutNote(n int, why string) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d left out%s", n, why)
}

// refusedNote says how many were left out for the reasons given, naming
// the first, or nothing when none were.
func refusedNote(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	return fmt.Sprintf("%d left out: %s", len(reasons), reasons[0])
}

// brokenNote says a listing's walk broke off partway, when it did.
func brokenNote(err error) string {
	if err == nil {
		return ""
	}
	return "the listing broke off partway"
}
