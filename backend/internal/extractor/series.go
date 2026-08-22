package extractor

// Shared reasoning for the hosts that expand a listing into files grouped by
// a label — a broadcaster's seasons, a page's programme strands.

// nestByLabel reports whether a listing spans more than one distinct
// non-empty label, which is when filing its files into per-label folders
// earns its keep. With one label the folder would only restate the job's own
// name, and with none there is nothing to tell apart — so both of those read
// as "leave the files loose", and every host that nests makes the same call
// the same way.
func nestByLabel[T any](items []T, label func(T) string) bool {
	distinct := make(map[string]bool)
	for _, item := range items {
		if l := label(item); l != "" {
			distinct[l] = true
		}
	}
	return len(distinct) > 1
}
