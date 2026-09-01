package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Mega resolves mega.nz links: a single file, a whole folder, or one file
// selected inside a folder.
//
// Two things make this host unlike the others here.
//
// The first is that nothing about a link is legible to the server. Names,
// sizes and bytes are all encrypted under the key in the URL fragment, which
// is never sent anywhere — see megacrypt.go. A link quoted without its
// fragment is unrecoverable, and is reported as such rather than as a
// failure to find the file.
//
// The second is that the payload arrives encrypted, so File.Cipher is set
// and the downloader decrypts on the way to disk. Because the mode is CTR
// that costs nothing else: ranges, resume and parallel connections all work
// as they do for a plain file.
//
// Download links are minted per request and tied to one storage node, so
// they are resolved at download time like bunkr's and turbo's rather than at
// extraction time.
type Mega struct {
	client *httpx.Client
	// seq numbers API requests. Mega uses it to collapse a retried request
	// with the one before it instead of doing the work twice.
	seq atomic.Int64
}

const (
	// megaAPI is the single endpoint every command goes to.
	megaAPI  = "https://g.api.mega.co.nz/cs"
	megaRoot = "https://mega.nz"
)

// NewMega builds the mega extractor.
func NewMega(client *httpx.Client) *Mega { return &Mega{client: client} }

func (m *Mega) Name() string { return "mega" }

func (m *Mega) Match(u *url.URL) bool {
	return util.HostMatches(u.Host, "mega.nz") || util.HostMatches(u.Host, "mega.co.nz")
}

// Extract resolves a public link into its files.
func (m *Mega) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	link, err := parseMegaLink(u)
	if err != nil {
		return nil, err
	}
	if link.folder {
		return m.extractFolder(ctx, link)
	}
	return m.extractFile(ctx, link)
}

// ------------------------------------------------------------------ links

// megaLink is a parsed public link.
type megaLink struct {
	// folder distinguishes a share of many nodes from a single file.
	folder bool
	// handle identifies the share.
	handle string
	// key decrypts it: 32 bytes for a file, 16 for a folder.
	key []byte
	// node names one file inside a folder share, when the link selects one.
	node string
}

// parseMegaLink reads both link shapes mega has used.
//
// The modern one puts the handle in the path and the key in the fragment,
// which is what keeps the key out of every request the browser makes. The
// older one put all of it in the fragment. Both are still handed around, so
// both are accepted.
func parseMegaLink(u *url.URL) (*megaLink, error) {
	segs := util.PathSegments(u)
	frag := strings.TrimSpace(u.Fragment)

	// /file/<handle>#<key>, /embed/<handle>#<key>,
	// /folder/<handle>#<key>[/file/<node>]
	if len(segs) >= 2 {
		switch segs[0] {
		case "file", "embed":
			return newMegaLink(false, segs[1], frag, "")
		case "folder":
			key, node := splitMegaFolderFragment(frag)
			return newMegaLink(true, segs[1], key, node)
		}
	}

	// #!<handle>!<key> and #F!<handle>!<key>[!<node>]
	if frag != "" {
		folder := false
		if rest, cut := strings.CutPrefix(frag, "F"); cut {
			folder, frag = true, rest
		}
		parts := strings.Split(strings.TrimPrefix(frag, "!"), "!")
		if len(parts) >= 2 {
			node := ""
			if folder && len(parts) >= 3 {
				node = parts[2]
			}
			return newMegaLink(folder, parts[0], parts[1], node)
		}
	}

	return nil, fmt.Errorf("mega: %s is not a file or folder link", u.Redacted())
}

// splitMegaFolderFragment separates a folder key from the node a link may
// select after it: "<key>/file/<node>".
//
// Only a file selection is honoured. The same shape with "folder" selects a
// subtree, which there is no way to name to the API separately — taking it
// for a file handle would find nothing and report the folder as empty,
// where ignoring it downloads the whole share.
func splitMegaFolderFragment(frag string) (key, node string) {
	key, rest, ok := strings.Cut(frag, "/")
	if !ok {
		return key, ""
	}
	if kind, handle, ok := strings.Cut(rest, "/"); ok && kind == "file" {
		return key, handle
	}
	return key, ""
}

// newMegaLink validates the parts and decodes the key.
func newMegaLink(folder bool, handle, key, node string) (*megaLink, error) {
	if handle == "" {
		return nil, fmt.Errorf("mega: link carries no content id")
	}
	if key == "" {
		return nil, fmt.Errorf("mega: link carries no key — mega encrypts everything, " +
			"and the part after the # is what decrypts it, so a link quoted without it cannot be opened")
	}
	raw, err := megaB64(key)
	if err != nil {
		return nil, fmt.Errorf("mega: link key is malformed: %w", err)
	}
	want := megaFileKeyLen
	if folder {
		want = megaFolderKeyLen
	}
	if len(raw) != want {
		return nil, fmt.Errorf("mega: link key decodes to %d bytes, expected %d", len(raw), want)
	}
	return &megaLink{folder: folder, handle: handle, key: raw, node: node}, nil
}

// ------------------------------------------------------------------- file

// extractFile resolves a single-file link.
func (m *Mega) extractFile(ctx context.Context, link *megaLink) (*Result, error) {
	key, nonce, _, err := megaFileKey(link.key)
	if err != nil {
		return nil, fmt.Errorf("mega: %w", err)
	}

	dl, err := m.link(ctx, "", megaCommand{A: "g", G: 1, SSL: 2, P: link.handle})
	if err != nil {
		return nil, err
	}
	attrs, err := megaDecryptAttrs(string(dl.Attrs), key)
	if err != nil {
		return nil, fmt.Errorf("mega: %w", err)
	}
	name := util.FirstNonEmpty(attrs.Name, link.handle)

	file := File{
		Name:    name,
		URL:     dl.URL,
		Size:    dl.Size,
		Cipher:  &StreamCipher{Key: key, Nonce: nonce},
		Resolve: m.resolver("", megaCommand{A: "g", G: 1, SSL: 2, P: link.handle}, name),
	}
	return &Result{Title: name, Files: []File{file}}, nil
}

// ----------------------------------------------------------------- folder

// extractFolder lists a folder share and turns every file in it into an item.
func (m *Mega) extractFolder(ctx context.Context, link *megaLink) (*Result, error) {
	var listing struct {
		Nodes []megaNode `json:"f"`
	}
	// c:1 asks for the whole tree, r:1 for it recursively.
	if err := m.api(ctx, link.handle, megaCommand{A: "f", C: 1, R: 1}, &listing); err != nil {
		return nil, err
	}
	if len(listing.Nodes) == 0 {
		return nil, fmt.Errorf("mega: folder is empty")
	}

	nodes := make(map[string]*megaDecrypted, len(listing.Nodes))
	order := make([]*megaDecrypted, 0, len(listing.Nodes))
	for i := range listing.Nodes {
		node := &listing.Nodes[i]
		dec := &megaDecrypted{node: node}
		dec.decrypt(link.key)
		nodes[node.Handle] = dec
		order = append(order, dec)
	}

	res := &Result{Title: megaFolderTitle(nodes, order)}
	for _, dec := range order {
		if dec.node.Type != megaTypeFile || dec.key == nil {
			continue
		}
		if link.node != "" && dec.node.Handle != link.node {
			continue
		}
		key, nonce, _, err := megaFileKey(dec.key)
		if err != nil {
			continue // a node whose key is the wrong shape is not ours to read
		}
		name := util.FirstNonEmpty(dec.name, dec.node.Handle)
		cmd := megaCommand{A: "g", G: 1, SSL: 2, N: dec.node.Handle}
		res.Files = append(res.Files, File{
			Name:    name,
			Dir:     megaDir(nodes, dec),
			Size:    dec.node.Size,
			Cipher:  &StreamCipher{Key: key, Nonce: nonce},
			Resolve: m.resolver(link.handle, cmd, name),
		})
	}
	if len(res.Files) == 0 {
		if link.node != "" {
			return nil, fmt.Errorf("mega: the selected file is not in this folder")
		}
		return nil, fmt.Errorf("mega: folder holds no readable files")
	}
	return res, nil
}

// Node types as mega numbers them.
const (
	megaTypeFile   = 0
	megaTypeFolder = 1
	megaTypeRoot   = 2
)

// megaNode is one entry of a folder listing.
type megaNode struct {
	Handle string    `json:"h"`
	Parent string    `json:"p"`
	Type   int       `json:"t"`
	Attrs  megaField `json:"a"`
	Key    megaField `json:"k"`
	Size   int64     `json:"s"`
}

// megaDecrypted pairs a node with what its key opened.
type megaDecrypted struct {
	node *megaNode
	key  []byte
	name string
}

// decrypt unwraps the node's key with the share key and reads its name.
//
// A node lists its key once per share it is reachable through, as
// "<share handle>:<wrapped key>" joined by slashes. Rather than working out
// which entry belongs to this share, every candidate is tried and the one
// whose attributes carry mega's magic prefix is the right one — the same
// check that catches a wrong key is exactly the test needed here.
func (d *megaDecrypted) decrypt(share []byte) {
	for entry := range strings.SplitSeq(string(d.node.Key), "/") {
		_, wrapped, ok := strings.Cut(entry, ":")
		if !ok {
			wrapped = entry
		}
		raw, err := megaB64(wrapped)
		if err != nil {
			continue
		}
		key, err := megaUnwrapKey(raw, share)
		if err != nil {
			continue
		}
		// A file's key folds down to the AES key; a folder's is already it.
		attrKey := key
		if len(key) == megaFileKeyLen {
			if attrKey, _, _, err = megaFileKey(key); err != nil {
				continue
			}
		}
		attrs, err := megaDecryptAttrs(string(d.node.Attrs), attrKey)
		if err != nil {
			continue
		}
		d.key, d.name = key, attrs.Name
		return
	}
}

// megaDir builds the relative directory a file sits in, walking up towards
// the share root.
//
// The root itself is left out, because the manager already files a
// multi-file job under the job title and that is the same name. It is
// recognised the same way megaFolderTitle recognises it — as the node whose
// own parent is not in the listing — so the two cannot disagree and produce
// the folder name twice. The depth bound doubles as a guard against a
// listing whose parents form a cycle.
func megaDir(nodes map[string]*megaDecrypted, file *megaDecrypted) string {
	var parts []string
	for handle := file.node.Parent; len(parts) < config.MaxFolderDepth; {
		parent := nodes[handle]
		if parent == nil || parent.node.Type == megaTypeRoot || nodes[parent.node.Parent] == nil {
			break // the share root, or a parent outside the listing
		}
		if parent.name == "" {
			break
		}
		parts = append([]string{parent.name}, parts...)
		handle = parent.node.Parent
	}
	return path.Join(parts...)
}

// megaFolderTitle names the job after the shared folder itself, which is the
// node no other node in the listing is the parent of.
func megaFolderTitle(nodes map[string]*megaDecrypted, order []*megaDecrypted) string {
	for _, dec := range order {
		if dec.node.Type == megaTypeFile {
			continue
		}
		if dec.node.Parent == "" || nodes[dec.node.Parent] == nil {
			if dec.name != "" {
				return dec.name
			}
		}
	}
	return "mega"
}

// -------------------------------------------------------------------- api

// megaCommand is one API command. Mega names every field with a single
// letter; the tags are the wire format and the names here say what they do.
type megaCommand struct {
	// A is the action: "g" for a download link, "f" for a folder listing.
	A string `json:"a"`
	// G asks the download command for a link rather than only metadata.
	G int `json:"g,omitempty"`
	// SSL asks for an https storage link.
	SSL int `json:"ssl,omitempty"`
	// P names a public file by its share handle.
	P string `json:"p,omitempty"`
	// N names a node inside a folder share.
	N string `json:"n,omitempty"`
	// C and R ask a listing for the whole tree, recursively.
	C int `json:"c,omitempty"`
	R int `json:"r,omitempty"`
}

// megaDownloadLink is the answer to a download command.
type megaDownloadLink struct {
	URL   string    `json:"g"`
	Size  int64     `json:"s"`
	Attrs megaField `json:"at"`
	Err   int       `json:"e"`
}

// link requests a fresh storage link.
func (m *Mega) link(ctx context.Context, folder string, cmd megaCommand) (*megaDownloadLink, error) {
	var dl megaDownloadLink
	if err := m.api(ctx, folder, cmd, &dl); err != nil {
		return nil, err
	}
	if dl.Err != 0 {
		return nil, fmt.Errorf("mega: %w", megaAPIError(dl.Err))
	}
	if dl.URL == "" {
		return nil, fmt.Errorf("mega: the api returned no download link")
	}
	// Storage nodes answer on both schemes and the API occasionally hands
	// back the plain one regardless of what was asked for.
	dl.URL = strings.Replace(dl.URL, "http://", "https://", 1)
	return &dl, nil
}

// resolver defers minting a storage link until the item actually starts.
// Mega's links are tied to one storage node and are handed out per request,
// so one signed while an item waited in a long queue would be spent by the
// time its turn came.
func (m *Mega) resolver(folder string, cmd megaCommand, name string) func(context.Context) (*Target, error) {
	return func(ctx context.Context) (*Target, error) {
		dl, err := m.link(ctx, folder, cmd)
		if err != nil {
			return nil, err
		}
		return &Target{URL: dl.URL, Size: dl.Size, Name: name}, nil
	}
}

// api sends one command and decodes the single result mega answers with.
func (m *Mega) api(ctx context.Context, folder string, cmd megaCommand, out any) error {
	endpoint := megaAPI + "?id=" + strconv.FormatInt(m.seq.Add(1), 10)
	if folder != "" {
		endpoint += "&n=" + url.QueryEscape(folder)
	}

	var raw json.RawMessage
	err := m.client.PostJSON(ctx, endpoint,
		httpx.RefererOrigin(megaRoot+"/", megaRoot), []megaCommand{cmd}, &raw)
	if err != nil {
		return fmt.Errorf("mega: api request: %w", err)
	}

	// A failure replaces the batch itself, or the one result in it, with a
	// bare negative number.
	if code, bad := megaErrorCode(raw); bad {
		return fmt.Errorf("mega: %w", megaAPIError(code))
	}
	var results []json.RawMessage
	if err := json.Unmarshal(raw, &results); err != nil || len(results) == 0 {
		return fmt.Errorf("mega: unexpected api response: %s", util.Truncate(string(raw), 120))
	}
	if code, bad := megaErrorCode(results[0]); bad {
		return fmt.Errorf("mega: %w", megaAPIError(code))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(results[0], out); err != nil {
		return fmt.Errorf("mega: decode api response: %w", err)
	}
	return nil
}

// megaErrorCode reports a bare negative number, which is how mega signals a
// failure where a result would otherwise be.
func megaErrorCode(raw json.RawMessage) (int, bool) {
	var code int
	if err := json.Unmarshal(raw, &code); err != nil {
		return 0, false
	}
	return code, code < 0
}

// megaAPIError is one of mega's numeric status codes.
type megaAPIError int

func (e megaAPIError) Error() string {
	switch e {
	case -2:
		return "the api rejected the request as malformed"
	case -3, -4:
		return "the api is rate limiting or temporarily overloaded"
	case -6, -11:
		return "access denied for this link"
	case -8:
		return "this link has expired"
	case -9:
		return "content not found (deleted, or the link is mistyped)"
	case -14:
		return "the key in the link does not decrypt this content"
	case -15:
		return "the session expired"
	case -16:
		return "the account that shared this has been suspended"
	case -17:
		return "download quota exceeded — mega limits how much one address may " +
			"pull in a period, and the wait is measured in hours"
	case -18:
		return "the content is temporarily unavailable"
	}
	return fmt.Sprintf("the api returned error %d", int(e))
}

// megaField is a string field mega does not always send as a string: an
// entry it cannot describe arrives as a number instead. Anything that is not
// a string reads as empty, which the callers already treat as unreadable.
type megaField string

func (f *megaField) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = megaField(s)
	}
	return nil
}
