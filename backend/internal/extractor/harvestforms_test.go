// SPDX-License-Identifier: MIT

package extractor

import (
	"slices"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Explicit harvesting admits established item routes on a known install,
// while its listings and account pages remain navigation. Exercise the
// actual registry so another family's overlapping Shape cannot silently
// turn a rejected route back into an accepted source.
func TestExplicitHarvestItemForms(t *testing.T) {
	for _, row := range []struct {
		name       string
		items      []string
		navigation []string
	}{
		{
			name: config.FamilyKVS,
			items: []string{
				"/videos/first-clip/", "/video/123/second-clip/", "/en/videos/third-clip/",
			},
			navigation: []string{
				"/videos/", "/categories/synthetic/", "/members/123/", "/members/123/videos/2/",
				"/search/synthetic/", "/search/videos/first-clip/", "/search/?q=synthetic",
				"/first-clip.html", "/videos/first-clip.mp4",
			},
		},
		{
			name: config.FamilyBooru,
			items: []string{
				"/posts/123", "/post/123", "/post/show/123/synthetic_tag", "/images/123",
				"/index.php?page=post&s=view&id=123", "/?page=post&s=view&id=123",
			},
			navigation: []string{
				"/posts", "/posts?tags=synthetic", "/post?tags=synthetic", "/search?q=synthetic",
				"/?tags=synthetic", "/index.php?page=post&s=list&tags=synthetic",
				"/posts/login", "/post/upload", "/images/random", "/post/show/new",
				"/index.php?page=post&s=view&id=upload",
			},
		},
		{
			name: "4chan",
			items: []string{
				"/a/thread/123", "/a/thread/123/synthetic-topic#p456",
			},
			navigation: []string{"/a/", "/a/catalog", "/a/archive", "/a/thread/new"},
		},
		{
			name: config.FamilyKemono,
			items: []string{
				"/synthetic/user/123/post/456", "/synthetic/user/123/post/456/revision/2",
			},
			navigation: []string{
				"/synthetic/user/123", "/synthetic/user/123/community", "/synthetic/user/123/fancards",
				"/synthetic/user/123/post", "/synthetic/user/123/post/456/comments",
			},
		},
		{
			name: config.FamilyFoolFuuka,
			items: []string{
				"/a/thread/123/", "/a/post/456/", "/a/thread/123/#456",
			},
			navigation: []string{
				"/a/", "/a/page/2/", "/a/search/text/synthetic/", "/a/post/login", "/a/thread/new",
			},
		},
		{
			name: config.FamilyPeerTube,
			items: []string{
				"/w/syntheticVideo", "/videos/watch/syntheticVideo", "/videos/embed/syntheticVideo",
				"/w/syntheticVideo;threadId=123",
			},
			navigation: []string{
				"/w/", "/w/p/syntheticPlaylist", "/videos/watch/playlist/syntheticPlaylist",
				"/video-playlists/syntheticPlaylist", "/video-playlists/embed/syntheticPlaylist",
				"/c/synthetic", "/video-channels/synthetic/videos", "/a/synthetic", "/accounts/synthetic",
			},
		},
		{
			name: config.FamilyChevereto,
			items: []string{
				"/i/AbCd12", "/img/AbCd12", "/image/First-Picture.AbCd12", "/photo/AbCd12",
			},
			navigation: []string{
				"/a/AbCd12", "/album/AbCd12", "/albums/AbCd12", "/synthetic/albums", "/AbCd12",
				"/image/", "/image/AbCd12/edit", "/image/first-picture.jpg", "/image/help.html",
			},
		},
		{
			name: config.FamilyFediverse,
			items: []string{
				"/@synthetic/123", "/web/@synthetic/123", "/deck/@synthetic/123",
				"/notes/abc123", "/notice/abc123", "/post/123", "/p/synthetic/123",
			},
			navigation: []string{
				"/@synthetic", "/web/@synthetic", "/users/synthetic", "/u/synthetic",
				"/c/synthetic", "/post/new", "/p/synthetic", "/settings/profile",
			},
		},
		{
			name: config.FamilyMediaWiki,
			items: []string{
				"/wiki/File:First_Picture.jpg", "/wiki/Image:Second_Picture.png",
				"/w/index.php?title=File:Third_Picture.jpg", "/index.php/File:Fourth_Picture.jpg",
				"/w/index.php?title=File:Fifth_Picture.jpg&action=view",
			},
			navigation: []string{
				"/wiki/Main_Page", "/wiki/Synthetic_Article", "/wiki/Category:Synthetic",
				"/wiki/Special:ListFiles", "/wiki/Special:UserLogin", "/wiki/User:Synthetic",
				"/w/index.php?title=Category:Synthetic", "/File:First_Picture.jpg",
				"/w/index.php?title=File:First_Picture.jpg&action=edit",
				"/wiki/File:First_Picture.jpg?action=history", "/wiki/File:First_Picture.jpg?action=raw",
			},
		},
		{
			// This platform recognises tracks in a document, without a URL
			// form that distinguishes a track page from other navigation.
			name:       config.FamilyBandzoogle,
			navigation: []string{"/music", "/music/first-song", "/track/123", "/profile"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			const host = "harvest.example.test"
			const base = "https://" + host
			cfg := &config.Config{}
			if row.name != "4chan" {
				cfg.ExtraHosts = map[string][]string{row.name: {host}}
			}
			reg := NewRegistry(cfg, nil)
			if row.name == "4chan" {
				// A single-site extractor has no configurable family installs.
				aliasClaims(reg, row.name, host)
			}
			from := mustParse(t, base+"/collection")
			self := NewLinks(nil, reg)
			var candidates, want []string
			for _, path := range append([]string{"/", "/login", "/account"}, row.navigation...) {
				candidates = append(candidates, base+path)
			}
			for _, path := range row.items {
				link := base + path
				candidates = append(candidates, link)
				want = append(want, link)
				if ex, ok := reg.Follows(mustParse(t, link), from); ok {
					t.Errorf("ordinary Follows(%q) selected %s", path, ex.Name())
				}
			}
			if got := supportedSources(reg, candidates, self, from); !slices.Equal(got, want) {
				t.Errorf("explicit sources = %q, want only items %q", got, want)
			}
			if got := fileLinkSources(reg, candidates, from); len(got) != 0 {
				t.Errorf("automatic scan admitted same-site platform routes: %q", got)
			}
			if got := supportedSources(reg, candidates, NewDirect(nil), from); len(got) != 0 {
				t.Errorf("an ordinary collection admitted same-site platform routes: %q", got)
			}

			// The identical forms on an unconfigured host are not evidence
			// that it runs this platform, even in an explicit harvest.
			for i := range candidates {
				candidates[i] = strings.Replace(candidates[i], host, "unknown.example.test", 1)
			}
			unknown := mustParse(t, "https://unknown.example.test/collection")
			if got := supportedSources(reg, candidates, self, unknown); len(got) != 0 {
				t.Errorf("unconfigured host admitted same-site routes: %q", got)
			}
		})
	}
}

func TestExplicitHarvestKnownHostBoundaries(t *testing.T) {
	for _, aliases := range []string{"configured", "claim", "discovery", "wrong-family", "unknown"} {
		t.Run(aliases, func(t *testing.T) {
			const host = "known.example.test"
			cfg := &config.Config{}
			if aliases == "configured" {
				cfg.ExtraHosts = map[string][]string{config.FamilyKVS: {host}}
			}
			if aliases == "wrong-family" {
				cfg.ExtraHosts = map[string][]string{config.FamilyBooru: {host}}
			}
			reg := NewRegistry(cfg, nil)
			if aliases == "claim" {
				aliasClaims(reg, config.FamilyKVS, host)
			}
			if aliases == "discovery" {
				reg.AliasHosts(config.FamilyKVS, host)
			}
			for _, servedHost := range []string{host, "www." + host, "sub." + host, host + ".unrelated.test"} {
				base := "https://" + servedHost
				from := mustParse(t, base+"/collection")
				link := base + "/videos/first-clip/"
				candidates := []string{base + "/login", link, base + "/members/123/"}
				var want []string
				if aliases != "wrong-family" && aliases != "unknown" && servedHost != host+".unrelated.test" {
					want = []string{link}
				}
				if got := supportedSources(reg, candidates, NewLinks(nil, reg), from); !slices.Equal(got, want) {
					t.Errorf("sources on %s = %q, want %q", servedHost, got, want)
				}
				if got := fileLinkSources(reg, candidates, from); len(got) != 0 {
					t.Errorf("automatic sources on %s = %q", servedHost, got)
				}
			}
		})
	}
}

func TestExplicitHarvestPreservesCrossSiteDiscovery(t *testing.T) {
	reg := NewRegistry(&config.Config{}, nil)
	from := mustParse(t, "https://collection.example.test/index")
	// Ordinary cross-site discovery already recognises these platform forms
	// without a configured install, including a platform's listing route.
	candidates := []string{
		"https://clips.example.test/videos/first-clip/",
		"https://clips.example.test/members/123/",
	}
	ordinary := fileLinkSources(reg, candidates, from)
	if !slices.Equal(ordinary, candidates) {
		t.Fatalf("ordinary cross-site sources = %q, want %q", ordinary, candidates)
	}
	if got := supportedSources(reg, candidates, NewLinks(nil, reg), from); !slices.Equal(got, ordinary) {
		t.Errorf("explicit cross-site sources = %q, want unchanged %q", got, ordinary)
	}
}
