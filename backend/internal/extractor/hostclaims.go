// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Host claims: which extractor takes a link because of the host it is on.
//
// Pages are recognised by what they serve, and that stays the rule for any
// host nobody lists. But a site's own hosts say more than its page can when
// the page is no help: an API-driven host whose HTML is a shell, a
// challenge or a redesign the API never noticed; a broadcaster's
// programme whose page carries none of the markers; a player's removed or
// private page, which serves the site's navigation and none of its player;
// a link yt-dlp handles whatever the page looks like. There the host is the
// answer the baseline gave, and it is given again, in one of two ways:
//
//   - claimFirst: the extractor takes the link before any probe. For hosts
//     whose extractor reads an API or hands the link to yt-dlp, the page is
//     never read, so probing it only costs a request and a point of
//     failure. claimFirstForm is the same for the link's form alone, the
//     host's other pages (news articles, about pages) left to recognition.
//   - claimFallback: recognition goes first, and the extractor takes the
//     link when nothing recognised the page or the probe failed. For hosts
//     whose extractor reads the page: the page's own markers are still the
//     better answer, but a page that has none is the site's own, and the
//     site's extractor is the one to say why (removed, private, expired),
//     not the generic fallback, which would read the page's related links
//     as the item.
//
// The hosts are the lists extractors were matched by before recognition
// (siteHosts), and each platform family's known installs plus
// HEAPLEACH_EXTRA_HOSTS (familyHosts).
type claimMode int

const (
	claimNone claimMode = iota
	claimFirst
	claimFirstForm
	claimFallback
	// claimFirstFormThenLast is claimFirstForm for the extractor's own
	// forms; the host's other pages go through recognition and the
	// generic fallback, and only one the fallback finds nothing on
	// (errPlainPage) is the extractor's (claimLast), to say why.
	claimFirstFormThenLast
	claimLast
)

// claimModes are the extractors that take their hosts' links before a
// probe; every other extractor with hosts takes them as a fallback.
var claimModes = map[string]claimMode{
	// yt-dlp handoffs: yt-dlp reads the page, whatever it looks like.
	"youtube": claimFirst, "odysee": claimFirst, "dailymotion": claimFirst, "bilibili": claimFirst,
	"soundcloud": claimFirst, "mixcloud": claimFirst, "rumble": claimFirst, "niconico": claimFirst,
	// File hosts read through their APIs or their own fetches.
	"gofile": claimFirst, "mega": claimFirst, "pixeldrain": claimFirst, "keep2share": claimFirst,
	"emload": claimFirst, "turbo": claimFirst, "dropbox": claimFirst, "drive": claimFirst,
	"wetransfer": claimFirst, "cyberdrop": claimFirst, "filester": claimFirst, "imagepond": claimFirst,
	"filejump": claimFirst,
	// Media sites read through their APIs.
	"redgifs": claimFirst, "streamable": claimFirst, "vimeo": claimFirst, "bitchute": claimFirst,
	"imgur": claimFirst, "civitai": claimFirst, "archive.org": claimFirst, "4chan": claimFirst,
	// Broadcasters that resolve a programme from the link's id; their other
	// pages are left to recognition.
	// A news article with a clip is still the generic fallback's; a page
	// it finds nothing on is the broadcaster's to explain.
	"svtplay": claimFirstFormThenLast, "nrk": claimFirstFormThenLast, "drtv": claimFirstFormThenLast,
	"ruv": claimFirstFormThenLast, "rtve": claimFirstFormThenLast, "npo": claimFirstFormThenLast,
	"vrtmax": claimFirstFormThenLast, "bbcsounds": claimFirstFormThenLast, "raiplay": claimFirstFormThenLast,
	"rtbf": claimFirstFormThenLast, "arte": claimFirstFormThenLast, "zdf": claimFirstFormThenLast,
	"francetv": claimFirstFormThenLast, "rtpplay": claimFirstFormThenLast,
	// Platform families read through their APIs, on their known installs.
	"peertube": claimFirstForm, "foolfuuka": claimFirstForm, "booru": claimFirstForm,
	"mediawiki": claimFirstForm, "kemono": claimFirstForm, "fediverse": claimFirstForm,
}

// claimsFiles are the extractors that take a file-named link on their
// hosts: a direct download link they resolve (a confirmation step, a
// rotation of mirrors) or ask for as the site's own pages do (ImagePond's
// media host, asked with the site's Referer), rather than a file served as
// it stands.
var claimsFiles = map[string]bool{"dropbox": true, "drive": true, "imagepond": true}

// familyInstalls are the installs each platform family ships knowing, and
// the label a job on each is shown with ("" for the host's first label).
// HEAPLEACH_EXTRA_HOSTS adds to them (config.ExtraHostsFor). A family's
// installs are claimed as their family's mode says, and labelled by
// install (Registry.JobLabel).
var familyInstalls = map[string]map[string]string{
	config.FamilyKVS: {
		"thisvid.com": "", "pornzog.com": "", "camwhores.tv": "", "sexvid.xxx": "", "porntrex.com": "",
		"pornhits.com": "", "fapster.xyz": "", "xxthots.com": "", "moannest.com": "",
	},
	config.FamilyPeerTube: {
		"framatube.org": "", "tilvids.com": "", "video.blender.org": "", "makertube.net": "",
		"tube.tchncs.de": "", "kolektiva.media": "", "spectra.video": "",
	},
	config.FamilyChevereto: {
		"ibb.co": "imgbb", "imgbb.com": "imgbb", "freeimage.host": "freeimage", "gifyu.com": "gifyu",
	},
	config.FamilyFoolFuuka: {
		"desuarchive.org": "desuarchive", "rbt.asia": "desuarchive", "archive.palanq.win": "palanq",
	},
	config.FamilyBooru: {
		"aibooru.online": "", "booru.borvar.art": "", "booru.foalcon.com": "", "booru.org": "",
		"derpibooru.org": "", "e621.net": "", "e6ai.net": "", "e926.net": "", "furbooru.org": "",
		"hypnohub.net": "", "konachan.com": "", "konachan.net": "", "ponybooru.org": "",
		"safebooru.org": "", "sakugabooru.com": "", "snootbooru.com": "", "tbib.org": "",
		"twibooru.org": "", "xbooru.com": "", "yande.re": "",
	},
	config.FamilyMediaWiki: {
		"wikipedia.org": "", "wikimedia.org": "", "wiktionary.org": "", "wikiquote.org": "",
		"wikibooks.org": "", "wikisource.org": "", "wikinews.org": "", "wikiversity.org": "",
		"wikivoyage.org": "", "wikidata.org": "", "wikifunctions.org": "", "mediawiki.org": "",
	},
	config.FamilyKemono: {
		"kemono.cr": "", "kemono.su": "", "kemono.st": "", "kemono.party": "",
		"coomer.st": "", "coomer.su": "", "coomer.party": "",
	},
	config.FamilyBandzoogle: {},
	// Fediverse servers are too many to list; only HEAPLEACH_EXTRA_HOSTS
	// names any.
	config.FamilyFediverse: {},
}

// familyHosts are each family's installs for one configuration: the built-in
// ones and the configured extras.
func familyHosts(cfg *config.Config) map[string][]string {
	out := make(map[string][]string, len(familyInstalls))
	for family, installs := range familyInstalls {
		for host := range installs {
			out[family] = append(out[family], host)
		}
		out[family] = append(out[family], cfg.ExtraHostsFor(family)...)
	}
	return out
}

// claimHosts are the hosts e takes links on by their host, and how.
func (r *Registry) claimHosts(e Extractor) ([]string, claimMode) {
	name := e.Name()
	hosts, ok := siteHosts[name]
	if !ok {
		hosts, ok = r.families[name]
	}
	hosts = append(hosts[:len(hosts):len(hosts)], r.claimAliases[name]...)
	if !ok && len(hosts) == 0 {
		return nil, claimNone
	}
	if mode, listed := claimModes[name]; listed {
		return hosts, mode
	}
	return hosts, claimFallback
}

// hostClaim is the extractor whose hosts the link is on, claiming it in one
// of the modes asked for, or nil. A link on several extractors' hosts goes
// to the first in registration order.
func (r *Registry) hostClaim(u *url.URL, modes ...claimMode) Extractor {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil
	}
	for _, e := range r.extractors {
		hosts, mode := r.claimHosts(e)
		if mode == claimNone || !onSiteHosts(u, hosts) {
			continue
		}
		switch {
		case mode == claimFirstFormThenLast && formClaims(e, u):
			mode = claimFirstForm
		case mode == claimFirstFormThenLast:
			mode = claimLast
		case mode == claimFirstForm && !formClaims(e, u):
			// The host's other pages: recognition, then the fallback.
			mode = claimNone
		case !probeWorthy(u) && !formClaims(e, u) && !claimsFiles[e.Name()]:
			// A file on the host (an image or media subdomain) is the
			// file, not a page of the site's.
			mode = claimNone
		}
		for _, want := range modes {
			if mode == want {
				return e
			}
		}
		return nil
	}
	return nil
}

// installLabel is the label of the family install u is on, or "".
func (r *Registry) installLabel(family string, host string) string {
	host = strings.TrimPrefix(strings.ToLower(strings.TrimSuffix(host, ".")), "www.")
	for install, label := range familyInstalls[family] {
		if host == install || strings.HasSuffix(host, "."+install) {
			return cmp.Or(label, hostLabel(host))
		}
	}
	for _, install := range r.families[family] {
		if host == install || strings.HasSuffix(host, "."+install) {
			return hostLabel(host)
		}
	}
	return ""
}
