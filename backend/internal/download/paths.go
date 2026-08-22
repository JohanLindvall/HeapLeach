package download

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxNameBytes keeps a component inside the 255-byte limit every common
// filesystem shares, with room for the ".<id>.part" suffix.
const maxNameBytes = 200

// SafeName reduces an untrusted filename to a single, portable path
// component. Separators, control characters and the shell-hostile set are
// replaced rather than dropped so distinct names stay distinct.
func SafeName(name string) string {
	name = strings.TrimSpace(name)

	// A remote name is never allowed to climb out of the download root.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}

	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, r == 0x7f:
			// Control characters: drop.
		case strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteRune('_')
		case r == utf8.RuneError:
			b.WriteRune('_')
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	name = strings.Join(strings.Fields(b.String()), " ")

	// Leading dots hide files; trailing dots and spaces confuse Windows.
	name = strings.TrimRight(name, ". ")
	name = strings.TrimLeft(name, ".")

	if name == "" || isReservedName(name) {
		name = "download"
	}
	return truncateName(name, maxNameBytes)
}

// SafeRelPath sanitises each component of a relative directory path and
// rejects any attempt to escape upwards.
func SafeRelPath(p string) string {
	var parts []string
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == "." || seg == ".." {
			continue
		}
		if s := SafeName(seg); s != "" {
			parts = append(parts, s)
		}
	}
	return filepath.Join(parts...)
}

// truncateName shortens a name to n bytes while keeping its extension and
// never splitting a UTF-8 rune.
func truncateName(name string, n int) string {
	if len(name) <= n {
		return name
	}
	ext := filepath.Ext(name)
	if len(ext) > 16 { // not really an extension
		ext = ""
	}
	keep := n - len(ext)
	if keep < 1 {
		return trimToValidUTF8(name, n)
	}
	return trimToValidUTF8(name[:len(name)-len(ext)], keep) + ext
}

func trimToValidUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return strings.TrimRight(s, ". ")
}

// isReservedName reports the Windows device names, which cannot be files.
func isReservedName(name string) bool {
	base := strings.ToUpper(name)
	if i := strings.Index(base, "."); i > 0 {
		base = base[:i]
	}
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) &&
		base[3] >= '1' && base[3] <= '9' {
		return true
	}
	return false
}

// UniquePath returns a path inside dir that does not collide with an
// existing file, appending " (2)", " (3)" and so on.
func UniquePath(dir, name string) (string, error) {
	candidate := filepath.Join(dir, name)
	if _, err := os.Lstat(candidate); os.IsNotExist(err) {
		return candidate, nil
	}

	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; i < 10000; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find a free filename for %q in %s", name, dir)
}

// newID returns a short random identifier for a job or item.
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot fail in practice; a panic here beats handing
		// out colliding ids.
		panic("download: read random: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// partSuffix derives the ".<id>.part" discriminator for a destination file.
//
// It hashes the source URL rather than using the item id, which is random
// per process. Two jobs writing the same filename concurrently still get
// different part files — the guarantee the item id used to provide — but the
// same URL maps to the same part file on every run, which is what makes an
// interrupted transfer resumable rather than merely restartable.
func partSuffix(sourceURL string) string {
	sum := sha256.Sum256([]byte(sourceURL))
	return hex.EncodeToString(sum[:8])
}

// claimPart reserves a part path for the caller. The second return value
// releases it. When the path is already in use — the same URL queued twice —
// ok is false and the caller falls back to a private name.
func (m *Manager) claimPart(path string) (release func(), ok bool) {
	m.partsMu.Lock()
	defer m.partsMu.Unlock()
	if _, taken := m.parts[path]; taken {
		return nil, false
	}
	m.parts[path] = struct{}{}
	return func() {
		m.partsMu.Lock()
		delete(m.parts, path)
		m.partsMu.Unlock()
	}, true
}
