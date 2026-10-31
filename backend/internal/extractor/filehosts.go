// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Plain file hosts: a share link stands for one file, and the work is
// turning the page a browser would open into the link the browser would
// eventually follow.
//
// They differ in how far that is. Dropbox needs a query parameter changed.
// Mediafire hands out a link that is on the page but not as an href.
// Google Drive puts a confirmation page in the way of anything large enough
// to be worth downloading. What they have in common is that none of them
// needs a session, so all three are a fetch and a parse.

// ---------------------------------------------------------------- dropbox

// dropbox turns a share link into the download it stands for.

const (
	dropboxRoot = "https://www.dropbox.com"
	// dropboxContentHost serves the same paths as the site itself, and is
	// where a dl=1 link ends up anyway. Kept as a mirror so a transfer that
	// the front end refuses gets a second, different answer.
	dropboxContentHost = "dl.dropboxusercontent.com"
)

// NewDropbox builds the dropbox extractor.
func NewDropbox(client *httpx.Client) *Share { return newShare(client, &dropboxSite) }

// dropboxShape is a share link: /s/<id>/<name> and /sh/<id>/<key>, and the
// newer /scl/fi/<id>/<name> and /scl/fo/<id>/<key>.
var dropboxShape = pathShape(`^/(s|sh)/[A-Za-z0-9_-]+/[^/]+`, `^/scl/(fi|fo)/[A-Za-z0-9_-]+/[^/]+`)

// dropboxBundle is where the web app's viewer bundles are served from.
const dropboxBundle = "/static/atlas/"

// dropboxSite takes a share link whose page is the site's viewer. Nothing on
// it is read — the download is the same link asking for the bytes — so the
// viewer is known by the bundle it loads and the name it gives itself.
var dropboxSite = shareSite{
	name: "dropbox",
	recognises: "share links (`/s/…`, `/sh/…`, `/scl/fi/…`, `/scl/fo/…`) whose page is the Dropbox viewer, " +
		"loading its `/static/atlas/` bundle",
	forms: []shareForm{{dropboxShape, func(p *Page) bool {
		return p.Script(dropboxBundle) && p.Named("Dropbox")
	}}},
	extract: dropboxExtract,
}

// dropboxExtract rewrites the link to ask for the bytes rather than the
// viewer.
//
// A folder link is answered with a zip of the whole folder, which is what
// dropbox itself offers and the only thing a single request can produce. Its
// name is left to the server, whose Content-Disposition names the folder;
// the path carries only an opaque id.
func dropboxExtract(_ context.Context, _ *Share, u *url.URL, _ Options) (*Result, error) {
	direct := *u
	direct.Fragment = ""
	q := direct.Query()
	q.Set("dl", "1")
	q.Del("raw")
	direct.RawQuery = q.Encode()

	name := util.NameFromURL(direct.String())
	if dropboxIsFolder(u) {
		// No extension, so the server's own name wins downstream.
		name = "dropbox-folder"
	}
	if name == "" {
		name = "dropbox-download"
	}

	file := File{Name: name, Size: -1, Headers: httpx.Referer(dropboxRoot + "/")}
	file = Mirrored(file, MirrorHosts(direct.String(), []string{dropboxContentHost}))
	return &Result{Title: name, Files: []File{file}}, nil
}

// dropboxIsFolder recognises the two share shapes that stand for a folder.
func dropboxIsFolder(u *url.URL) bool {
	segs := util.PathSegments(u)
	if len(segs) == 0 {
		return false
	}
	return segs[0] == "sh" || (segs[0] == "scl" && len(segs) > 1 && segs[1] == "fo")
}

// -------------------------------------------------------------- mediafire

// mediafire resolves file pages and folder shares.

const (
	mediafireRoot = "https://www.mediafire.com"
	// mediafireAPI is the API's base, which a test serves (Share.api).
	mediafireAPI = "https://www.mediafire.com/api/1.5"

	// mediafireButtonID marks the anchor holding the real link.
	mediafireButtonID = "downloadButton"
	// mediafireScrambled is where that link hides when the page declines to
	// put it in the href, base64 encoded.
	mediafireScrambled = "data-scrambled-url"
)

// NewMediafire builds the mediafire extractor.
func NewMediafire(client *httpx.Client) *Share { return newShare(client, &mediafireSite) }

// mediafireFileShape is a file page, /file/<key>[/<name>[/file]] and its
// older spellings; mediafireFolderShape is a folder share, /folder/<key>.
// A key is ten to fifteen lower-case letters and digits, which keeps the
// forms off the /file/<x> and /download/<x> paths of every other site.
var (
	mediafireFileShape   = pathShape(`^/(file|file_premium|view|download)/[a-z0-9]{10,15}(/|$)`)
	mediafireFolderShape = pathShape(`^/folder/[a-z0-9]{10,15}(/|$)`)
	// mediafireQueryShape is the oldest link, the key alone as the query.
	mediafireQueryShape = regexp.MustCompile(`^[a-z0-9]{11,15}$`)
)

// mediafireFolderMarker is on the file-row template of the folder viewer.
const mediafireFolderMarker = "mf_filecontainer"

// mediafireSite takes a file page by the download button extract reads its
// link from, and a folder by the viewer's own file-row template — the folder
// itself is listed by the API — each named as the site names itself. The
// old /?<key> link is followed for the file page it redirects to.
var mediafireSite = shareSite{
	name: "mediafire",
	recognises: "file pages (`/file/<key>/…`) carrying the `#downloadButton` link, and folder shares " +
		"(`/folder/<key>`) whose page is the MediaFire folder viewer",
	forms: []shareForm{
		{mediafireFileShape, func(p *Page) bool {
			return p.Named("MediaFire") && p.hasElement(func(n *html.Node) bool {
				return isElem(n, atom.A) && attr(n, "id") == mediafireButtonID &&
					(attr(n, "href") != "" || attr(n, mediafireScrambled) != "")
			})
		}},
		{mediafireFolderShape, func(p *Page) bool { return p.Named("MediaFire") && p.Has(mediafireFolderMarker) }},
		{shape: func(u *url.URL) bool {
			return (u.EscapedPath() == "" || u.EscapedPath() == "/") && mediafireQueryShape.MatchString(u.RawQuery)
		}},
	},
	extract: mediafireExtract,
	api:     mediafireAPI,
}

// mediafireExtract resolves either a single file page or a folder share.
func mediafireExtract(ctx context.Context, s *Share, u *url.URL, opts Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) > 1 && segs[0] == "folder" {
		return mediafireShare(ctx, s, segs[1], opts)
	}
	return signedResult(ctx, mediafireTarget(s, u.String()))
}

// mediafireTarget reads one file page for its download link and filename,
// again at download time: the link on it is signed per visit, so one taken
// while an item waited in the queue would be spent.
func mediafireTarget(s *Share, page string) func(context.Context) (*Target, error) {
	return func(ctx context.Context) (*Target, error) {
		link, name, err := mediafirePage(ctx, s, page)
		if err != nil {
			return nil, err
		}
		return &Target{URL: link, Name: name, Size: -1, Headers: httpx.Referer(page)}, nil
	}
}

// mediafirePage reads one file page for its download link and filename.
func mediafirePage(ctx context.Context, s *Share, page string) (link, name string, err error) {
	pageURL, err := url.Parse(page)
	if err != nil {
		return "", "", fmt.Errorf("mediafire: %w", err)
	}
	root, _, err := fetchRoot(ctx, s.client, pageURL, httpx.Referer(mediafireRoot+"/"))
	if err != nil {
		return "", "", fmt.Errorf("mediafire: fetch %s: %w", page, err)
	}

	button := findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.A) && attr(n, "id") == mediafireButtonID
	})
	if button == nil {
		return "", "", fmt.Errorf("mediafire: no download link on %s "+
			"(the file may have been removed, or need a password)", page)
	}

	link = attr(button, "href")
	if !strings.HasPrefix(link, "http") {
		// Newer pages carry the link only as base64 on the button itself.
		decoded, decErr := util.DecodeBase64(attr(button, mediafireScrambled))
		if decErr != nil || !strings.HasPrefix(string(decoded), "http") {
			return "", "", fmt.Errorf("mediafire: download link on %s is not a URL", page)
		}
		link = string(decoded)
	}

	name = cmp.Or(
		strings.TrimSpace(metaContent(root, "og:title")),
		util.NameFromURL(link),
		util.NameFromURL(page),
	)
	return link, name, nil
}

// mediafireShare lists a folder share through mediafire's own API, which
// answers with the file names, sizes and page links in one request per
// chunk — far cheaper than walking the folder's HTML.
func mediafireShare(ctx context.Context, s *Share, key string, opts Options) (*Result, error) {
	res := &Result{Title: key}
	if info, err := mediafireFolderName(ctx, s, key); err == nil && info != "" {
		res.Title = info
	}
	list := func(ctx context.Context, w *treeWalk, key string, _ int) ([]treeItem[string], error) {
		return mediafireList(ctx, s, w, key)
	}
	if err := walkTree(ctx, opts, res, key, key, list); err != nil {
		return nil, err
	}
	return res, nil
}

// mediafireList reads one folder's files and subfolders for walkTree.
func mediafireList(ctx context.Context, s *Share, w *treeWalk, key string) ([]treeItem[string], error) {
	files, folders, err := mediafireContent(ctx, s, w, key)
	if err != nil {
		return nil, err
	}
	items := make([]treeItem[string], 0, len(files)+len(folders))
	for _, f := range files {
		page := cmp.Or(f.Links.NormalDownload,
			mediafireRoot+"/file/"+url.PathEscape(f.QuickKey))
		name := cmp.Or(f.Filename, util.NameFromURL(page))
		items = append(items, treeItem[string]{File: &File{
			Name:    name,
			Size:    parsedSize(f.Size),
			Headers: httpx.Referer(page),
			// Every file in a folder needs its own page read for a link,
			// so none of them is resolved until it starts.
			Resolve: mediafireTarget(s, page),
		}})
	}
	for _, sub := range folders {
		if sub.FolderKey != "" {
			// A subfolder without a name keeps its files in its parent's
			// directory.
			items = append(items, treeItem[string]{Folder: sub.FolderKey, Key: sub.FolderKey, Name: sub.Name})
		}
	}
	return items, nil
}

// mediafireFile and mediafireFolder are the entries a folder listing holds.
type mediafireFile struct {
	QuickKey string `json:"quickkey"`
	Filename string `json:"filename"`
	// Size arrives as a decimal string rather than a number.
	Size  string `json:"size"`
	Links struct {
		NormalDownload string `json:"normal_download"`
	} `json:"links"`
}

type mediafireFolder struct {
	FolderKey string `json:"folderkey"`
	Name      string `json:"name"`
}

// mediafireContent pages through one folder's files, then its subfolders,
// noting on the walk a listing that stopped short. Once the files outnumber
// the walk's room the rest are not read, nor the subfolders, which come
// after them and could not fit either: the walk says the file limit cut the
// share. The files' first chunk failing is the folder failing; once some
// are read, the subfolders failing leaves them standing, said so.
func mediafireContent(ctx context.Context, s *Share, w *treeWalk, key string) ([]mediafireFile, []mediafireFolder, error) {
	room := w.room()
	files, note, err := walkPages(ctx, pageWalk[mediafireFile]{
		key: func(f mediafireFile) string {
			return cmp.Or(f.QuickKey, f.Links.NormalDownload, f.Filename)
		},
		limit: room + 1,
		fetch: func(ctx context.Context, n int) (pageOf[mediafireFile], error) {
			c, err := mediafireChunkOf(ctx, s, key, "files", n+1)
			return pageOf[mediafireFile]{items: c.Files, next: c.more(), end: !c.more()}, err
		},
	})
	if err != nil {
		return nil, nil, err
	}
	w.partial(note)
	if len(files) > room {
		return files, nil, nil
	}
	folders, note, err := walkPages(ctx, pageWalk[mediafireFolder]{
		key:   func(f mediafireFolder) string { return f.FolderKey },
		limit: -1,
		fetch: func(ctx context.Context, n int) (pageOf[mediafireFolder], error) {
			c, err := mediafireChunkOf(ctx, s, key, "folders", n+1)
			return pageOf[mediafireFolder]{items: c.Folders, next: c.more(), end: !c.more()}, err
		},
	})
	switch {
	case err == nil:
		w.partial(note)
	case ctx.Err() != nil || len(files) == 0:
		return nil, nil, err
	default:
		w.note("unreadable folders")
	}
	return files, folders, nil
}

// mediafireChunk is one chunk of a folder's content.
type mediafireChunk struct {
	Files      []mediafireFile   `json:"files"`
	Folders    []mediafireFolder `json:"folders"`
	MoreChunks string            `json:"more_chunks"`
}

// more says another chunk follows. Its absence is the API saying this chunk
// is the last, so an empty one — a folder of only subfolders, or none — is
// the end, not a page that added nothing.
func (c mediafireChunk) more() bool { return strings.EqualFold(c.MoreChunks, "yes") }

// mediafireChunkOf reads one chunk, counted from 1, of a folder's files or
// subfolders (kind).
func mediafireChunkOf(ctx context.Context, s *Share, key, kind string, chunk int) (mediafireChunk, error) {
	q := url.Values{
		"content_type":    {kind},
		"folder_key":      {key},
		"chunk":           {strconv.Itoa(chunk)},
		"response_format": {"json"},
	}
	var env struct {
		Response struct {
			Result        string         `json:"result"`
			Message       string         `json:"message"`
			FolderContent mediafireChunk `json:"folder_content"`
		} `json:"response"`
	}
	endpoint := s.api + "/folder/get_content.php?" + q.Encode()
	if err := s.client.GetJSON(ctx, endpoint, httpx.Referer(mediafireRoot+"/"), &env); err != nil {
		return mediafireChunk{}, fmt.Errorf("mediafire: list folder: %w", err)
	}
	if env.Response.Result != "" && !strings.EqualFold(env.Response.Result, "Success") {
		return mediafireChunk{}, fmt.Errorf("mediafire: list folder: %s",
			cmp.Or(env.Response.Message, env.Response.Result))
	}
	return env.Response.FolderContent, nil
}

// mediafireFolderName reads a folder's own name, for the job title.
func mediafireFolderName(ctx context.Context, s *Share, key string) (string, error) {
	q := url.Values{"folder_key": {key}, "response_format": {"json"}}
	var env struct {
		Response struct {
			FolderInfo struct {
				Name string `json:"name"`
			} `json:"folder_info"`
		} `json:"response"`
	}
	endpoint := s.api + "/folder/get_info.php?" + q.Encode()
	if err := s.client.GetJSON(ctx, endpoint, httpx.Referer(mediafireRoot+"/"), &env); err != nil {
		return "", err
	}
	return env.Response.FolderInfo.Name, nil
}

// ----------------------------------------------------------- google drive

// GoogleDrive resolves single-file share links.
//
// Folders are deliberately left out: listing one needs an API key, which
// this program has no way to ask for and no business holding.
type GoogleDrive struct {
	about
	client *httpx.Client
}

const (
	driveRoot = "https://drive.google.com"
	// driveDownload is the endpoint that serves the bytes. The web app
	// redirects to it, and going straight there skips a hop.
	driveDownload = "https://drive.usercontent.google.com/download"
	// driveFormID marks the confirmation page google puts in front of
	// anything too large for it to have virus-scanned.
	driveFormID = "download-form"
)

// NewGoogleDrive builds the drive extractor.
func NewGoogleDrive(client *httpx.Client) *GoogleDrive {
	return &GoogleDrive{client: client, about: about{name: "drive", shape: driveShape,
		recognises: "file links (`/file/d/<id>`, `/open?id=`, `/uc?id=`) whose page is the Google Drive file viewer " +
			"(its `apps-fileview` bundle), or the download confirmation form"}}
}

// driveFileShape is a file's viewer, /file/d/<id>[/view]; driveQueryShape
// the endpoints that take the id as a query, /open, /uc and /download.
var (
	driveFileShape  = pathShape(`^/file/d/[A-Za-z0-9_-]{10,}(/[a-z]*)?/?$`)
	driveQueryShape = pathShape(`^/(open|uc|download)/?$`)
)

// driveViewerBundle is the file viewer's script bundle.
const driveViewerBundle = "/_/apps-fileview/"

// driveShape is a file link: its viewer, or an endpoint given its id.
func driveShape(u *url.URL) bool {
	return driveFileShape(u) || driveQueryShape(u) && u.Query().Get("id") != ""
}

// Recognise takes a file link answered by the viewer — known by its bundle
// and its name, since the file itself is fetched from the download endpoint
// rather than read off the page — or by the confirmation form that endpoint
// puts in front of a large file, which is what Extract walks past.
func (g *GoogleDrive) Recognise(_ context.Context, p *Page) bool {
	if !driveShape(p.URL) && !driveShape(p.Final) {
		return false
	}
	if p.Script(driveViewerBundle) && p.Named("Google Drive") {
		return true
	}
	return p.hasElement(func(n *html.Node) bool {
		return isElem(n, atom.Form) && attr(n, "id") == driveFormID
	})
}

// Extract resolves a share link to the download endpoint.
func (g *GoogleDrive) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	id := driveFileID(u)
	if id == "" {
		return nil, fmt.Errorf("drive: no file id in %s "+
			"(folder links are not supported: listing one needs an API key)", u.Redacted())
	}

	res, err := signedResult(ctx, func(ctx context.Context) (*Target, error) {
		link, name, err := g.confirm(ctx, id)
		if err != nil {
			return nil, err
		}
		return &Target{URL: link, Name: name, Size: -1, Headers: httpx.Referer(driveRoot + "/")}, nil
	})
	if err != nil {
		return nil, err
	}
	// The name is usually only in the Content-Disposition the transfer
	// itself sees; an extensionless placeholder lets the server's win.
	if res.Title == "" {
		res.Title = "drive-" + id
		res.Files[0].Name = res.Title
	}
	return res, nil
}

// confirm walks past the interstitial google shows for large files.
//
// This has to happen here rather than being left to the transfer: the
// confirmation page is HTML served with a 200, which the downloader rightly
// refuses to write to disk, and its token is minted per visit.
func (g *GoogleDrive) confirm(ctx context.Context, id string) (link, name string, err error) {
	q := url.Values{"id": {id}, "export": {"download"}, "confirm": {"t"}}
	direct := driveDownload + "?" + q.Encode()

	doc, err := g.client.GetString(ctx, direct, httpx.Referer(driveRoot+"/"))
	if err != nil {
		// Not HTML at all is the good case: the bytes are already there.
		return direct, "", nil
	}
	root, parseErr := parseHTML(doc)
	if parseErr != nil {
		return direct, "", nil
	}

	form := findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.Form) && attr(n, "id") == driveFormID
	})
	if form == nil {
		if msg := driveRefusal(doc); msg != "" {
			return "", "", fmt.Errorf("drive: %s", msg)
		}
		return direct, driveName(root), nil
	}

	action := cmp.Or(attr(form, "action"), driveDownload)
	values := url.Values{}
	for _, input := range findAll(form, func(n *html.Node) bool { return isElem(n, atom.Input) }) {
		if key := attr(input, "name"); key != "" {
			values.Set(key, attr(input, "value"))
		}
	}
	values.Set("confirm", cmp.Or(values.Get("confirm"), "t"))
	if values.Get("id") == "" {
		values.Set("id", id)
	}
	return action + "?" + values.Encode(), driveName(root), nil
}

// driveName reads the filename off the confirmation page, which states it
// above the button.
func driveName(root *html.Node) string {
	span := findFirst(root, func(n *html.Node) bool {
		return hasClass(n, "uc-name-size")
	})
	if span == nil {
		return ""
	}
	if link := findFirst(span, func(n *html.Node) bool { return isElem(n, atom.A) }); link != nil {
		return strings.TrimSpace(textOf(link))
	}
	return ""
}

// driveRefusal recognises the two refusals drive words as a page rather than
// as a status, so they are reported instead of retried.
func driveRefusal(doc string) string {
	switch {
	case strings.Contains(doc, "Too many users have viewed or downloaded"):
		return "this file has hit its download quota; it becomes available again after a day or so"
	case strings.Contains(doc, "You need access"), strings.Contains(doc, "Request access"):
		return "this file is not shared publicly"
	}
	return ""
}

// driveFileID pulls the file id out of any of the link shapes drive uses.
func driveFileID(u *url.URL) string {
	if id := u.Query().Get("id"); id != "" {
		return id
	}
	segs := util.PathSegments(u)
	for i, seg := range segs {
		// /file/d/<id>/view and /d/<id>
		if seg == "d" && i+1 < len(segs) {
			return segs[i+1]
		}
	}
	return ""
}
