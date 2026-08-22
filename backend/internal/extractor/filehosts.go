package extractor

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
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

// Dropbox turns a share link into the download it stands for.
type Dropbox struct{ client *httpx.Client }

const (
	dropboxRoot = "https://www.dropbox.com"
	// dropboxContentHost serves the same paths as the site itself, and is
	// where a dl=1 link ends up anyway. Kept as a mirror so a transfer that
	// the front end refuses gets a second, different answer.
	dropboxContentHost = "dl.dropboxusercontent.com"
)

// NewDropbox builds the dropbox extractor.
func NewDropbox(client *httpx.Client) *Dropbox { return &Dropbox{client: client} }

func (d *Dropbox) Name() string { return "dropbox" }

func (d *Dropbox) Match(u *url.URL) bool {
	return util.HostMatches(u.Host, "dropbox.com") || util.HostMatches(u.Host, "dropboxusercontent.com")
}

// Extract rewrites the link to ask for the bytes rather than the viewer.
//
// A folder link is answered with a zip of the whole folder, which is what
// dropbox itself offers and the only thing a single request can produce. Its
// name is left to the server, whose Content-Disposition names the folder;
// the path carries only an opaque id.
func (d *Dropbox) Extract(_ context.Context, u *url.URL, _ Options) (*Result, error) {
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

// Mediafire resolves file pages and folder shares.
type Mediafire struct{ client *httpx.Client }

const (
	mediafireRoot = "https://www.mediafire.com"
	mediafireAPI  = "https://www.mediafire.com/api/1.5"

	// mediafireButtonID marks the anchor holding the real link.
	mediafireButtonID = "downloadButton"
	// mediafireScrambled is where that link hides when the page declines to
	// put it in the href, base64 encoded.
	mediafireScrambled = "data-scrambled-url"
)

// NewMediafire builds the mediafire extractor.
func NewMediafire(client *httpx.Client) *Mediafire { return &Mediafire{client: client} }

func (m *Mediafire) Name() string { return "mediafire" }

func (m *Mediafire) Match(u *url.URL) bool { return util.HostMatches(u.Host, "mediafire.com") }

// Extract resolves either a single file page or a folder share.
func (m *Mediafire) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) > 1 && segs[0] == "folder" {
		return m.extractFolder(ctx, segs[1])
	}

	page := u.String()
	link, name, err := m.filePage(ctx, page)
	if err != nil {
		return nil, err
	}
	return &Result{Title: name, Files: []File{{
		Name:    name,
		URL:     link,
		Size:    -1,
		Headers: httpx.Referer(page),
		Resolve: m.resolver(page, name),
	}}}, nil
}

// resolver re-reads the file page at download time. The link on it is signed
// per visit, so one taken while an item waited in the queue would be spent.
func (m *Mediafire) resolver(page, name string) func(context.Context) (*Target, error) {
	return func(ctx context.Context) (*Target, error) {
		link, fresh, err := m.filePage(ctx, page)
		if err != nil {
			return nil, err
		}
		return &Target{URL: link, Name: util.FirstNonEmpty(fresh, name), Size: -1,
			Headers: httpx.Referer(page)}, nil
	}
}

// filePage reads one file page for its download link and filename.
func (m *Mediafire) filePage(ctx context.Context, page string) (link, name string, err error) {
	doc, err := m.client.GetString(ctx, page, httpx.Referer(mediafireRoot+"/"))
	if err != nil {
		return "", "", fmt.Errorf("mediafire: fetch %s: %w", page, err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return "", "", fmt.Errorf("mediafire: parse %s: %w", page, err)
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

	name = util.FirstNonEmpty(
		strings.TrimSpace(metaContent(root, "og:title")),
		util.NameFromURL(link),
		util.NameFromURL(page),
	)
	return link, name, nil
}

// extractFolder lists a folder share through mediafire's own API, which
// answers with the file names, sizes and page links in one request per
// chunk — far cheaper than walking the folder's HTML.
func (m *Mediafire) extractFolder(ctx context.Context, key string) (*Result, error) {
	res := &Result{Title: key}
	if info, err := m.folderInfo(ctx, key); err == nil && info != "" {
		res.Title = info
	}
	if err := m.collectFolder(ctx, key, "", 0, res); err != nil {
		return nil, err
	}
	return res, nil
}

// collectFolder appends one folder's files to res and recurses into its
// subfolders.
func (m *Mediafire) collectFolder(ctx context.Context, key, dir string, depth int, res *Result) error {
	files, folders, err := m.folderContent(ctx, key)
	if err != nil {
		return err
	}
	for _, f := range files {
		page := util.FirstNonEmpty(f.Links.NormalDownload,
			mediafireRoot+"/file/"+url.PathEscape(f.QuickKey))
		size, convErr := strconv.ParseInt(f.Size, 10, 64)
		if convErr != nil {
			size = -1
		}
		name := util.FirstNonEmpty(f.Filename, util.NameFromURL(page))
		res.Files = append(res.Files, File{
			Name:    name,
			Dir:     dir,
			Size:    size,
			Headers: httpx.Referer(page),
			// Every file in a folder needs its own page read for a link,
			// so none of them is resolved until it starts.
			Resolve: m.resolver(page, name),
		})
	}
	if depth >= config.MaxFolderDepth {
		return nil
	}
	for _, sub := range folders {
		if sub.FolderKey == "" {
			continue
		}
		next := dir
		if sub.Name != "" {
			next = strings.TrimPrefix(dir+"/"+sub.Name, "/")
		}
		// One unreadable subfolder should not sink the whole job.
		_ = m.collectFolder(ctx, sub.FolderKey, next, depth+1, res)
	}
	return nil
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

// folderContent pages through one folder's files and subfolders.
func (m *Mediafire) folderContent(ctx context.Context, key string) ([]mediafireFile, []mediafireFolder, error) {
	var (
		files   []mediafireFile
		folders []mediafireFolder
	)
	for _, kind := range []string{"files", "folders"} {
		for chunk := 1; chunk <= config.MaxAlbumPages; chunk++ {
			q := url.Values{
				"content_type":    {kind},
				"folder_key":      {key},
				"chunk":           {strconv.Itoa(chunk)},
				"response_format": {"json"},
			}
			var env struct {
				Response struct {
					Result        string `json:"result"`
					Message       string `json:"message"`
					FolderContent struct {
						Files      []mediafireFile   `json:"files"`
						Folders    []mediafireFolder `json:"folders"`
						MoreChunks string            `json:"more_chunks"`
					} `json:"folder_content"`
				} `json:"response"`
			}
			endpoint := mediafireAPI + "/folder/get_content.php?" + q.Encode()
			if err := m.client.GetJSON(ctx, endpoint, httpx.Referer(mediafireRoot+"/"), &env); err != nil {
				return nil, nil, fmt.Errorf("mediafire: list folder: %w", err)
			}
			if env.Response.Result != "" && !strings.EqualFold(env.Response.Result, "Success") {
				return nil, nil, fmt.Errorf("mediafire: list folder: %s",
					util.FirstNonEmpty(env.Response.Message, env.Response.Result))
			}
			files = append(files, env.Response.FolderContent.Files...)
			folders = append(folders, env.Response.FolderContent.Folders...)
			if !strings.EqualFold(env.Response.FolderContent.MoreChunks, "yes") {
				break
			}
		}
	}
	return files, folders, nil
}

// folderInfo reads a folder's own name, for the job title.
func (m *Mediafire) folderInfo(ctx context.Context, key string) (string, error) {
	q := url.Values{"folder_key": {key}, "response_format": {"json"}}
	var env struct {
		Response struct {
			FolderInfo struct {
				Name string `json:"name"`
			} `json:"folder_info"`
		} `json:"response"`
	}
	endpoint := mediafireAPI + "/folder/get_info.php?" + q.Encode()
	if err := m.client.GetJSON(ctx, endpoint, httpx.Referer(mediafireRoot+"/"), &env); err != nil {
		return "", err
	}
	return env.Response.FolderInfo.Name, nil
}

// ----------------------------------------------------------- google drive

// GoogleDrive resolves single-file share links.
//
// Folders are deliberately left out: listing one needs an API key, which
// this program has no way to ask for and no business holding.
type GoogleDrive struct{ client *httpx.Client }

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
func NewGoogleDrive(client *httpx.Client) *GoogleDrive { return &GoogleDrive{client: client} }

func (g *GoogleDrive) Name() string { return "drive" }

func (g *GoogleDrive) Match(u *url.URL) bool {
	if util.HostMatches(u.Host, "drive.google.com") || util.HostMatches(u.Host, "drive.usercontent.google.com") {
		return true
	}
	// docs.google.com serves the same /uc endpoint for drive files.
	return util.HostMatches(u.Host, "docs.google.com") && strings.Contains(u.Path, "/uc")
}

// Extract resolves a share link to the download endpoint.
func (g *GoogleDrive) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	id := driveFileID(u)
	if id == "" {
		return nil, fmt.Errorf("drive: no file id in %s "+
			"(folder links are not supported: listing one needs an API key)", u.Redacted())
	}

	link, name, err := g.confirm(ctx, id)
	if err != nil {
		return nil, err
	}
	// The name is usually only in the Content-Disposition the transfer
	// itself sees; an extensionless placeholder lets the server's win.
	title := util.FirstNonEmpty(name, "drive-"+id)
	return &Result{Title: title, Files: []File{{
		Name:    title,
		URL:     link,
		Size:    -1,
		Headers: httpx.Referer(driveRoot + "/"),
		Resolve: func(ctx context.Context) (*Target, error) {
			fresh, freshName, err := g.confirm(ctx, id)
			if err != nil {
				return nil, err
			}
			return &Target{URL: fresh, Name: util.FirstNonEmpty(freshName, title), Size: -1,
				Headers: httpx.Referer(driveRoot + "/")}, nil
		},
	}}}, nil
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

	action := util.FirstNonEmpty(attr(form, "action"), driveDownload)
	values := url.Values{}
	for _, input := range findAll(form, func(n *html.Node) bool { return isElem(n, atom.Input) }) {
		if key := attr(input, "name"); key != "" {
			values.Set(key, attr(input, "value"))
		}
	}
	values.Set("confirm", util.FirstNonEmpty(values.Get("confirm"), "t"))
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
