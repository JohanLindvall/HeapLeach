// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// FourChan resolves 4chan threads through the site's read-only JSON API,
// which needs no key and is served from a separate host to the boards.
type FourChan struct {
	about
	client *httpx.Client
	// api and files are the two hosts this reads, kept as fields only so a
	// test can point them at a fixture server.
	api   string
	files string
}

const (
	fourChanAPI   = "https://a.4cdn.org"
	fourChanFiles = "https://i.4cdn.org"
	fourChanRoot  = "https://boards.4chan.org"
)

// NewFourChan builds the 4chan extractor.
func NewFourChan(client *httpx.Client) *FourChan {
	return &FourChan{client: client, api: fourChanAPI, files: fourChanFiles, about: about{name: "4chan", shape: fourChanShape,
		recognises: "4chan threads (`/<board>/thread/<number>`) whose page renders the thread's own " +
			"`t<number>` container on the board and names the site"}}
}

// fourChanShape is a thread: /<board>/thread/<number>, optionally with the
// slug the site appends.
var fourChanShape = pathShape(`^/[a-z0-9]{1,8}/thread/[0-9]+(/[^/]*)?/?$`)

// Recognise takes a thread page whose board renders this very thread — the
// container the site's scripts number after it — and that calls itself
// 4chan. The archives that mirror the boards share the link's form but
// neither the markup nor the name. A thread page the probe was kept from
// (probeWithheld) is taken when the read-only API, on its own host, answers
// for that board and thread with posts: the API is what Extract reads.
func (f *FourChan) Recognise(ctx context.Context, p *Page) bool {
	if !fourChanShape(p.Final) {
		return false
	}
	if probeWithheld(p) {
		// An archive mirrors the boards' link form and their thread
		// numbers, so a withheld page whose own origin answers as an
		// archive is the archive's (FoolFuuka asks the same question,
		// which the page keeps), even when the thread is still on the
		// board.
		var payload fourChanPayload
		return f.client != nil && !foolFuukaAnswers(ctx, p, f.client) &&
			pageJSON(ctx, p, f.client, f.threadAPI(p.Final), nil, &payload) &&
			len(payload.Posts) > 0 && payload.Posts[0].No > 0
	}
	if !p.Named("4chan") {
		return false
	}
	root := p.HTML()
	if root == nil {
		return false
	}
	want := "t" + p.Segments()[2]
	board := findFirst(root, func(n *html.Node) bool { return isElem(n, atom.Div) && hasClass(n, "board") })
	return board != nil && findFirst(board, func(n *html.Node) bool {
		return isElem(n, atom.Div) && hasClass(n, "thread") && attr(n, "id") == want
	}) != nil
}

// fourChanPayload is a thread as the API answers it.
type fourChanPayload struct {
	Posts []struct {
		No       int64  `json:"no"`
		Subject  string `json:"sub"`
		Time     int64  `json:"tim"`
		Filename string `json:"filename"`
		Ext      string `json:"ext"`
		FileSize int64  `json:"fsize"`
	} `json:"posts"`
}

// threadAPI is the API endpoint of the thread a link of fourChanShape names.
func (f *FourChan) threadAPI(u *url.URL) string {
	segs := util.PathSegments(u)
	if len(segs) < 3 {
		return ""
	}
	return fmt.Sprintf("%s/%s/thread/%s.json", f.api, url.PathEscape(segs[0]), url.PathEscape(segs[2]))
}

// Extract lists every attachment in a thread.
func (f *FourChan) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 3 || segs[1] != "thread" {
		return nil, fmt.Errorf("4chan: %s is not a thread link (expected /<board>/thread/<number>)", u.Redacted())
	}
	board, thread := segs[0], segs[2]

	var payload fourChanPayload
	endpoint := f.threadAPI(u)
	// A recogniser that took a withheld page asked the API already.
	if !askedJSON(ctx, probed(ctx, u), f.client, endpoint, nil, &payload) {
		if err := f.client.GetJSON(ctx, endpoint, httpx.Referer(fourChanRoot+"/"), &payload); err != nil {
			return nil, fmt.Errorf("4chan: fetch thread %s/%s: %w", board, thread, err)
		}
	}
	if len(payload.Posts) == 0 {
		return nil, fmt.Errorf("4chan: thread %s/%s is empty or gone", board, thread)
	}

	headers := httpx.Referer(fourChanRoot + "/" + board + "/")
	var files []File
	for _, post := range payload.Posts {
		if post.Time == 0 || post.Ext == "" {
			continue // a post without an attachment
		}
		stored := strconv.FormatInt(post.Time, 10) + post.Ext
		size := post.FileSize
		if size == 0 {
			size = -1
		}
		files = append(files, File{
			// The stored name keeps posts in order and unique; the poster's
			// original name is not, and often collides.
			Name:    stored,
			URL:     f.files + "/" + board + "/" + stored,
			Size:    size,
			Headers: headers,
		})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("4chan: thread %s/%s has no attachments", board, thread)
	}

	title := strings.TrimSpace(payload.Posts[0].Subject)
	if title == "" {
		title = board + "-" + thread
	}
	return &Result{Title: title, Files: files}, nil
}
