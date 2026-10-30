// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Arte reads ARTE's player API (api.arte.tv/api/player/v2), which answers
// anyone, in each of the six languages the site is published in.
//
// A programme's configuration names one HLS master for every version of it:
// each audio language, clear speech and audio description as renditions of
// their own, every subtitle set as a WebVTT rendition, and the video in
// H.264 and HEVC. The downloader joins them all (download/mux.go), so a
// programme comes out as one MP4 carrying every version it is offered in.
// It used to go to yt-dlp for the muxing. The parts are byte ranges of one
// file per rendition, and the links are not signed.
//
// A collection (RC-…) is read through the playlist endpoint, which names
// each programme with its title, so the collection costs one request. Its
// programmes' streams are resolved when their transfers start.
type Arte struct {
	client *httpx.Client
	// api is the player API's base, a field so that tests can stand in for
	// it.
	api string
}

const arteAPIRoot = "https://api.arte.tv/api/player/v2"

// arteLanguages are the languages ARTE publishes in, the first path
// segment of every page.
var arteLanguages = []string{"de", "fr", "en", "es", "pl", "it"}

var (
	arteProgrammeID  = regexp.MustCompile(`^[0-9]{6}-[0-9]{3}-[A-Z]$`)
	arteCollectionID = regexp.MustCompile(`^RC-[0-9]+$`)
)

// NewArte builds the ARTE extractor.
func NewArte(client *httpx.Client) *Arte {
	return &Arte{client: client, api: arteAPIRoot}
}

func (a *Arte) Name() string { return "arte" }

// Recognises is the inventory's description.
func (a *Arte) Recognises() string {
	return "programmes and collections (`/<lang>/videos/<id>/`, `/embeds/<lang>/<id>`) of ARTE, whose page calls its player API (`/api/player/v2/`)"
}

// Shape reports a programme or collection link.
func (a *Arte) Shape(u *url.URL) bool {
	_, id := arteLink(u)
	return id != ""
}

// Recognise takes a programme or collection page that configures ARTE's
// player API (/api/player/v2/), the one this reads.
func (a *Arte) Recognise(_ context.Context, p *Page) bool {
	return a.Shape(p.Final) && p.Has("/api/player/v2/") && p.Named("ARTE")
}

// arteLink reads a page's language and the programme or collection it names:
// /<lang>/videos/<id>/<slug>/, and the player's /embeds/<lang>/<id>.
func arteLink(u *url.URL) (lang, id string) {
	segs := util.PathSegments(u)
	switch {
	case len(segs) >= 3 && segs[0] == "embeds":
		lang, id = segs[1], segs[2]
	case len(segs) >= 3 && segs[1] == "videos":
		lang, id = segs[0], segs[2]
	default:
		return "", ""
	}
	if !slices.Contains(arteLanguages, lang) || !(arteProgrammeID.MatchString(id) || arteCollectionID.MatchString(id)) {
		return "", ""
	}
	return lang, id
}

// Extract reads a programme into its one file, or a collection into one
// file a programme.
func (a *Arte) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	lang, id := arteLink(u)
	if id == "" {
		return nil, fmt.Errorf("arte: %s names no programme or collection", u.Redacted())
	}
	if arteCollectionID.MatchString(id) {
		return a.collection(ctx, lang, id)
	}
	target, title, err := a.programme(ctx, lang, id)
	if err != nil {
		return nil, err
	}
	return &Result{Title: title, Files: []File{target.file("")}}, nil
}

// arteConfig is the part of a programme's configuration read here.
type arteConfig struct {
	Data struct {
		Attributes struct {
			Metadata struct {
				Title    string `json:"title"`
				Subtitle string `json:"subtitle"`
			} `json:"metadata"`
			Live        bool `json:"live"`
			Restriction struct {
				Geoblocking struct {
					Code           string `json:"code"`
					RestrictedArea bool   `json:"restrictedArea"`
				} `json:"geoblocking"`
			} `json:"restriction"`
			Error *struct {
				Title   string `json:"title"`
				Message string `json:"message"`
			} `json:"error"`
			Streams []struct {
				URL      string `json:"url"`
				Protocol string `json:"protocol"`
			} `json:"streams"`
		} `json:"attributes"`
	} `json:"data"`
}

// config reads a programme's configuration and title, refusing what ARTE
// does not show here.
func (a *Arte) config(ctx context.Context, lang, id string) (*arteConfig, string, error) {
	var cfg arteConfig
	if err := a.client.GetJSON(ctx, a.api+"/config/"+lang+"/"+url.PathEscape(id), arteHeaders(), &cfg); err != nil {
		if httpx.HasStatus(err, http.StatusNotFound) {
			return nil, "", &broadcastRefusal{"arte", id, "it is not online: it does not exist, or its time on ARTE is over"}
		}
		return nil, "", fmt.Errorf("arte: %s: %w", id, err)
	}
	attrs := cfg.Data.Attributes
	title := nrkJoin(strings.TrimSpace(attrs.Metadata.Title), strings.TrimSpace(attrs.Metadata.Subtitle))
	title = cmp.Or(title, id)
	switch geo := attrs.Restriction.Geoblocking; {
	case attrs.Error != nil:
		return nil, "", &broadcastRefusal{"arte", title, cmp.Or(attrs.Error.Message, attrs.Error.Title, "refused")}
	case geo.RestrictedArea:
		return nil, "", &broadcastRefusal{"arte", title, "it is not shown where this is asked from (ARTE limits it to " + geo.Code + ")"}
	case attrs.Live:
		return nil, "", &broadcastRefusal{"arte", title, "it is a live broadcast, which has no end to download to"}
	}
	return &cfg, title, nil
}

// programme reads a programme's configuration and resolves its stream.
func (a *Arte) programme(ctx context.Context, lang, id string) (*Target, string, error) {
	cfg, title, err := a.config(ctx, lang, id)
	if err != nil {
		return nil, "", err
	}
	attrs := cfg.Data.Attributes
	var master string
	for _, s := range attrs.Streams {
		if strings.Contains(strings.ToUpper(s.Protocol), "HLS") && s.URL != "" {
			master = s.URL
			break
		}
	}
	if master == "" {
		return nil, "", fmt.Errorf("arte: %s offers no stream to download", title)
	}
	headers := arteHeaders()
	media, err := resolveMediaPlaylist(ctx, a.client, master, headers)
	if err != nil {
		return nil, "", fmt.Errorf("arte: %s: %w", title, err)
	}
	return media.target(master, title+media.extension(), headers), title, nil
}

// arteHeaders are what the site's own player sends.
func arteHeaders() httpx.Header {
	return httpx.RefererOrigin("https://www.arte.tv/", "https://www.arte.tv")
}

// arteCollection is the part of a collection's playlist read here.
type arteCollection struct {
	Data struct {
		Attributes struct {
			Metadata struct {
				Title string `json:"title"`
			} `json:"metadata"`
			Items []struct {
				ProviderID string `json:"providerId"`
				Title      string `json:"title"`
				Subtitle   string `json:"subtitle"`
			} `json:"items"`
		} `json:"attributes"`
	} `json:"data"`
}

// collection reads a collection into one file a programme, its streams
// resolved when each transfer starts. A collection within it, a season of
// a series, is read into a folder of its own, one level down.
//
// Each programme's configuration is read as the collection is, several at
// a time, because ARTE shows some programmes of a collection in fewer
// countries than others and the listing does not say which. One it does
// not show here is left out, the job's note giving ARTE's own reason once;
// one whose configuration could not be read for any other cause is kept,
// for its transfer to try again. Each check is a request, so a collection
// is checked as far as config.MaxResolvedEpisodes programmes.
func (a *Arte) collection(ctx context.Context, lang, id string) (*Result, error) {
	title, entries, err := a.playlist(ctx, lang, id)
	if err != nil {
		return nil, err
	}
	var programmes []arteEntry
	for _, entry := range entries {
		if !arteCollectionID.MatchString(entry.id) {
			programmes = append(programmes, entry)
			continue
		}
		season, listed, err := a.playlist(ctx, lang, entry.id)
		if err != nil {
			return nil, err
		}
		for _, p := range listed {
			if arteProgrammeID.MatchString(p.id) {
				p.season = cmp.Or(season, entry.title, entry.id)
				programmes = append(programmes, p)
			}
		}
	}
	title = cmp.Or(title, id)
	if len(programmes) == 0 {
		return nil, fmt.Errorf("arte: %s lists no programmes", title)
	}

	var refused []string
	resolved, note, err := expandSeries(ctx, series[arteEntry]{
		site: "arte", noun: "programmes", link: title,
		items: programmes,
		// A season of a collection is a folder of its own even when it is
		// the only one, beside the programmes listed at the top.
		label: func(entry arteEntry) string { return entry.season },
		split: true,
		resolve: func(ctx context.Context, entry arteEntry) (*File, error) {
			if _, _, err := a.config(ctx, lang, entry.id); err != nil {
				if _, refused := refusalReason(err); refused {
					return nil, err
				}
			}
			file := a.lazy(lang, entry)
			return &file, nil
		},
		refused: &refused,
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	return &Result{Title: title, Files: listedFiles(resolved), Note: listingNote(note, refusedNote(refused))}, nil
}

// arteEntry is one programme or collection a playlist names, and the season
// of the collection a programme of a season is listed under.
type arteEntry struct{ id, title, season string }

// playlist reads a collection's title and the entries it lists.
func (a *Arte) playlist(ctx context.Context, lang, id string) (string, []arteEntry, error) {
	var doc arteCollection
	if err := a.client.GetJSON(ctx, a.api+"/playlist/"+lang+"/"+url.PathEscape(id), arteHeaders(), &doc); err != nil {
		if httpx.HasStatus(err, http.StatusNotFound) {
			return "", nil, &broadcastRefusal{"arte", "collection " + id, "it is not online: it does not exist, or its time on ARTE is over"}
		}
		return "", nil, fmt.Errorf("arte: collection %s: %w", id, err)
	}
	attrs := doc.Data.Attributes
	var entries []arteEntry
	for _, item := range attrs.Items {
		if item.ProviderID == "" {
			continue
		}
		title := nrkJoin(strings.TrimSpace(item.Title), strings.TrimSpace(item.Subtitle))
		entries = append(entries, arteEntry{id: item.ProviderID, title: cmp.Or(title, item.ProviderID)})
	}
	return strings.TrimSpace(attrs.Metadata.Title), entries, nil
}

// lazy is a collection's programme, its stream resolved when the transfer
// starts: a collection can run to dozens, each a configuration, a master
// and a playlist for every version.
func (a *Arte) lazy(lang string, entry arteEntry) File {
	return lazyEpisode(entry.title+".mp4", "", arteHeaders(), func(ctx context.Context) (*Target, error) {
		target, _, err := a.programme(ctx, lang, entry.id)
		return target, err
	})
}
