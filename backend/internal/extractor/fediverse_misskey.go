package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// The Misskey dialect of the fediverse extractor — the POST-only API the
// whole Misskey family shares. Split from fediverse.go; the shared discovery
// and URL shapes stay there.

// ------------------------------------------------------------------- misskey

// misskeyUser is the part of a user this needs.
type misskeyUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Host     string `json:"host"`
}

// misskeyNote is one post.
type misskeyNote struct {
	ID    string        `json:"id"`
	Text  string        `json:"text"`
	User  misskeyUser   `json:"user"`
	Files []misskeyFile `json:"files"`
}

// misskeyFile is one attachment.
//
// Unlike the Mastodon family this states an exact byte length — but it states
// the *original upload's*, and url does not always serve that file. See
// misskeyFiles. thumbnailUrl is absent for the usual reason: it is a
// thumbnail.
type misskeyFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

// misskeyWebPublic prefixes the filename of an instance's re-encoded copy.
const misskeyWebPublic = "webpublic-"

// misskey resolves a user's notes, or a single note. Every call is a POST
// carrying JSON, including the ones that only read.
func (f *Fediverse) misskey(ctx context.Context, u *url.URL, t *fediTarget) (*Result, error) {
	if t.kind == fediCommunity {
		return nil, fmt.Errorf("fediverse: communities are a lemmy thing, and %s does not run "+
			"it (%s)", u.Hostname(), u.Redacted())
	}
	root := util.Origin(u)

	if t.kind == fediPost {
		var note misskeyNote
		body := struct {
			NoteID string `json:"noteId"`
		}{NoteID: t.id}
		if err := f.client.PostJSON(ctx, root+"/api/notes/show", fediHeaders(root), body, &note); err != nil {
			return nil, fmt.Errorf("fediverse: read note %s: %w", t.id, err)
		}
		files := misskeyFiles([]misskeyNote{note})
		if len(files) == 0 {
			return nil, fmt.Errorf("fediverse: note %s carries no files", t.id)
		}
		return &Result{Title: misskeyNoteTitle(&note, u.Hostname()), Files: files}, nil
	}

	user, err := f.misskeyLookup(ctx, root, t.handle)
	if err != nil {
		return nil, err
	}
	title := "@" + fediHandle(t.handle, u.Hostname())

	notes, err := f.misskeyTimeline(ctx, root, user.ID)
	if err != nil {
		return nil, err
	}
	files := misskeyFiles(notes)
	if len(files) == 0 {
		return nil, fmt.Errorf("fediverse: %s has attached no files to its most recent %d notes",
			title, config.MaxTimelinePages*fediMisskeyPageSize)
	}
	return &Result{Title: title, Files: files}, nil
}

// misskeyLookup turns a handle into the user, and above all into their id.
//
// host is null for a local account and the domain for one this instance is
// merely showing, and it has to be *sent* either way: it is how the API tells
// "the alice here" from "the alice over there".
func (f *Fediverse) misskeyLookup(ctx context.Context, root, handle string) (*misskeyUser, error) {
	name, host, _ := strings.Cut(handle, "@")
	if name == "" {
		return nil, fmt.Errorf("fediverse: the link names no account")
	}
	body := struct {
		Username string  `json:"username"`
		Host     *string `json:"host"`
	}{Username: name}
	if host != "" {
		body.Host = &host
	}

	var user misskeyUser
	if err := f.client.PostJSON(ctx, root+"/api/users/show", fediHeaders(root), body, &user); err != nil {
		return nil, fmt.Errorf("fediverse: no account %q on this instance: %w", handle, err)
	}
	if user.ID == "" {
		return nil, fmt.Errorf("fediverse: no account %q on this instance", handle)
	}
	return &user, nil
}

// misskeyTimeline pages back through a user's notes that carry files.
//
// untilId is this API's max_id: exclusive, newest first, each page asking for
// what precedes the last id of the one before. The same three stopping
// conditions apply as to a Mastodon timeline — a short page, a page that
// makes no progress, and the cap.
func (f *Fediverse) misskeyTimeline(ctx context.Context, root, userID string) ([]misskeyNote, error) {
	var all []misskeyNote
	until := ""

	for page := 0; page < config.MaxTimelinePages; page++ {
		body := struct {
			UserID    string `json:"userId"`
			Limit     int    `json:"limit"`
			WithFiles bool   `json:"withFiles"`
			UntilID   string `json:"untilId,omitempty"`
		}{UserID: userID, Limit: fediMisskeyPageSize, WithFiles: true, UntilID: until}

		var batch []misskeyNote
		if err := f.client.PostJSON(ctx, root+"/api/users/notes", fediHeaders(root), body, &batch); err != nil {
			if page == 0 {
				return nil, fmt.Errorf("fediverse: read the account's notes: %w", err)
			}
			break
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)

		next := batch[len(batch)-1].ID
		if next == "" || next == until || len(batch) < fediMisskeyPageSize {
			break
		}
		until = next
	}
	return all, nil
}

// misskeyFiles flattens notes into downloadable attachments.
//
// A renote needs none of the care a Mastodon boost does: a note's files are
// always its own, and a plain renote carries none, so asking for notes with
// files never returns somebody else's.
//
// The size is where care is needed. It is exact — but it describes the file
// as uploaded, and url does not always serve that file: an instance that
// re-encodes images for the web publishes the copy under a "webpublic-" name,
// and a measured example was 4.8 MB by this field and 1.1 MB on the wire. The
// original is not addressable, its object name being a different UUID
// entirely, so the copy is what gets fetched and the size is dropped rather
// than reported. A length describing a different file is worse than none:
// this one is exact enough to be believed, and the downloader would use it to
// conclude that a file already on disk was this one.
func misskeyFiles(notes []misskeyNote) []File {
	seen := make(map[string]bool)
	var files []File

	for _, note := range notes {
		for _, file := range note.Files {
			if file.URL == "" || seen[file.URL] {
				continue
			}
			seen[file.URL] = true

			size := file.Size
			if size <= 0 || misskeyIsDerived(file.URL) {
				size = -1
			}
			files = append(files, File{
				Name: misskeyName(&file),
				URL:  file.URL,
				Size: size,
			})
		}
	}
	return files
}

// misskeyIsDerived reports whether a URL serves the instance's own re-encoded
// copy rather than what was uploaded.
func misskeyIsDerived(link string) bool {
	return strings.HasPrefix(util.NameFromURL(link), misskeyWebPublic)
}

// misskeyName keeps the uploader's filename but corrects its extension to
// what the URL actually serves, so a re-encoded copy is not saved under the
// format it used to be.
func misskeyName(file *misskeyFile) string {
	name := util.FirstNonEmpty(file.Name, util.NameFromURL(file.URL), file.ID)
	served := fediExt(file.URL)
	if served != "" && !strings.EqualFold(path.Ext(name), served) {
		name = strings.TrimSuffix(name, path.Ext(name)) + served
	}
	return name
}

// misskeyNoteTitle names a single-note job after what was written, falling
// back to the handle and id for a note that is media and nothing else.
func misskeyNoteTitle(note *misskeyNote, host string) string {
	if text := folderName(note.Text); text != "" {
		return text
	}
	// The user's own host is set only for an account this instance is
	// showing on another's behalf; for a local one it is null.
	author := note.User.Username
	if note.User.Host != "" {
		author += "@" + note.User.Host
	}
	return "@" + fediHandle(author, host) + " " + note.ID
}
