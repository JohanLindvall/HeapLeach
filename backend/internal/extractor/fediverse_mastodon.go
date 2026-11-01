// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// The Mastodon dialect of the fediverse extractor: the client API that
// Pleroma, Akkoma, GoToSocial and most forks implement as well, plus the
// Pixelfed variant that moves its statuses route. The shared discovery, URL
// shapes and timeline engine stay in fediverse.go.
//
// Nothing here goes near /api/v1/timelines/public: since Mastodon 4.4 it
// answers an anonymous caller 422 "This method requires an authenticated
// user". The per-account statuses route is the one that stayed open, and it
// is what a pasted profile is asking about anyway.

// fediMastodonAPI is the Mastodon client API's base.
const fediMastodonAPI = "/api/v1"

// fediMastodon is the Mastodon client API, which Pleroma, Akkoma,
// GoToSocial, Hometown and most forks implement as well. It is also what
// software nobody here has heard of is assumed to speak.
var fediMastodon = fediDialect{
	api: fediMastodonAPI, stride: fediMastodonPageSize,
	post: mastodonSingle, owner: mastodonOwner, page: mastodonTimeline,
	empty: "%s has attached no media to its most recent %d posts",
}

// fediPixelfed is that same API with its statuses route moved.
//
// Pixelfed answers /api/v1/accounts/<id>/statuses with a 302 to its login
// page, and following that redirect yields HTML where JSON was expected. Its
// own prefix serves the identical Mastodon-shaped statuses to anyone. The
// account lookup is the other way round — only /api/v1 has it — so the two
// bases cannot be collapsed into one. The stride travels with the base
// because pixelfed caps a page at 24 and refuses anything larger.
var fediPixelfed = fediDialect{
	api: "/api/pixelfed/v1", stride: fediPixelfedPageSize,
	post: pixelfedSingle, owner: mastodonOwner, page: mastodonTimeline,
	empty: fediMastodon.empty,
}

// mastodonAccount is the part of an account this needs, which is mostly its
// id: every other route is keyed by that rather than by the name.
type mastodonAccount struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Acct     string `json:"acct"`
}

// mastodonStatus is one post.
type mastodonStatus struct {
	ID      string          `json:"id"`
	Account mastodonAccount `json:"account"`
	Media   []mastodonMedia `json:"media_attachments"`
	// Reblog is the post this one boosts, if it is a boost at all.
	Reblog *mastodonStatus `json:"reblog"`
}

// mastodonMedia is one attachment.
//
// preview_url is deliberately absent. It is the same file under /small/ — a
// thumbnail — and a struct that does not carry it cannot reach for it by
// mistake when url is empty. meta is absent for a sharper reason:
// meta.original carries a field named "size" whose value is "2225x3356", the
// pixel dimensions. Read as a byte count it would be wrong by three orders of
// magnitude, and it would be wrong in the direction that tells the downloader
// a part-finished file is already complete.
type mastodonMedia struct {
	URL string `json:"url"`
	// RemoteURL is the origin instance's copy, set when this instance is
	// showing somebody else's media. It stands in when the local cache has
	// not been filled yet, which is what a null url means.
	RemoteURL string `json:"remote_url"`
}

// mastodonOwner names an account's timeline by the account's own handle.
func mastodonOwner(ctx context.Context, s *fediSession) (string, string, error) {
	account, err := mastodonLookup(ctx, s)
	if err != nil {
		return "", "", err
	}
	return account.ID, "@" + fediHandle(cmp.Or(account.Acct, account.Username, s.t.handle), s.host()), nil
}

// mastodonLookup turns a handle into the account, and above all into its id.
//
// lookup is asked first and /accounts/<nickname> second, in that order for a
// reason: Pleroma predates accounts/lookup and answers 404 for it while
// accepting a nickname where Mastodon insists on a numeric id — and Mastodon
// answers 404 for *that*. Either way round only one host pays the second
// request, and only when the first found nothing.
func mastodonLookup(ctx context.Context, s *fediSession) (*mastodonAccount, error) {
	handle := s.t.handle
	if handle == "" {
		return nil, fmt.Errorf("fediverse: the link names no account")
	}

	var account mastodonAccount
	lookup := s.root + fediMastodonAPI + "/accounts/lookup?acct=" + url.QueryEscape(handle)
	err := s.client.GetJSON(ctx, lookup, fediHeaders(s.root), &account)
	if err == nil && account.ID != "" {
		return &account, nil
	}

	byName := s.root + fediMastodonAPI + "/accounts/" + url.PathEscape(handle)
	if nickErr := s.client.GetJSON(ctx, byName, fediHeaders(s.root), &account); nickErr == nil && account.ID != "" {
		return &account, nil
	}
	if err == nil {
		err = fmt.Errorf("the lookup returned no account")
	}
	return nil, fmt.Errorf("fediverse: no account %q on this instance: %w", handle, err)
}

// mastodonTimeline reads one page of an account's media posts. only_media is
// what keeps the timeline page cap spent on posts there is something to
// fetch from rather than on a year of conversation.
func mastodonTimeline(ctx context.Context, s *fediSession, until string, _ int) ([]fediEntry, error) {
	statuses, err := mastodonStatuses(ctx, s, s.owner, until)
	return mastodonEntries(statuses), err
}

// mastodonStatuses reads one page of an account's media posts, those that
// precede until when it is set.
func mastodonStatuses(ctx context.Context, s *fediSession, accountID, until string) ([]mastodonStatus, error) {
	query := url.Values{}
	query.Set("limit", strconv.Itoa(s.d.stride))
	query.Set("only_media", "true")
	if until != "" {
		query.Set("max_id", until)
	}
	endpoint := fmt.Sprintf("%s%s/accounts/%s/statuses?%s",
		s.root, s.d.api, url.PathEscape(accountID), query.Encode())

	var batch []mastodonStatus
	if err := s.client.GetJSON(ctx, endpoint, fediHeaders(s.root), &batch); err != nil {
		return nil, fmt.Errorf("fediverse: read the account's posts: %w", err)
	}
	return batch, nil
}

// mastodonSingle fetches a single post.
func mastodonSingle(ctx context.Context, s *fediSession) (*Result, error) {
	var status mastodonStatus
	endpoint := s.root + fediMastodonAPI + "/statuses/" + url.PathEscape(s.t.id)
	if err := s.client.GetJSON(ctx, endpoint, fediHeaders(s.root), &status); err != nil {
		return nil, fmt.Errorf("fediverse: read post %s: %w", s.t.id, err)
	}
	// A link to a boost was pasted because of what the boost shows, so this
	// one follows it. A timeline does the opposite; see mastodonEntries.
	if status.Reblog != nil {
		status = *status.Reblog
	}
	return mastodonPostResult(s, &status)
}

// pixelfedSingle finds a single post on its author's timeline.
//
// Pixelfed serves neither of its single-status routes to a logged-out caller
// — /api/v1 redirects to the login page and its own prefix answers 403 — so
// there the post is found on the account's timeline instead, and in one
// request rather than by walking it: ids are ordered and max_id is exclusive,
// so asking for what precedes id+1 puts the wanted post first.
func pixelfedSingle(ctx context.Context, s *fediSession) (*Result, error) {
	account, err := mastodonLookup(ctx, s)
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseUint(s.t.id, 10, 64)
	if err != nil || id == ^uint64(0) {
		return nil, fmt.Errorf("fediverse: %q is not a pixelfed post id", s.t.id)
	}
	statuses, err := mastodonStatuses(ctx, s, account.ID, strconv.FormatUint(id+1, 10))
	if err != nil {
		return nil, err
	}
	for i := range statuses {
		if statuses[i].ID == s.t.id {
			return mastodonPostResult(s, &statuses[i])
		}
	}
	return nil, fmt.Errorf("fediverse: post %s is not among %s's recent posts, and pixelfed "+
		"serves a single post only to signed-in callers", s.t.id, s.t.handle)
}

// mastodonPostResult is a single post's job, named after its author.
func mastodonPostResult(s *fediSession, status *mastodonStatus) (*Result, error) {
	files := fediFiles(mastodonEntries([]mastodonStatus{*status}))
	if len(files) == 0 {
		return nil, fmt.Errorf("fediverse: post %s on %s carries no media", s.t.id, s.host())
	}
	handle := fediHandle(cmp.Or(status.Account.Acct, s.t.handle), s.host())
	return &Result{Title: "@" + handle + " " + s.t.id, Files: files}, nil
}

// mastodonEntries reads statuses as the posts the engine walks, each with
// its attachments.
//
// A boost carries no files rather than its original's, and that is not
// merely a preference about whose posts belong in the folder: Pleroma and
// Akkoma fill a boost's own media_attachments with the *original author's*
// files, where Mastodon leaves it empty. Taking them would quietly mix
// somebody else's media into a job that named this account, and on a Pleroma
// instance that is the common case rather than the odd one.
//
// No size is reported at all, because nothing in this API states a byte
// length — see mastodonMedia for the field that looks like one and is not.
func mastodonEntries(statuses []mastodonStatus) []fediEntry {
	entries := make([]fediEntry, 0, len(statuses))
	for _, status := range statuses {
		entry := fediEntry{id: status.ID}
		for i, media := range status.Media {
			if link := cmp.Or(media.URL, media.RemoteURL); link != "" && status.Reblog == nil {
				entry.files = append(entry.files, File{
					Name: fmt.Sprintf("%s_%d%s", status.ID, i+1, fediExt(link)),
					URL:  link,
					Size: -1,
				})
			}
		}
		entries = append(entries, entry)
	}
	return entries
}
