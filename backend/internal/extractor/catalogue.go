package extractor

import (
	"slices"
	"strings"
)

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
		slices.Sort(info.Sites)
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b HostInfo) int { return strings.Compare(a.Name, b.Name) })
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

// Sites lists the instances of the software this covers.
func (k *Kemono) Sites() []string {
	out := make([]string, 0, len(kemonoHosts))
	for domain := range kemonoHosts {
		out = append(out, domain)
	}
	return out
}

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

// The rest claim something a plain host list cannot express — a section of a
// domain, a wildcard TLD, an exact host without its subdomains, a domain and
// a path shape — so each keeps a Match of its own and states here what that
// Match amounts to. Everything that claims a plain list embeds a hostSet
// instead, and needs no line here: see hosts.go.
func (b *Bunkr) Sites() []string { return []string{"bunkr.*"} }
func (g *GoogleDrive) Sites() []string {
	return []string{"drive.google.com", "drive.usercontent.google.com", "docs.google.com"}
}
func (s *SRF) Sites() []string      { return []string{"srf.ch", "playsuisse.ch"} }
func (v *VRTMax) Sites() []string   { return []string{"vrt.be"} }
func (n *NPOStart) Sites() []string { return []string{"npo.nl", "npostart.nl"} }
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
func (y *Yandex) Sites() []string    { return []string{"yandex.*"} }
func (x *XHamster) Sites() []string  { return []string{"xhamster.*"} }

// AlohaTube hosts nothing: it is listed because a link to it resolves,
// not because its videos live there.
func (i *Imgur) Sites() []string      { return []string{"imgur.com"} }
func (c *Civitai) Sites() []string    { return []string{"civitai.com"} }
func (s *Streamable) Sites() []string { return []string{"streamable.com"} }
func (w *WeTransfer) Sites() []string { return []string{"wetransfer.com", "we.tl"} }
func (p *PornPics) Sites() []string   { return []string{"pornpics.com"} }
func (a *ArchiveOrg) Sites() []string { return []string{"archive.org"} }
