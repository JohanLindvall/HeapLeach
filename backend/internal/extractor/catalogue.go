package extractor

import "sort"

// What the program supports, asked of the program rather than written down.
//
// The host table in README.md explains how a handful of interesting hosts
// resolve, and that is editorial: it is worth a person's judgement about what
// deserves explaining. The *inventory* is not. A list of every supported site
// maintained by hand is a list that is wrong within two commits, and wrong in
// the direction that matters — it under-reports, so people conclude a host is
// unsupported when it is not.
//
// So the registry answers the question itself, `cmd/hostlist` renders the
// answer as Markdown, and `make hosts` writes it into README.md between two
// markers. Adding a host to NewRegistry is then the only step; the
// documentation follows from the code, and CI fails if the two have drifted.

// SiteLister is the optional half of the contract. An extractor covering
// several domains implements it to say which — a family extractor's Name is
// the software ("chevereto", "booru"), and naming the software tells a reader
// nothing about whether their link is covered.
//
// An extractor that covers exactly the one host it is named for does not need
// this; its name is the answer.
type SiteLister interface {
	// Sites lists the domains this extractor claims. An empty return means
	// the extractor recognises a shape rather than a domain list, which is
	// reported as such rather than as covering nothing.
	Sites() []string
}

// HostInfo is one row of the catalogue.
type HostInfo struct {
	// Name is the label the UI shows.
	Name string
	// Sites are the domains, where the extractor could enumerate them.
	Sites []string
	// ByShape is true for an extractor that recognises a URL or document
	// shape instead of a fixed set of hosts, and so covers an open-ended
	// set — a fact worth stating rather than rendering as a blank cell.
	ByShape bool
}

// Catalogue reports every registered extractor and the sites it covers,
// ordered by name so the generated documentation has a stable diff.
func (r *Registry) Catalogue() []HostInfo {
	out := make([]HostInfo, 0, len(r.extractors))
	for _, e := range r.extractors {
		info := HostInfo{Name: e.Name()}
		lister, ok := e.(SiteLister)
		switch {
		case !ok:
			// No list to give: the name is the host.
			info.Sites = []string{e.Name()}
		default:
			info.Sites = lister.Sites()
			info.ByShape = len(info.Sites) == 0
		}
		sort.Strings(info.Sites)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Sites lists the boards each API family covers.
func (b *Booru) Sites() []string {
	var out []string
	for _, site := range booruSites {
		out = append(out, site.domains...)
	}
	return out
}

// Sites lists the instances known at build time. The set is open — an
// unlisted install is recognised by its own API — but naming the ones that
// ship is what a reader wants.
func (p *PeerTube) Sites() []string { return []string{p.host} }

// Sites names the one install this extractor was built for.
func (c *Chevereto) Sites() []string { return c.domains }

// Sites names the one archive this extractor was built for.
func (f *FoolFuuka) Sites() []string { return f.site.domains }

// Sites names the one KVS install this extractor was built for.
func (k *KVS) Sites() []string { return []string{k.host} }

// Sites lists the domains this host rotates through.
func (s *Streamtape) Sites() []string { return streamtapeHosts }

// Sites lists the domains this host rotates through.
func (d *DoodStream) Sites() []string { return doodHosts }

// Sites lists the domains this host rotates through.
func (m *MixDrop) Sites() []string { return mixdropHosts }

// Sites lists the domains this host rotates through.
func (v *Vidmoly) Sites() []string { return vidmolyHosts }

// Sites lists the instances of the software this covers.
func (k *Kemono) Sites() []string {
	out := make([]string, 0, len(kemonoHosts))
	for domain := range kemonoHosts {
		out = append(out, domain)
	}
	return out
}

// Sites names the platform this hands to an external downloader.
func (h *Handoff) Sites() []string { return h.site.domains }

// Sites returns nothing: the harvester claims no host of its own, and reaches
// whatever the page it is pointed at links to.
func (l *Links) Sites() []string { return nil }

// Sites returns nothing: any host may serve an adaptive manifest.
func (h *HLSDirect) Sites() []string { return nil }

// Sites returns nothing: an instance is recognised by the nodeinfo document
// it publishes, which every one of them does, rather than by being listed.
func (f *Fediverse) Sites() []string { return nil }

// Sites returns nothing: the software runs on a great many wikis and is
// recognised by its API rather than its domain.
func (m *MediaWiki) Sites() []string { return nil }

// Sites returns nothing: a directory listing is a shape, not a host.
func (a *Autoindex) Sites() []string { return nil }

// Sites returns nothing: any URL may turn out to be a feed.
func (f *Feeds) Sites() []string { return nil }

// The rest name their own domains. These are one-liners rather than a field
// on each extractor because the domains are already stated in each Match,
// and a second copy that could disagree with the first is worse than a
// method here that a reader can check against it in one glance.
func (g *Gofile) Sites() []string     { return []string{"gofile.io"} }
func (m *Mega) Sites() []string       { return []string{"mega.nz", "mega.co.nz"} }
func (b *Bunkr) Sites() []string      { return []string{"bunkr.*"} }
func (e *Erome) Sites() []string      { return []string{"erome.com"} }
func (p *Pixeldrain) Sites() []string { return []string{"pixeldrain.com", "nova.storage"} }
func (t *Turbo) Sites() []string      { return []string{"turbo.cr", "turbocdn.st"} }
func (d *Dropbox) Sites() []string    { return []string{"dropbox.com", "dropboxusercontent.com"} }
func (m *Mediafire) Sites() []string  { return []string{"mediafire.com"} }
func (g *GoogleDrive) Sites() []string {
	return []string{"drive.google.com", "drive.usercontent.google.com", "docs.google.com"}
}
func (s *SVTPlay) Sites() []string  { return []string{"svtplay.se"} }
func (r *RUV) Sites() []string      { return []string{"ruv.is"} }
func (a *ARD) Sites() []string      { return []string{"ardmediathek.de"} }
func (z *ZDF) Sites() []string      { return []string{"zdf.de"} }
func (s *SRF) Sites() []string      { return []string{"srf.ch", "playsuisse.ch"} }
func (v *VRTMax) Sites() []string   { return []string{"vrt.be"} }
func (n *NPOStart) Sites() []string { return []string{"npo.nl", "npostart.nl"} }
func (r *RaiPlay) Sites() []string  { return []string{"raiplay.it"} }
func (r *RTVE) Sites() []string     { return []string{"rtve.es"} }
func (r *RTPPlay) Sites() []string  { return []string{"rtp.pt"} }
func (p *PBS) Sites() []string      { return []string{"pbs.org"} }
func (n *NPR) Sites() []string      { return []string{"npr.org"} }

// These two claim one section of a very large domain rather than the domain,
// so the catalogue says which. A row reading "bbc.co.uk" would promise
// iPlayer and the news site along with it, and the generated list is read as
// a statement of what works.
func (a *ABCListen) Sites() []string { return []string{"abc.net.au/listen"} }
func (b *BBCSounds) Sites() []string { return []string{"bbc.co.uk/sounds"} }
func (y *YouTube) Sites() []string {
	return []string{"youtube.com", "youtu.be", "youtube-nocookie.com"}
}
func (v *Vimeo) Sites() []string    { return []string{"vimeo.com", "player.vimeo.com"} }
func (y *Yandex) Sites() []string   { return []string{"yandex.*"} }
func (f *FourChan) Sites() []string { return []string{"4chan.org", "4channel.org"} }
func (r *RedGifs) Sites() []string  { return []string{"redgifs.com"} }
func (p *PornHub) Sites() []string  { return []string{"pornhub.com"} }
func (x *XHamster) Sites() []string { return []string{"xhamster.*"} }
func (t *TNAFlix) Sites() []string  { return []string{"tnaflix.com"} }
func (p *PornOne) Sites() []string  { return []string{"pornone.com"} }
func (p *Pixhost) Sites() []string  { return []string{"pixhost.to"} }
func (i *ImagePond) Sites() []string {
	return []string{"imagepond.net"}
}
func (s *Suvobox) Sites() []string    { return []string{"suvobox.com"} }
func (f *Fapello) Sites() []string    { return []string{"fapello.com"} }
func (c *CoomerFans) Sites() []string { return []string{"coomerfans.com"} }
func (o *OKru) Sites() []string       { return []string{"ok.ru", "odnoklassniki.ru"} }
func (i *Imgur) Sites() []string      { return []string{"imgur.com"} }
func (c *Cyberdrop) Sites() []string  { return []string{"cyberdrop.cr"} }
func (c *Civitai) Sites() []string    { return []string{"civitai.com"} }
func (b *BitChute) Sites() []string   { return []string{"bitchute.com"} }
func (s *Streamable) Sites() []string { return []string{"streamable.com"} }
func (w *WeTransfer) Sites() []string { return []string{"wetransfer.com", "we.tl"} }
func (n *NRK) Sites() []string        { return []string{"nrk.no", "tv.nrk.no"} }
func (p *PornPics) Sites() []string   { return []string{"pornpics.com"} }
func (a *ArchiveOrg) Sites() []string { return []string{"archive.org"} }
