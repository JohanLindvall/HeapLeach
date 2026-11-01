// SPDX-License-Identifier: MIT

package extractor

import (
	"slices"
	"strings"
)

// Job labels for platform families (Registry.JobLabel).
//
// A family's extractor is one recogniser for every install, and names
// itself by the software in its errors. The job list is another matter:
// each install was its own site there while installs were listed one by
// one, and a column of "kvs" says nothing about where a job came from. So
// a job one of these read is labelled by its install's host. The label
// never names a folder (the result's title does), so nothing on disk
// depends on it.

// installName labels a Kernel Video Sharing install by its host's first
// label, as the listed installs were named.
func (k *KVS) installName(host string) string { return hostLabel(host) }

// installName labels a PeerTube instance by the rightmost label of its
// host that names it, passing over the public suffix and the words
// instances use to say they are a video site (tube.example.org is
// "example"); a host of nothing but such words keeps its first label.
func (p *PeerTube) installName(host string) string {
	host = strings.TrimPrefix(strings.ToLower(strings.TrimSuffix(host, ".")), "www.")
	if host == "" {
		return ""
	}
	labels := strings.Split(host, ".")
	if len(labels) > 1 {
		labels = labels[:len(labels)-1]
	}
	for _, label := range slices.Backward(labels) {
		if !peerTubeGenericLabels[label] {
			return label
		}
	}
	return labels[0]
}

// peerTubeGenericLabels are the words an instance's hostname uses to say it
// is a video site rather than to name itself, plus the second-level labels
// that are really part of a public suffix.
var peerTubeGenericLabels = map[string]bool{
	"video": true, "videos": true, "tube": true, "peertube": true,
	"media": true, "watch": true, "stream": true, "tv": true,
	"co": true, "com": true, "org": true, "net": true,
}
