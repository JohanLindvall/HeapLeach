// SPDX-License-Identifier: MIT

package extractor

import (
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// What the link scanner follows, beside each extractor's Shape.
//
// Shape is a form any site's link may have, and it is what decides that a
// link is worth a probe at all; recognition then reads the page. Some links
// that plainly lead to a supported item have no such form: a short link
// (/<code> on a host that only redirects), or a site's canonical item link,
// /<id>, which on any other host is navigation. A Shape naming those would
// probe every bare link on every page. Found on a page, they were followed
// all the same while extractors were chosen by host, and a thread of them
// is a common thing to harvest.
//
// linkForms names them, by the host that hands them out, for discovery
// alone, and only for a link found on another site's page: Registry.Follows
// follows them, and their pages are still recognised
// by what they serve, never because of the host. No Shape or Recognise reads
// anything here (TestDiscoveryFormsNeverDecideRecognition), so a host named
// below is an operational constant like an API base, not a reason any
// extractor is chosen.
//
// A form some platform's software uses for every page of its own — a
// board's /posts/<n>, a site's /members/<n>/ — is the opposite case: a
// Shape, since such a link on another site leads to that platform, but on
// the site the page itself is on it is the site's own navigation, and
// following it costs a probe and platform API guesses per link. local
// names those forms, followed only from another site's page by automatic
// scanners. harvest admits their item routes on known installs when the
// user explicitly asks for links:.
type linkForm struct {
	// follow is the links followed besides the extractor's Shape.
	follow func(*url.URL) bool
	// local is the part of the Shape a site's own pages use for
	// navigation.
	local func(*url.URL) bool
	// harvest singles out item routes that explicit links: may follow
	// on a known install, despite local suppression. Listings, profiles
	// and account pages are not items.
	harvest func(*url.URL) bool
}

// siteHosts are the hosts of each extractor that serves one site, keyed by
// extractor name. The link scanner follows such an extractor's links only
// on these hosts (Registry.Follows): its Shape names paths (/v/<id>, /e/<id>,
// /watch/...) that every other site uses for its own pages, and following
// them wherever they appear probes a page's whole navigation. On the site's
// own hosts, a link found on another site's page is followed whatever its
// path, as it was when extractors were chosen by host: a band's front page,
// a profile, a programme. Recognition never reads this table; it decides
// only what discovery follows.
//
// A domain takes its subdomains. "name.*" is the name under any top-level
// domain, one label or two ("bunkr.cr", "bunkr.co.uk"); "name*.*" also
// accepts labels beginning with name, for a service's rotating aliases.
// "host/path" is the host's pages under path alone. An extractor not listed
// here is a platform or a capability (a booru, a KVS install, the archive
// software two of whose installs host creators' posts, a feed), whose forms
// stay valid on any host. A listed extractor's form whose id alone names the
// service (offHostForms) is followed on any host as well.
var siteHosts = map[string][]string{
	"gofile":         {"gofile.io"},
	"mega":           {"mega.nz", "mega.co.nz"},
	"bunkr":          {"bunkr*.*"},
	"erome":          {"erome.com"},
	"rajce":          {"rajce.idnes.cz", "rajce.net"},
	"pixeldrain":     {"pixeldrain.com", "nova.storage"},
	"keep2share":     {"k2s.cc", "keep2share.cc", "fboom.me"},
	"filejump":       {"filejump.com"},
	"emload":         {"emload.com"},
	"turbo":          {"turbo.cr", "turbocdn.st"},
	"dropbox":        {"dropbox.com", "dropboxusercontent.com"},
	"mediafire":      {"mediafire.com"},
	"drive":          {"drive.google.com", "docs.google.com", "drive.usercontent.google.com"},
	"svtplay":        {"svtplay.se"},
	"ruv":            {"ruv.is"},
	"ard":            {"ardmediathek.de"},
	"zdf":            {"zdf.de"},
	"srf":            {"srf.ch", "playsuisse.ch"},
	"arte":           {"arte.tv"},
	"orf":            {"orf.at"},
	"yle":            {"areena.yle.fi", "arenan.yle.fi"},
	"drtv":           {"dr.dk"},
	"rtbf":           {"rtbf.be"},
	"francetv":       {"france.tv"},
	"radio-canada":   {"radio-canada.ca"},
	"vrtmax":         {"vrt.be", "vrtmax.be"},
	"npo":            {"npo.nl", "npostart.nl"},
	"raiplay":        {"raiplay.it"},
	"rtve":           {"rtve.es"},
	"rtpplay":        {"rtp.pt"},
	"pbs":            {"pbs.org"},
	"npr":            {"npr.org"},
	"abclisten":      {"abc.net.au/listen"},
	"bbcsounds":      {"bbc.co.uk/sounds"},
	"vimeo":          {"vimeo.com"},
	"niconico":       {"nicovideo.jp", "nico.ms"},
	"yandex":         {"yandex.*"},
	"4chan":          {"4chan.org", "4channel.org"},
	"redgifs":        {"redgifs.com"},
	"pornhub":        {"pornhub.com"},
	"xhamster":       {"xhamster.*"},
	"tnaflix":        {"tnaflix.com"},
	"drtuber":        {"drtuber.com"},
	"pornone":        {"pornone.com"},
	"eporner":        {"eporner.com"},
	"pixhost":        {"pixhost.to"},
	"imagepond":      {"imagepond.net"},
	"suvobox":        {"suvobox.com"},
	"filester":       {"filester.*"},
	"bandcamp":       {"bandcamp.com"},
	"cyberdrop":      {"cyberdrop.cr"},
	"imgur":          {"imgur.com"},
	"civitai":        {"civitai.com"},
	"bitchute":       {"bitchute.com"},
	"streamable":     {"streamable.com"},
	"wetransfer":     {"wetransfer.com", "we.tl"},
	"vidmoly":        {"vidmoly.me", "vidmoly.to", "vidmoly.biz", "vidmoly.net"},
	"nrk":            {"tv.nrk.no"},
	"pornpics":       {"pornpics.com", "pornpics.de"},
	"archive.org":    {"archive.org"},
	"fapello":        {"fapello.com"},
	"coomerfans":     {"coomerfans.com"},
	"celeb":          {"celeb.st"},
	"cum":            {"cum.st"},
	"ok.ru":          {"ok.ru", "odnoklassniki.ru"},
	"streamtape":     {"streamtape.com", "streamtape.net", "streamtape.to", "streamtape.cc", "streamtape.site", "streamtape.xyz", "streamta.pe", "strcloud.link"},
	"doodstream":     {"doodstream.com", "dood.la", "dood.li", "dood.re", "dood.sh", "dood.so", "dood.to", "dood.watch", "dood.ws", "dood.yt", "d000d.com", "d0o0d.com", "do0od.com", "dooood.com", "ds2play.com", "playmogo.com", "vidply.com"},
	"mixdrop":        {"mixdrop.ag", "mixdrop.bz", "mixdrop.ch", "mixdrop.club", "mixdrop.co", "mixdrop.gl", "mixdrop.is", "mixdrop.my", "mixdrop.ps", "mixdrop.sx", "mixdrop.to", "mixdrp.co", "mixdrp.to", "mdbekjwqa.pw"},
	"lulustream":     {"lulustream.com", "luluvdo.com", "luluvido.com"},
	"vids.st":        {"vids.st"},
	"sendvid":        {"sendvid.com"},
	"knuthing":       {"knuthing.com"},
	"noodlemagazine": {"noodlemagazine.com"},
	"youtube":        {"youtube.com", "youtu.be", "youtube-nocookie.com"},
	"odysee":         {"odysee.com", "lbry.tv"},
	"dailymotion":    {"dailymotion.com", "dai.ly"},
	"bilibili":       {"bilibili.com", "b23.tv"},
	"soundcloud":     {"soundcloud.com"},
	"mixcloud":       {"mixcloud.com"},
	"rumble":         {"rumble.com"},
	"alohatube":      {"alohatube.com"},
	"balbums":        {"balbums.st"},
	"voyeurking":     {"voyeurking.com"},
}

// offHostForms are the forms of a single-site extractor that name their
// service by the id alone, whatever host carries them — a mirror domain, a
// custom domain, an embed — and are followed on any host: a content UUID, a
// key carried in the fragment, a hex file id, an embed route named for the
// service. Generic paths (/v/<id>, /e/<id>, /d/<code>, /watch/...) are not
// among them: every other site uses those for its own pages.
var offHostForms = map[string]func(*url.URL) bool{
	"gofile": pathShape(`^/d/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/?$`),
	"mega": func(u *url.URL) bool {
		return (megaPathShape(u) && u.Fragment != "") || megaFragmentShape.MatchString(u.Fragment)
	},
	"keep2share": pathShape(`^/file/[0-9a-f]{13,}(/[^/]*)?/?$`),
	"ok.ru":      pathShape(`^/videoembed/[0-9]{6,}/?$`),
}

// onSiteHosts reports whether u is on one of hosts, as siteHosts writes
// them.
func onSiteHosts(u *url.URL, hosts []string) bool {
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	for _, h := range hosts {
		domain, prefix, scoped := strings.Cut(h, "/")
		switch {
		case strings.HasSuffix(domain, ".*"):
			if anyTLD(host, strings.TrimSuffix(domain, ".*")) {
				return true
			}
		case util.HostMatches(host, domain):
			if !scoped || u.Path == "/"+prefix || strings.HasPrefix(u.Path, "/"+prefix+"/") {
				return true
			}
		}
	}
	return false
}

// anyTLD reports whether host is name, or a subdomain of it, under a
// top-level domain of one or two labels. A terminal * on name opts into
// label-prefix matching without changing exact-name patterns.
func anyTLD(host, name string) bool {
	name, prefix := strings.CutSuffix(name, "*")
	if name == "" {
		return false
	}
	labels := strings.Split(host, ".")
	for i, label := range labels {
		if label == name || (prefix && strings.HasPrefix(label, name)) {
			rest := len(labels) - i - 1
			if rest == 1 || rest == 2 {
				return true
			}
			if !prefix {
				return false
			}
			// A subdomain sharing the prefix must not hide a matching label
			// farther right with the required suffix depth.
		}
	}
	return false
}

// linkForms is keyed by extractor name.
var linkForms = map[string]linkForm{
	// Short links that only redirect to the item's page.
	"youtube":     {follow: hostLinks([]string{"youtu.be"}, `^/[A-Za-z0-9_-]{11}/?$`)},
	"dailymotion": {follow: hostLinks([]string{"dai.ly"}, `^/x?[0-9a-zA-Z]{5,}/?$`)},
	"bilibili":    {follow: hostLinks([]string{"b23.tv"}, `^/[0-9A-Za-z]{4,}/?$`)},
	"soundcloud": {follow: eitherShape(
		hostLinks([]string{"on.soundcloud.com"}, `^/[0-9A-Za-z]{4,}/?$`),
		// A track, private ones with their token included.
		exactHostLinks([]string{"soundcloud.com", "www.soundcloud.com", "m.soundcloud.com"},
			`^/[^/]+/[^/.]+(?:/s-[0-9A-Za-z]+)?/?$`),
	)},
	// A show, /<user>/<show>/.
	"mixcloud": {follow: exactHostLinks([]string{"mixcloud.com", "www.mixcloud.com", "m.mixcloud.com"},
		`^/[^/]+/[^/.]+/?$`)},
	// Canonical single-item links, /<id>.
	"streamable": {follow: exactHostLinks([]string{"streamable.com", "www.streamable.com"}, `^/[0-9a-z]{4,10}/?$`)},
	"sendvid":    {follow: exactHostLinks([]string{"sendvid.com", "www.sendvid.com"}, `^/[A-Za-z0-9]{6,12}/?$`)},
	"lulustream": {follow: hostLinks([]string{"lulustream.com", "luluvdo.com", "luluvido.com"}, `^/[0-9A-Za-z]{12}/?$`)},
	"vidmoly":    {follow: hostLinks([]string{"vidmoly.me", "vidmoly.to", "vidmoly.biz", "vidmoly.net"}, `^/[A-Za-z0-9]{5,}/?$`)},
	"imgur":      {follow: exactHostLinks([]string{"imgur.com", "www.imgur.com", "m.imgur.com"}, `^/[A-Za-z0-9]{5,8}/?$`)},
	"chevereto": {follow: exactHostLinks([]string{"ibb.co"}, `^/[A-Za-z0-9]{5,10}/?$`),
		local: everyLink, harvest: cheveretoHarvestItem},
	// A Kernel Video Sharing install that serves its videos at
	// /<slug>.html, which on any other site is an article.
	"kvs": {
		follow:  hostLinks([]string{"sexvid.xxx"}, `^/[^/]+\.html$`),
		local:   everyLink,
		harvest: kvsHarvestItem,
	},
	// A show's feed on a publisher's feeds host, /<show-id>.
	"feed": {follow: feedHost},
	// Legacy domains that redirect into the catalogue under its new path.
	"npo":    {follow: hostLinks([]string{"npostart.nl"}, `^/[^/]+/[^/]+`)},
	"vrtmax": {follow: hostLinks([]string{"vrtmax.be"}, `^/a-z/[^/]+`)},

	// Platform forms a site's own pages use for navigation.
	"booru":     {local: everyLink, harvest: booruHarvestItem},
	"4chan":     {local: everyLink, harvest: fourChanShape},
	"kemono":    {local: everyLink, harvest: kemonoHarvestItem},
	"foolfuuka": {local: everyLink, harvest: foolFuukaHarvestItem},
	// Every other platform's forms are as common in a site's own
	// navigation: a channel, a user, a status, a wiki article.
	"peertube":   {local: everyLink, harvest: peerTubeHarvestItem},
	"bandzoogle": {local: everyLink},
	"fediverse":  {local: everyLink, harvest: fediHarvestItem},
	"mediawiki":  {local: everyLink, harvest: mediaWikiHarvestItem},
}

// Explicit harvesting uses each platform's item parser rather than its
// broader Shape, which may also admit profiles, searches and listings.
func kvsHarvestItem(u *url.URL) bool {
	if _, member := kvsMemberPath(u); member {
		return false
	}
	if _, _, search := kvsSearchPath(u); search {
		return false
	}
	return kvsVideoPath(u)
}

func booruHarvestItem(u *url.URL) bool {
	for _, api := range booruCandidates(u) {
		_, id, _ := (&booruSite{api: api}).parseTarget(u)
		if allDigits(id) {
			return true
		}
	}
	return false
}

func kemonoHarvestItem(u *url.URL) bool {
	p := util.PathSegments(u)
	return kemonoShape(u) && len(p) >= 5 && p[3] == "post"
}

func foolFuukaHarvestItem(u *url.URL) bool {
	target, err := foolFuukaParse(u)
	return err == nil && (target.kind == foolFuukaThreadTarget || target.kind == foolFuukaPostTarget) && allDigits(target.num)
}

func peerTubeHarvestItem(u *url.URL) bool {
	return peerTubeParseRoute(u).Kind == peerTubeVideoPage
}

func cheveretoHarvestItem(u *url.URL) bool {
	p := util.PathSegments(u)
	if !cheveretoPagePath(u) || len(p) != 2 {
		return false
	}
	switch strings.ToLower(p[0]) {
	case "i", "img", "image", "photo":
		return true
	}
	return false
}

func fediHarvestItem(u *url.URL) bool {
	target := fediTargetOf(u)
	return target != nil && target.kind == fediPost
}

func mediaWikiHarvestItem(u *url.URL) bool {
	if action := u.Query().Get("action"); action != "" && action != "view" {
		return false
	}
	title, explicit := mediaWikiTitle(u)
	return explicit && mediaWikiKindOf(title) == mediaWikiFilePage
}

// everyLink is a local form covering the whole Shape.
func everyLink(*url.URL) bool { return true }

// hostLinks is a discovery form: a path pattern on the named hosts and
// their subdomains.
func hostLinks(hosts []string, patterns ...string) func(*url.URL) bool {
	path := pathShape(patterns...)
	return func(u *url.URL) bool {
		for _, host := range hosts {
			if util.HostMatches(u.Host, host) {
				return path(u)
			}
		}
		return false
	}
}

// exactHostLinks is hostLinks on exactly the named hosts, for a site
// whose subdomains serve something else (an API, a media store).
func exactHostLinks(hosts []string, patterns ...string) func(*url.URL) bool {
	path := pathShape(patterns...)
	return func(u *url.URL) bool {
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		for _, h := range hosts {
			if host == h {
				return path(u)
			}
		}
		return false
	}
}

// oneSite reports whether a and b are on one site, ignoring "www.".
func oneSite(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	trim := func(u *url.URL) string {
		return strings.TrimPrefix(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), "www.")
	}
	return trim(a) == trim(b)
}
