package extractor

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// FourChan resolves 4chan threads through the site's read-only JSON API,
// which needs no key and is served from a separate host to the boards.
type FourChan struct {
	hostSet
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
	return &FourChan{hostSet: hostSet{"4chan.org", "4channel.org"}, client: client, api: fourChanAPI, files: fourChanFiles}
}

func (f *FourChan) Name() string { return "4chan" }

// Extract lists every attachment in a thread.
func (f *FourChan) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 3 || segs[1] != "thread" {
		return nil, fmt.Errorf("4chan: %s is not a thread link (expected /<board>/thread/<number>)", u.Redacted())
	}
	board, thread := segs[0], segs[2]

	var payload struct {
		Posts []struct {
			No       int64  `json:"no"`
			Subject  string `json:"sub"`
			Time     int64  `json:"tim"`
			Filename string `json:"filename"`
			Ext      string `json:"ext"`
			FileSize int64  `json:"fsize"`
		} `json:"posts"`
	}
	endpoint := fmt.Sprintf("%s/%s/thread/%s.json", f.api, url.PathEscape(board), url.PathEscape(thread))
	if err := f.client.GetJSON(ctx, endpoint, httpx.Referer(fourChanRoot+"/"), &payload); err != nil {
		return nil, fmt.Errorf("4chan: fetch thread %s/%s: %w", board, thread, err)
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
