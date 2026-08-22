package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// RUV resolves programmes and episodes of Iceland's public broadcaster.
//
// The player is backed by a plain GraphQL endpoint that asks for nothing at
// all — no account, no key, no cookie, no token, not even a User-Agent — and
// one query answers with the entire programme, every episode's stream URL
// already in it. That is what makes this the smallest of the broadcasters
// here: where svtplay and nrk list a series and then fetch each episode's
// manifest one at a time, a RÚV series of any length costs a single request,
// and there is no season nesting to unpick either, because RÚV files each
// season as a programme of its own with its own slug and id.
//
// Two kinds of file come back, and both are wanted. Television is an HLS
// master playlist whose only EXT-X-MEDIA entry is subtitles: no audio group
// is declared anywhere, so every variant carries video and audio in the same
// MPEG-TS segments and concatenating the best one yields something that
// plays. Radio is not a playlist at all but a plain MP3 with a
// Content-Length that answers byte ranges — much the better case, since it
// gets the segmented engine, resume and the skip check, so it is passed
// through as a URL rather than dressed up as a stream.
//
// Geography is where this differs from NRK, and the difference decides the
// code. RÚV declares it per episode as a category, "Global" or "Domestic",
// which maps onto the CDN path — /opid/ is open, /lokad/ is locked, and
// Akamai answers a foreign address with a 403 page. It is tempting to read
// that field and refuse before fetching anything. It would also be wrong:
// unlike NRK's nonPlayable, which the API computes for whoever asked, scope
// is a fixed label on the episode and reads "Domestic" for a viewer in
// Reykjavík too — for whom the file plays perfectly well. Refusing on it
// would refuse the one audience this extractor exists to serve. So scope is
// not a veto but an explanation: the manifest is fetched, and a 403 on an
// episode RÚV marked Domestic is reported in words instead of as an Akamai
// error page. That refusal also settles the question for the rest of the
// extraction, so a wholly Iceland-only series costs one failed request
// rather than one per episode.
type RUV struct {
	client *httpx.Client
}

const (
	ruvRoot    = "https://www.ruv.is"
	ruvGraphQL = "https://spilari.nyr.ruv.is/gql/"

	// ruvPlayPath is the path segment every playable link has in common.
	// What precedes it names the station — sjonvarp, utvarp, krakkaruv,
	// ungruv, menntaruv — and is cosmetic: all five are one catalogue
	// behind one API.
	ruvPlayPath = "spila"

	// ruvGlobalScope is the one scope that is not restricted to Iceland.
	ruvGlobalScope = "Global"

	// ruvMaxEpisodes bounds how many episodes one programme URL expands to.
	ruvMaxEpisodes = 500

	// ruvPlaylistExt marks an adaptive manifest. Everything else RÚV names
	// is a file that can be fetched whole.
	ruvPlaylistExt = ".m3u8"
)

// ruvProgramQuery is the player's own getEpisode operation, cut down to what
// is used here.
//
// foreign_title is deliberately not asked for: it is the original-language
// name of an imported programme — "Mini-Agenterna" for "Smáspæjararnir" —
// and naming a download after it would disagree with the page the link came
// from.
const ruvProgramQuery = `query getEpisode($programID: Int!) {
  Program(id: $programID) {
    id
    slug
    title
    reverse_episode_order
    episodes { id title scope file }
  }
}`

// NewRUV builds the RÚV extractor.
func NewRUV(client *httpx.Client) *RUV { return &RUV{client: client} }

func (r *RUV) Name() string { return "ruv" }

// Match takes ruv.is and everything under it. The stations are separate
// paths rather than separate hosts, and the API lives on a subdomain.
func (r *RUV) Match(u *url.URL) bool { return util.HostMatches(u.Host, "ruv.is") }

// Extract handles one episode or a whole programme, which are the same
// request: the listing already carries every stream URL, so a link naming an
// episode is a filter over what has been fetched anyway.
func (r *RUV) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	programID, episodeID, ok := ruvTarget(u)
	if !ok {
		return nil, fmt.Errorf("ruv: no programme in %s "+
			"(a playable link looks like %s/sjonvarp/spila/<slug>/<programme id>)",
			u.Redacted(), ruvRoot)
	}

	prog, err := r.programme(ctx, programID)
	if err != nil {
		return nil, err
	}

	session := &ruvSession{client: r.client}
	if episodeID != "" {
		return session.single(ctx, prog, episodeID)
	}
	return session.whole(ctx, prog, u)
}

// ruvSession is one extraction's worth of state.
//
// The single field is what a refusal has already proved. A Domestic episode
// turned away by the CDN says nothing about that episode and everything
// about the address asking: it is outside Iceland, and every other
// Iceland-only episode in the same listing will be refused for the same
// reason. Remembering that turns a hundred pointless requests into one.
type ruvSession struct {
	client *httpx.Client
	// outsideIceland records that an Iceland-only file has already been
	// refused this run.
	outsideIceland bool
}

// single resolves the one episode a link named.
func (s *ruvSession) single(ctx context.Context, prog *ruvProgram, episodeID string) (*Result, error) {
	i := slices.IndexFunc(prog.Episodes, func(ep ruvEpisode) bool { return ep.ID == episodeID })
	if i < 0 {
		return nil, fmt.Errorf("ruv: %s lists no episode %s "+
			"(it may have left the catalogue since the link was written)", prog.name(), episodeID)
	}

	file, err := s.file(ctx, prog.Episodes[i])
	if err != nil {
		return nil, err
	}
	// A lone file lands in the download directory with no folder to say what
	// it belongs to, so its name carries the programme as well: RÚV episode
	// titles are routinely no more than "Þáttur 1 af 6".
	title := ruvJoin(prog.name(), prog.Episodes[i].Title)
	file.Name = title + file.Name
	return &Result{Title: title, Files: []File{*file}}, nil
}

// whole expands a programme into one file per episode.
func (s *ruvSession) whole(ctx context.Context, prog *ruvProgram, u *url.URL) (*Result, error) {
	episodes := prog.ordered()
	if len(episodes) > ruvMaxEpisodes {
		// The cap keeps the head of the list as RÚV orders it, which is what
		// makes it the right end to keep: the only programmes long enough to
		// reach it are the daily strands, and those are the ones the player
		// leaves newest-first.
		episodes = episodes[:ruvMaxEpisodes]
	}
	names := ruvNames(episodes)

	result := &Result{Title: prog.name()}
	var refused string
	for i, ep := range episodes {
		file, err := s.file(ctx, ep)
		if err != nil {
			// One Iceland-only or empty episode must not sink the rest. The
			// reason is kept, though: when every episode is refused they are
			// refused for the same reason, and saying it once is the whole
			// point of recognising it.
			var refusal *ruvRefused
			if refused == "" && errors.As(err, &refusal) {
				refused = refusal.message
			}
			continue
		}
		file.Name = names[i] + file.Name
		result.Files = append(result.Files, *file)
	}
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("ruv: none of the %d episodes of %s are playable: %s",
			len(episodes), u.Redacted(),
			util.FirstNonEmpty(refused, "the programme lists them but none carries a stream"))
	}
	return result, nil
}

// file resolves one episode to something downloadable. The returned File
// carries only the extension in Name; the caller supplies the stem, which
// differs between a lone episode and one of a programme's many.
func (s *ruvSession) file(ctx context.Context, ep ruvEpisode) (*File, error) {
	if ep.File == "" {
		return nil, fmt.Errorf("ruv: %s is listed but names no file", ep.label())
	}

	// Scope is a label on the episode rather than an answer to this caller,
	// so it is never a reason to refuse on its own — but once one Domestic
	// file has been turned away, the rest need not be asked for.
	domestic := !strings.EqualFold(ep.Scope, ruvGlobalScope)
	if domestic && s.outsideIceland {
		return nil, &ruvRefused{episode: ep.label(), message: ruvDomesticRefusal}
	}

	headers := httpx.Referer(ruvRoot + "/")
	if ext := ruvExtension(ep.File); ext != ruvPlaylistExt {
		// Radio: a plain file with a length and byte ranges. There is no
		// size to declare here — the schema publishes duration in seconds
		// and no byte count anywhere — so the transfer reads Content-Length
		// itself, which is exact where a rounded figure would poison the
		// skip check.
		return &File{
			Name:    ext,
			URL:     ep.File,
			Size:    -1,
			Headers: headers,
		}, nil
	}

	segments, variant, err := resolvePlaylist(ctx, s.client, ep.File, headers)
	if err != nil {
		if domestic && httpx.HasStatus(err, http.StatusForbidden) {
			s.outsideIceland = true
			return nil, &ruvRefused{episode: ep.label(), message: ruvDomesticRefusal}
		}
		return nil, fmt.Errorf("ruv: %s: %w", ep.label(), err)
	}
	return &File{
		Name:     playlistExtension(variant),
		Size:     -1,
		Headers:  headers,
		Segments: segments,
	}, nil
}

// programme fetches a listing, which is the only request this host needs.
func (r *RUV) programme(ctx context.Context, id int) (*ruvProgram, error) {
	body := map[string]any{
		"query":     ruvProgramQuery,
		"variables": map[string]int{"programID": id},
	}
	var out ruvResponse
	if err := r.client.PostJSON(ctx, ruvGraphQL, httpx.Referer(ruvRoot+"/"), body, &out); err != nil {
		return nil, fmt.Errorf("ruv: programme %d: %w", id, err)
	}
	return out.program(id)
}

// ruvResponse is the GraphQL envelope. Both halves are read, because the two
// ways this query fails look nothing alike.
type ruvResponse struct {
	Data struct {
		Program *ruvProgram `json:"Program"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// program unwraps the answer.
func (r ruvResponse) program(id int) (*ruvProgram, error) {
	// A schema that has moved on answers with a null programme and an
	// explanation beside it. Reporting that explanation is the difference
	// between a fixable bug report and a misleading "not a programme id".
	if len(r.Errors) > 0 {
		return nil, fmt.Errorf("ruv: programme %d: %s", id, r.Errors[0].Message)
	}
	if r.Data.Program == nil {
		// Program(id:) is strict about which id it takes, and the site has
		// several. The number in a /spila/ path is the right one; the ids
		// inside Featured panels and Category listings are a different key
		// altogether, which is why the Category query names the real one
		// "programID" alongside them.
		return nil, fmt.Errorf("ruv: %d is not a programme id "+
			"(only the number in a /%s/ link is one)", id, ruvPlayPath)
	}
	return r.Data.Program, nil
}

// ruvTarget reads a programme, and the episode where a link names one, out of
// a /<station>/spila/<slug>/<programme id>/<episode id> path.
//
// The reading is positional rather than a hunt for the first number, because
// the slug sits between the two and a programme whose slug is all digits
// would otherwise be mistaken for the id. The player builds every link this
// way, so there is nothing to gain by guessing.
func ruvTarget(u *url.URL) (programme int, episode string, ok bool) {
	segs := util.PathSegments(u)
	i := slices.Index(segs, ruvPlayPath)
	if i < 0 || i+2 >= len(segs) {
		return 0, "", false
	}
	id, err := strconv.Atoi(segs[i+2])
	if err != nil {
		return 0, "", false
	}
	if i+3 < len(segs) {
		episode = segs[i+3]
	}
	return id, episode, true
}

// ruvNames names each episode of a programme, and is where RÚV's one naming
// trap is dealt with: episode titles are not unique. A news strand runs to
// well over a thousand instalments of which a couple of dozen are all called
// "Fréttir og veður", and files sharing a name share a destination — the
// second would be written over the first.
//
// Only the titles that actually repeat are disambiguated, since spoiling a
// thousand good names to fix eighteen would be a poor trade. The episode id
// goes in front rather than behind because a long Icelandic title can reach
// the filename length limit, and truncation keeps the extension and eats the
// end.
func ruvNames(episodes []ruvEpisode) []string {
	names := make([]string, len(episodes))
	count := make(map[string]int, len(episodes))
	for i, ep := range episodes {
		names[i] = util.FirstNonEmpty(strings.TrimSpace(ep.Title), ep.ID)
		count[names[i]]++
	}
	for i, ep := range episodes {
		if count[names[i]] > 1 {
			names[i] = ep.ID + " " + names[i]
		}
	}
	return names
}

// ruvJoin names one episode by its programme and its own title together.
//
// RÚV writes that pair as "Programme: Episode" throughout its own site, and
// this does not: a colon is one of the characters a filename cannot keep, so
// it would arrive on disk as an underscore. The equality case is not
// hypothetical either — a one-off programme is routinely given a single
// episode named after itself.
func ruvJoin(programme, episode string) string {
	episode = strings.TrimSpace(episode)
	switch {
	case episode == "":
		return programme
	case programme == "" || strings.EqualFold(programme, episode):
		return episode
	}
	return programme + " - " + episode
}

// ruvExtension reads the container off an episode's file URL: ".m3u8" for
// television, ".mp3" for radio.
func ruvExtension(file string) string {
	if u, err := ParseURL(file); err == nil {
		return strings.ToLower(path.Ext(u.Path))
	}
	return strings.ToLower(path.Ext(file))
}

// ruvDomesticRefusal is what a Domestic scope turns into once the CDN has
// confirmed it. RÚV publishes a category rather than a sentence — there is no
// endUserMessage to pass through the way NRK offers one — so the words are
// this extractor's, and they say the two things a caller can act on: who
// licensed it and that the refusal was of this address rather than of the
// request.
const ruvDomesticRefusal = "RÚV licenses this for Iceland only, and its CDN turned this address away"

// ruvRefused is an episode declined for where the caller is. It is a type
// rather than a message so that expanding a whole programme can report the
// reason once instead of once per episode.
type ruvRefused struct {
	episode string
	message string
}

func (e *ruvRefused) Error() string { return "ruv: " + e.episode + ": " + e.message }

// ruvProgram is a programme and every episode of it, which is the whole of
// what this host is asked for.
type ruvProgram struct {
	ID    int    `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// ReverseEpisodeOrder is the player's instruction to turn the list
	// round; see ordered.
	ReverseEpisodeOrder bool         `json:"reverse_episode_order"`
	Episodes            []ruvEpisode `json:"episodes"`
}

// name is what the job and its folder are called.
func (p *ruvProgram) name() string {
	return util.FirstNonEmpty(strings.TrimSpace(p.Title), p.Slug, strconv.Itoa(p.ID))
}

// ordered puts the episodes in the order RÚV's own player shows them.
//
// The API answers newest-first whatever the programme, so the flag is not a
// description of the list but an instruction about it: honouring it puts a
// numbered series in episode order — which is also the order it is worth
// downloading in — and leaves a daily strand with its most recent
// instalment first.
func (p *ruvProgram) ordered() []ruvEpisode {
	if !p.ReverseEpisodeOrder {
		return p.Episodes
	}
	out := slices.Clone(p.Episodes)
	slices.Reverse(out)
	return out
}

// ruvEpisode is one entry of a programme's listing, stream URL included.
type ruvEpisode struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Scope is "Global" or "Domestic": who the episode is licensed for,
	// not whether this caller may have it.
	Scope string `json:"scope"`
	// File is the manifest or the media itself, ready to fetch.
	File string `json:"file"`
}

// label names an episode in an error message.
func (e ruvEpisode) label() string {
	return util.FirstNonEmpty(strings.TrimSpace(e.Title), e.ID, "an untitled episode")
}
