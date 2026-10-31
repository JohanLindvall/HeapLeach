// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// The Misskey dialect of the fediverse extractor — the POST-only API the
// whole Misskey family shares. The shared discovery, URL shapes and timeline
// engine stay in fediverse.go.

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

// fediMisskey is the Misskey family's API, which is POST-only: every call
// carries JSON, including the ones that only read.
var fediMisskey = fediDialect{
	stride: fediMisskeyPageSize,
	post:   misskeySingle, owner: misskeyOwner, page: misskeyTimeline,
	empty: "%s has attached no files to its most recent %d notes",
}

// misskeySingle reads a single note.
func misskeySingle(ctx context.Context, s *fediSession) (*Result, error) {
	var note misskeyNote
	body := struct {
		NoteID string `json:"noteId"`
	}{NoteID: s.t.id}
	if err := s.client.PostJSON(ctx, s.root+"/api/notes/show", fediHeaders(s.root), body, &note); err != nil {
		return nil, fmt.Errorf("fediverse: read note %s: %w", s.t.id, err)
	}
	files := fediFiles(misskeyEntries([]misskeyNote{note}))
	if len(files) == 0 {
		return nil, fmt.Errorf("fediverse: note %s carries no files", s.t.id)
	}
	return &Result{Title: misskeyNoteTitle(&note, s.host()), Files: files}, nil
}

// misskeyOwner turns a handle into the user, and above all into their id.
//
// host is null for a local account and the domain for one this instance is
// merely showing, and it has to be *sent* either way: it is how the API tells
// "the alice here" from "the alice over there".
func misskeyOwner(ctx context.Context, s *fediSession) (string, string, error) {
	handle := s.t.handle
	name, host, _ := strings.Cut(handle, "@")
	if name == "" {
		return "", "", fmt.Errorf("fediverse: the link names no account")
	}
	body := struct {
		Username string  `json:"username"`
		Host     *string `json:"host"`
	}{Username: name}
	if host != "" {
		body.Host = &host
	}

	var user misskeyUser
	if err := s.client.PostJSON(ctx, s.root+"/api/users/show", fediHeaders(s.root), body, &user); err != nil {
		return "", "", fmt.Errorf("fediverse: no account %q on this instance: %w", handle, err)
	}
	if user.ID == "" {
		return "", "", fmt.Errorf("fediverse: no account %q on this instance", handle)
	}
	return user.ID, "@" + fediHandle(handle, s.host()), nil
}

// misskeyTimeline reads one page of a user's notes that carry files. untilId is
// this API's max_id.
func misskeyTimeline(ctx context.Context, s *fediSession, until string, _ int) ([]fediEntry, error) {
	body := struct {
		UserID    string `json:"userId"`
		Limit     int    `json:"limit"`
		WithFiles bool   `json:"withFiles"`
		UntilID   string `json:"untilId,omitempty"`
	}{UserID: s.owner, Limit: fediMisskeyPageSize, WithFiles: true, UntilID: until}

	var batch []misskeyNote
	if err := s.client.PostJSON(ctx, s.root+"/api/users/notes", fediHeaders(s.root), body, &batch); err != nil {
		return nil, fmt.Errorf("fediverse: read the account's notes: %w", err)
	}
	return misskeyEntries(batch), nil
}

// misskeyEntries reads notes as the posts the engine walks, each with its
// files.
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
func misskeyEntries(notes []misskeyNote) []fediEntry {
	entries := make([]fediEntry, 0, len(notes))
	for _, note := range notes {
		entry := fediEntry{id: note.ID}
		for _, file := range note.Files {
			if file.URL == "" {
				continue
			}
			size := knownSize(file.Size)
			if misskeyIsDerived(file.URL) {
				size = -1
			}
			entry.files = append(entry.files, File{Name: misskeyName(&file), URL: file.URL, Size: size})
		}
		entries = append(entries, entry)
	}
	return entries
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
	name := cmp.Or(file.Name, util.NameFromURL(file.URL), file.ID)
	served := fediExt(file.URL)
	if served != "" && !strings.EqualFold(path.Ext(name), served) {
		name = util.WithExt(name, served)
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
