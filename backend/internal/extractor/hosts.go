package extractor

import (
	"net/url"
	"slices"

	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// hostSet is the set of domains an extractor claims, declared once and
// answering both halves of the contract from that one list: Match, which the
// registry asks, and Sites, which the catalogue asks when it writes the
// README's inventory. Embedding one is what keeps the two from disagreeing —
// they used to be written out separately, and a reader had to hold each
// Sites against its Match to know the documentation was telling the truth.
//
// A domain claims itself and every subdomain, so "example.test" takes
// "www.example.test" and "cdn.example.test" with it. An extractor that claims
// a section of a domain, a wildcard TLD, or a document shape rather than a
// host keeps a Match of its own and states its Sites by hand; see
// catalogue.go.
type hostSet []string

// Match reports whether the URL belongs to one of the domains.
func (h hostSet) Match(u *url.URL) bool { return matchesHosts(u, h) }

// Sites lists the domains, copied so the catalogue may sort what it is given.
func (h hostSet) Sites() []string { return slices.Clone(h) }

// matchesHosts reports whether a URL belongs to any of a host list.
func matchesHosts(u *url.URL, hosts []string) bool {
	for _, host := range hosts {
		if util.HostMatches(u.Host, host) {
			return true
		}
	}
	return false
}
