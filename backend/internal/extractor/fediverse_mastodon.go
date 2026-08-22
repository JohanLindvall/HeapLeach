package extractor

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// The Mastodon dialect of the fediverse extractor: the client API that
// Pleroma, Akkoma, GoToSocial and most forks implement as well, plus the
// Pixelfed variant that moves its statuses route. Split from fediverse.go the
// way mega keeps its cryptography apart from its API plumbing; the shared
// discovery and URL shapes stay there.

// ------------------------------------------------------------------ mastodon

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

// mastodon resolves an account or a single status through the Mastodon
// client API.
//
// Nothing here goes near /api/v1/timelines/public: since Mastodon 4.4 it
// answers an anonymous caller 422 "This method requires an authenticated
// user". The per-account statuses route is the one that stayed open, and it
// is what a pasted profile is asking about anyway.
func (f *Fediverse) mastodon(ctx context.Context, u *url.URL, t *fediTarget, pixelfed bool) (*Result, error) {
	if t.kind == fediCommunity {
		return nil, fmt.Errorf("fediverse: communities are a lemmy thing, and %s does not run "+
			"it (%s)", u.Hostname(), u.Redacted())
	}
	root := util.Origin(u)

	if t.kind == fediPost {
		status, err := f.mastodonOne(ctx, root, t, pixelfed)
		if err != nil {
			return nil, err
		}
		files := mastodonFiles([]mastodonStatus{*status})
		if len(files) == 0 {
			return nil, fmt.Errorf("fediverse: post %s on %s carries no media", t.id, u.Hostname())
		}
		handle := fediHandle(util.FirstNonEmpty(status.Account.Acct, t.handle), u.Hostname())
		return &Result{Title: "@" + handle + " " + t.id, Files: files}, nil
	}

	account, err := f.mastodonLookup(ctx, root, t.handle)
	if err != nil {
		return nil, err
	}
	title := "@" + fediHandle(util.FirstNonEmpty(account.Acct, account.Username, t.handle), u.Hostname())

	statuses, err := f.mastodonTimeline(ctx, root, account.ID, "", pixelfed)
	if err != nil {
		return nil, err
	}
	files := mastodonFiles(statuses)
	if len(files) == 0 {
		_, stride := mastodonPaging(pixelfed)
		return nil, fmt.Errorf("fediverse: %s has attached no media to its most recent %d posts",
			title, config.MaxTimelinePages*stride)
	}
	return &Result{Title: title, Files: files}, nil
}

// mastodonPaging picks the base and the page stride for the dialect in hand.
//
// Pixelfed answers /api/v1/accounts/<id>/statuses with a 302 to its login
// page, and following that redirect yields HTML where JSON was expected. Its
// own prefix serves the identical Mastodon-shaped statuses to anyone. The
// account lookup is the other way round — only /api/v1 has it — so the two
// bases cannot be collapsed into one. The stride travels with the base
// because pixelfed caps a page at 24 and refuses anything larger.
func mastodonPaging(pixelfed bool) (api string, pageSize int) {
	if pixelfed {
		return fediPixelfedAPI, fediPixelfedPageSize
	}
	return fediMastodonAPI, fediMastodonPageSize
}

// mastodonLookup turns a handle into the account, and above all into its id.
//
// lookup is asked first and /accounts/<nickname> second, in that order for a
// reason: Pleroma predates accounts/lookup and answers 404 for it while
// accepting a nickname where Mastodon insists on a numeric id — and Mastodon
// answers 404 for *that*. Either way round only one host pays the second
// request, and only when the first found nothing.
func (f *Fediverse) mastodonLookup(ctx context.Context, root, handle string) (*mastodonAccount, error) {
	if handle == "" {
		return nil, fmt.Errorf("fediverse: the link names no account")
	}

	var account mastodonAccount
	lookup := root + fediMastodonAPI + "/accounts/lookup?acct=" + url.QueryEscape(handle)
	err := f.client.GetJSON(ctx, lookup, fediHeaders(root), &account)
	if err == nil && account.ID != "" {
		return &account, nil
	}

	byName := root + fediMastodonAPI + "/accounts/" + url.PathEscape(handle)
	if nickErr := f.client.GetJSON(ctx, byName, fediHeaders(root), &account); nickErr == nil && account.ID != "" {
		return &account, nil
	}
	if err == nil {
		err = fmt.Errorf("the lookup returned no account")
	}
	return nil, fmt.Errorf("fediverse: no account %q on this instance: %w", handle, err)
}

// mastodonTimeline pages back through an account's media posts.
//
// max_id is exclusive and the listing is newest first, so each page asks for
// what precedes the last id of the one before. A page that comes back short,
// or that hands back the id it was given, ends the walk — as does the cap,
// which is what stops an account with a decade of posts resolving for
// minutes. only_media is what keeps that cap spent on posts there is
// something to fetch from rather than on a year of conversation. A page after
// the first that fails is not fatal: what has already been collected is still
// worth queueing.
func (f *Fediverse) mastodonTimeline(ctx context.Context, root, accountID, until string, pixelfed bool) ([]mastodonStatus, error) {
	api, pageSize := mastodonPaging(pixelfed)

	var all []mastodonStatus
	for page := 0; page < config.MaxTimelinePages; page++ {
		query := url.Values{}
		query.Set("limit", strconv.Itoa(pageSize))
		query.Set("only_media", "true")
		if until != "" {
			query.Set("max_id", until)
		}
		endpoint := fmt.Sprintf("%s%s/accounts/%s/statuses?%s",
			root, api, url.PathEscape(accountID), query.Encode())

		var batch []mastodonStatus
		if err := f.client.GetJSON(ctx, endpoint, fediHeaders(root), &batch); err != nil {
			if page == 0 {
				return nil, fmt.Errorf("fediverse: read the account's posts: %w", err)
			}
			break
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)

		next := batch[len(batch)-1].ID
		if next == "" || next == until || len(batch) < pageSize {
			break
		}
		until = next
	}
	return all, nil
}

// mastodonOne fetches a single post.
//
// Pixelfed serves neither of its single-status routes to a logged-out caller
// — /api/v1 redirects to the login page and its own prefix answers 403 — so
// there the post is found on the account's timeline instead, and in one
// request rather than by walking it: ids are ordered and max_id is exclusive,
// so asking for what precedes id+1 puts the wanted post first.
func (f *Fediverse) mastodonOne(ctx context.Context, root string, t *fediTarget, pixelfed bool) (*mastodonStatus, error) {
	if pixelfed {
		account, err := f.mastodonLookup(ctx, root, t.handle)
		if err != nil {
			return nil, err
		}
		id, err := strconv.ParseUint(t.id, 10, 64)
		if err != nil || id == ^uint64(0) {
			return nil, fmt.Errorf("fediverse: %q is not a pixelfed post id", t.id)
		}
		statuses, err := f.mastodonTimeline(ctx, root, account.ID, strconv.FormatUint(id+1, 10), true)
		if err != nil {
			return nil, err
		}
		for i := range statuses {
			if statuses[i].ID == t.id {
				return &statuses[i], nil
			}
		}
		return nil, fmt.Errorf("fediverse: post %s is not among %s's recent posts, and pixelfed "+
			"serves a single post only to signed-in callers", t.id, t.handle)
	}

	var status mastodonStatus
	endpoint := root + fediMastodonAPI + "/statuses/" + url.PathEscape(t.id)
	if err := f.client.GetJSON(ctx, endpoint, fediHeaders(root), &status); err != nil {
		return nil, fmt.Errorf("fediverse: read post %s: %w", t.id, err)
	}
	// A link to a boost was pasted because of what the boost shows, so this
	// one follows it. A timeline does the opposite; see mastodonFiles.
	if status.Reblog != nil {
		return status.Reblog, nil
	}
	return &status, nil
}

// mastodonFiles flattens statuses into downloadable attachments.
//
// A boost is skipped rather than followed, and that is not merely a
// preference about whose posts belong in the folder: Pleroma and Akkoma fill
// a boost's own media_attachments with the *original author's* files, where
// Mastodon leaves it empty. Taking them would quietly mix somebody else's
// media into a job that named this account, and on a Pleroma instance that is
// the common case rather than the odd one.
//
// No size is reported at all, because nothing in this API states a byte
// length — see mastodonMedia for the field that looks like one and is not.
func mastodonFiles(statuses []mastodonStatus) []File {
	seen := make(map[string]bool)
	var files []File

	for _, status := range statuses {
		if status.Reblog != nil {
			continue
		}
		for i, media := range status.Media {
			link := util.FirstNonEmpty(media.URL, media.RemoteURL)
			if link == "" || seen[link] {
				continue
			}
			seen[link] = true
			files = append(files, File{
				Name: fmt.Sprintf("%s_%d%s", status.ID, i+1, fediExt(link)),
				URL:  link,
				Size: -1,
			})
		}
	}
	return files
}
