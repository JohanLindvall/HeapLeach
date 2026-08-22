package extractor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// TestGofileOrderedChildren pins the listing order: gofile's own ordering
// where it supplied one, and a name sort for anything it left out — so a
// folder renders the same way the website shows it, and deterministically
// either way.
func TestGofileOrderedChildren(t *testing.T) {
	content := &gofileContent{
		ChildrenIDs: []string{"id-b", "id-a", "id-b"}, // the repeat must not double
		Children: map[string]gofileContent{
			"id-a": {ID: "id-a", Name: "Alpha"},
			"id-b": {ID: "id-b", Name: "Beta"},
			// Not in ChildrenIDs at all: sorted by name after the ordered ones.
			"id-z": {ID: "id-z", Name: "aardvark"},
			"id-y": {ID: "id-y", Name: "Zebra"},
		},
	}
	var names []string
	for _, child := range content.orderedChildren() {
		names = append(names, child.Name)
	}
	want := "Beta Alpha aardvark Zebra"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
}

func TestGofileAccessError(t *testing.T) {
	password := &gofileContent{PasswordStatus: "passwordRequired"}
	if err := password.accessError(); !errors.Is(err, ErrPasswordRequired) {
		t.Errorf("a password gate reported %v", err)
	}
	// Some responses carry the password as a field rather than a status.
	flagged := &gofileContent{Password: json.RawMessage(`true`)}
	if err := flagged.accessError(); !errors.Is(err, ErrPasswordRequired) {
		t.Errorf("a password flag reported %v", err)
	}
	expired := &gofileContent{Expire: json.RawMessage(`1700000000`)}
	if err := expired.accessError(); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("an expired link reported %v", err)
	}
	if err := (&gofileContent{}).accessError(); err == nil {
		t.Error("an inaccessible node with no stated reason reported nothing")
	}
}

func TestGofileStatusError(t *testing.T) {
	g := &Gofile{}
	if err := g.statusError("error-passwordRequired"); !errors.Is(err, ErrPasswordRequired) {
		t.Errorf("passwordRequired mapped to %v", err)
	}
	if err := g.statusError("error-notFound"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("notFound mapped to %v", err)
	}
	if err := g.statusError("error-wrongToken"); !errors.Is(err, errGofileStaleToken) {
		t.Errorf("wrongToken mapped to %v", err)
	}
}

// TestGofileExplain covers the two refusals the API words as a status error:
// a rejected signature — which must start the cooldown, since gofile blocks
// an address that keeps signing badly — and a stale account token, which the
// caller retries with a fresh one.
func TestGofileExplain(t *testing.T) {
	g := &Gofile{}
	rejected := g.explain(&httpx.StatusError{Code: 401, Body: `{"status":"error-notPremium"}`})
	if rejected == nil || !strings.Contains(rejected.Error(), "HEAPLEACH_GOFILE_SECRET") {
		t.Errorf("a rejected signature explained itself as %v", rejected)
	}
	if g.rejectedAt.IsZero() {
		t.Error("a rejected signature did not start the cooldown")
	}

	if err := (&Gofile{}).explain(&httpx.StatusError{Code: 401, Body: `{"status":"error-wrongToken"}`}); !errors.Is(err, errGofileStaleToken) {
		t.Errorf("a stale token explained itself as %v", err)
	}
}
