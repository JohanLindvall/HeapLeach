# Broadcasters and streaming platforms

Notes on sites read through their players' own APIs, mostly as HLS with
separate audio and subtitle renditions. How playlists are fetched and joined
is in [media.md](../media.md); what is common to every extractor is in
[extractors.md](../extractors.md). Where a note says a site's other pages go
to yt-dlp, the extractor keeps a `Handoff` for them.

## vimeo

vimeo is read from the embed player's own configuration (`window.playerConfig`
on `player.vimeo.com/video/<id>`), and that one door is what makes it work:
`vimeo.com/<id>` answers a non-browser client with a bot check and the
player's JSON config endpoint answers 403, while the player page is served to
anyone with a browser's TLS. curl's handshake is turned away from it ("We
couldn't verify the security of your connection"); the client's Chrome-shaped
one is not.

The configuration's HLS master, taken AVC first and on its default CDN, keeps
video, audio and subtitles in playlists of their own, which `mux.go` joins.
Its links are signed for an hour, so `Resolve` reads the page again, holding
to the kind of stream the first reading chose. The tallest progressive MP4 is
taken only when the HLS cannot be read; modern uploads mostly have none. Some
videos are DRM-protected in every form they are offered in, FairPlay HLS and
Widevine DASH (`request.drm`), which yt-dlp cannot fetch either, and those are
refused in those words. A `view` other than 1 is a private or
password-protected video. An unlisted link's `<hash>` becomes `?h=`.

## arte

arte reads ARTE's player API (`api.arte.tv/api/player/v2`), which answers
anyone in each of the six languages the site is published in, the first path
segment of every page. A programme's configuration names one HLS master for
every version of it: each audio language, clear speech and audio description
as renditions of their own, every subtitle set as WebVTT, the video in H.264
and HEVC, its parts byte ranges of one file per rendition, on links that are
not signed. So one MP4 carries every version.

The configuration also says whether the programme is shown where it is asked
from (`restriction.geoblocking`, `error`), and a refusal is passed on in
ARTE's words. A collection (`RC-…`) is the playlist endpoint, which names each
programme and its title. A season nested in a series is read into a folder of
its own. ARTE shows some programmes of a collection in fewer countries than
others and the listing does not say which, so each programme's configuration
is read as the collection is (`FanOut`). One not shown here is left out, the
note giving the reason once (`arteRefused`); one that could not be read for
another cause is kept, for its transfer to try again. Their streams are
resolved when the transfers start.

## orf

orf reads ORF ON's own API (`api-tvthek.orf.at`) with the credentials the site
hands every visitor: its runtime configuration (`window.__NUXT__.config`)
names the API's base, user and password, and the site's player sends them as
Basic authorisation. They are read off the page, never written down here, and
read again when the API refuses them (a rotated build). An older public
endpoint takes the episode id base64'd behind a fixed prefix; the site's own
code uses another prefix for something else, which that endpoint does not
take, so it is not used.

An episode names one adaptive HLS master among fixed qualities, its DRM flag,
the rights it is shown under, and its subtitles as files, WebVTT among them (a
one-file `Track`). The master keeps its audio apart, the audio description one
rendition. What ORF shows in Austria alone is not refused by the API but by
the stream: asked from elsewhere, the master redirects to a notice
(`insert_geoprotection`) that would download as a short clip of something
else. It is recognised and refused in words, and a series' Austria-only
episodes are left out once one has shown it. A series (`/sendereihe/<id>`,
`profile/<id>/episodes`) lists newest first and is filed oldest first.
Anything else on orf.at, its radio and news pages, goes to yt-dlp.

## svtplay

svtplay reads SVT's player API (`api.svt.se/videoplayer-api/video/<id>`). A
video it offers no stream for at all is answered with an empty
`videoReferences`, `hasVodReferences` false and no reason. Such a video's
window has closed or not yet opened, or it is shown in Sweden alone, so
`unavailable` asks SVT Play's own GraphQL what its page shows
(`detailsPageByPath` on `/video/<id>`: `validFrom`, `validTo`,
`restrictions.onlyAvailableInSweden`) and says which. "No usable stream" is
left for a video offered only in forms this cannot read, and names them.

## yle

yle reads Areena through the player API's preview
(`player.api.yle.fi/v1/preview/<id>.json`), asked with the static player's app
id and key, the pair Yle's own player script sends (a chunk
`player-v2.yle.fi/embed.js` loads). Areena's pages publish another pair
(`appIdFrontend`), for its UI API, which the preview refuses with a 403. The
preview is in one of the states the player knows (`yleStates`); one that plays
names its title and series in both languages, its region, and an HLS master in
MPEG-TS with WebVTT subtitle renditions, or for audio one MP3 (`media_url`).
Both are signed for some hours and minted again when the transfer starts.

Three things here are not what they look like:

- `is_content_protected` is not DRM. Programmes it marks that are shown
  worldwide are plain HLS with no key anywhere, and refusing on it would drop
  most of them. A stream that really is encrypted is the playlist reader's to
  refuse.
- What Yle shows in Finland alone (`region` "Finland") is handed a stream like
  any other, whatever `countryCode` says, and the CDN refuses it from
  elsewhere with a 403, the MP3s as well as the masters (`refusedHere` asks an
  MP3 for one byte). The player sends `countryCode` FI until it has located
  the viewer; since it changes nothing, it is not sent.
- A series id asked of the preview answers with one of its episodes, so the
  item's page says what an id is (`meta.item.type`: `TVSeries`, `RadioSeries`,
  `TVSeason`).

A series is its page's first tab's untitled list, read through the UI API
(`areena.api.yle.fi/v1/ui/content/list?token=…`, signed per list) with the
page's own pair, `v=10` and the UI's spelling of the language, `fi` or `sv`; a
two-letter cut of the preview's `swe` is refused. Seasons are the filter's
options (`path.season`), read in the site's order, each into a folder. A
season's own page carries the same filter with itself as the default, and
lists itself alone. A list is followed by the `count` it states and by what
each page adds, not by the size asked for. Each episode is asked of the
preview as the listing is read (`FanOut`): one Areena does not offer is left
out with its reason once, as are the Finland-only ones once one of them has
been refused here. An episode keeps the name it is listed under, the card's
title with its number, so that a listing read again finds what was saved:
`finishedLocked` keys on the name an item finished with, the `.ts`, not the
MP4 it was remuxed into.

## drtv

drtv reads DR TV (`dr.dk/drtv/...`) through the API its player calls
(`production-cdn.dr-massive.com/api`), asked as the anonymous subscriber
(`sub=Anonymous`), as yt-dlp asks. The site asks viewers to sign in before it
plays anything and treats a signed-out browser as another subscriber
(`Anonymous2`) that is offered nothing, but `Anonymous` is served what DR
offers free. A programme the API offers no files (`8009`, "Ingen filer") is
reported as needing a login: that is what every programme would answer if that
subscriber were retired.

An item says what it is (`type`: episode, program, season, show, channel), so
the id after the link's last `_` is dispatched on that; slugs have underscores
of their own. A programme's videos are a master per version, and the standard
one is taken. Its master is CMAF with video-only variants and the audio apart,
every part a byte range of one MP4 per rendition, with no key and no
signature.

Two things in it are not what they say. The audio group holds one track at two
or three bitrates (`English` and `English_2`, or a group per variant), so only
its default is kept; joined whole, the MP4 would carry the same sound twice.
And every rendition is tagged English, Danish programmes included. The API's
`language` replaces the tag: `da`, or `foreign`, which marks foreign speech in
the programme and names no language, so the track is left unnamed.

What DR shows in Denmark alone (`IsGeoRestricted`) its CDN refuses from
elsewhere with a 403, sent as an HTML page under a playlist's content type;
the API does not refuse it. DRM is marked per version (`drm`: `PlayReady`). A
show lists its seasons and a season its episodes (`/children`, 200 a page,
`paging.total` counting pages). Each entry carries its `contextualTitle`, the
episode as its season names it, number and all, and its geo flag, so a listing
needs no request per episode. A season whose title is only the show's is filed
as "Sæson N". Anything else on dr.dk goes to yt-dlp.

## rtbf

rtbf reads Auvio through the two APIs its pages use. RTBF's own
(`bff-service.rtbf.be/auvio/v1.23`) names a media (`embed/media/<id>`) and
lists a programme (`pages/emission/<slug>-<id>`, then its widgets). Red Bee
Media's Exposure (`exposure.api.redbee.live`) plays it for an anonymous
session, which needs no account: POST `auth/anonymous` with a device id of its
own, then `entitlement/<id>_6BA97Bb/play`.

The play answer is the authority on who may watch, in its own terms. A free
account is `LOGIN` (and a future `availableAt` means not yet); Belgium alone
is `NOT_ENTITLED` with no action at all; paid is `BUY_WATCH_NOW`; expired is
`NOT_PUBLISHED`. RTBF's own `isAnonymousAllowed` is not the authority: it can
be false on archives that play. Anonymously that leaves news clips, archives,
some programmes and many podcasts. yt-dlp's RTBF extractor does not work, so
nothing here hands off to it.

A video is Unified Streaming HLS: fragmented MP4, the audio apart, WebVTT
renditions with no timestamp map that count from the programme's start,
nothing encrypted or signed. Some packages declare one recording three times,
as the French track, the original version (`qaa`) and an audio description
(`qad`). The asset record (`content/asset`, `audio[]`) says how many tracks
there really are, and where it says one, only the default rendition is kept.
The same packages declare subtitle tracks with no cues, which the merge leaves
out. A podcast is one MP3, served after a redirect that adds a listening
session.

A programme's episodes are the tabs of its `TAB_LIST` (a season or a year,
each a folder), each list paged by its own `links.next`, 50 at most a page.
The entries say nothing of who may play them, so each is asked of the play API
as the listing is read (`FanOut`), and what cannot be had is left out with
each reason counted once. Any other rtbf.be page, an article say, is read for
the players it embeds (`data-asset-id`, embed links, escaped ones in script
included).

## francetv

francetv reads france.tv through the API its player calls
(`k7.ftven.fr/videos/<id>`), asked as Safari (`browser=safari`, with `domain`
and `device_type`, which it refuses to answer without). Asked as Chrome it
answers with DASH; asked as Safari it answers with HLS of the same asset,
clear for nearly all of the replays. What it keeps under DRM it refuses to a
player that does not declare DRM (2019), which is the clean signal; declared,
it hands over FairPlay. Expired is 2003 and unknown 2012.

The master is signed by the token service the answer names
(`hdfauth.ftven.fr`), six hours in the path and ten minutes in the query, so
it is minted at transfer time. Video is MPEG-TS under AES-128, one static key,
and the audio is packed AAC in renditions of its own (see packed audio in
[media.md](../media.md)). Subtitles are the answer's whole WebVTT files, which
count from the picture's start, rather than the master's segmented ones, whose
packager splits a cue at each segment boundary. `qsm` is French for the deaf
and hard of hearing.

The asset's name says where it is shown: `_monde_` everywhere,
`_france-domtom_` in France alone, refused by the CDN with a 403 elsewhere.
The API answers wherever it is asked from unless told a country, which is not
done.

A video page names its id as its player's `options` in flight data written
into script as a string (`\"`), or in the tracking whose `content_id` is the
number the link ends in. A video page's own `<title>` is the site's name
alone, so pages are named by `og:title`, less its "en replay". A programme's
page and its `toutes-les-videos` pages (`?page=N`) list cards with the video's
id and `content_format`; whole programmes (`integrale`) are kept and clips
left out where both are listed. Anything else on france.tv goes to yt-dlp.

## radio-canada

radio-canada reads ICI Radio-Canada's videos and ICI OHdio's audio through the
media API its players call (`services.radio-canada.ca/media/`), which answers
without a key: `validation/v2` names a stream for an application code and a
media id (`medianet`, with `connectionType=hd`, `deviceType=ipad`), and
`meta/v1` describes one. Other device types are refused ("plate-forme n'est
pas supportée"). Refusals ("n'est pas disponible dans votre pays", premium)
are the API's own words, passed on.

A page names its own media in its data layer (`IdsCollection`,
`MedianetId:<id>`), with its programme beside it (`Emission`, used for OHdio's
names; for news it is a section's). Every page carries other media ids too,
recommendations, so the first id on a page is not its own. A daily show's
episode (`/ohdio/premiere/emissions/...`) names only its `EpisodeId`, and its
media is its player's item for that episode (`playlistItemId`, whose
`globalId2.id` is the episode, `mediaId`). An article (`NewstoryId`) plays the
media of its attachments (`attachment.playableContent.media`): one is named
after the article, several each by the API under the article's title. The
media console's older link shapes are read too
(`/widgets/mediaconsole/<app>/<id>` and `/media-<id>`). An article with no
media, and any page with none of its own, goes to yt-dlp.

Some media are signed (`hdnea` on the master for two minutes, then an `hdntl`
path token and `aka_me_session` on every segment for a day; a segment without
them is a 403), so every file resolves again when its transfer starts. The
API's link also asks the CDN to leave out every rendition above 1.5 Mbps
(`aws.manifestfilter`), and no option of the API's lifts it (every
`connectionType`, `multibitrate=false`). Asked without the filter, the same
CDN serves the whole ladder, so the filter is dropped and any token kept: a
news video then comes at 1280x720 or 1920x1080, where the API's link stops at
852x480. Where the CDN refuses the unfiltered master, the API's own link is
taken instead.

Video is TS with its audio in a rendition of its own, which the packager names
`audio1_fr`, so the name is left off and the language kept; one ending in `dv`
(`audio0_frdv`) is the audio description, named so. OHdio is audio-only TS,
its transcript a WebVTT rendition (which can have no cues at all; the merge
leaves such a track out).

An OHdio programme's page lists its episodes as `EpisodeListItem` entries with
their media ids, fifty a page, newest first. The next pages are
`?pageNumber=N`, which the site's own loader reads off the URL (1 to 200;
`?page=` is ignored), and `pagination.totalCount` says when to stop. A
programme with seasons shows its latest alone; each season is a path of its
own (`/saison-N`), linked from the page's season selector, with
`numberOfSeasons` in its structured data. So every season is read, every page
of it, into a folder named for it (`selectedSeason`), and a season pasted
alone is titled with its name and read alone.

## niconico

niconico is read through the site's own player API. A watch page describes its
video in the JSON of a `<meta name="server-response">`: the qualities on offer
(`media.domand`, video and audio apart, each marked available or not without
an account), a key to ask with (`accessRightKey`), and the player's track id.
POSTed one video and one audio quality, the access-rights API mints an HLS
master for the pair and sets a `domand_bid` cookie. The segments sit on signed
links anyone can fetch, but the AES-128 key answers 403 without that cookie,
so it rides in the transfer's headers rather than being left to the jar. The
audio is a rendition of its own, and the parts are fragmented MP4 under
AES-128 with the initialisation segment in the clear and an explicit IV
(`SegmentKey.Clear`).

The grant and the cookie last a day, and a queue can wait longer, so `Resolve`
mints them when the transfer starts, from a fresh reading of the watch page.
The quality taken is the tallest available, then the highest bitrate. An error
page, deleted or missing, is a 400 or 404 carrying the same description, read
off `StatusError.Body` so the refusal can name its reason.

Mylists (`/v2/mylists`), series (`/v2/series`) and a user's uploads
(`/v3/users/<id>/videos`) list each video's id and title, so a listing costs a
request a page and no watch page until a transfer starts. Entries gone from
the site, or paid, are left out and counted in the note apart from what the
cap cut. Any other page (tags, rankings, channels) goes to yt-dlp.
