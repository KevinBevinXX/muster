package ui

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The snapshot store's tag carries the page's user and build, deferred, and is
// left out entirely without a signed-in user. Red before: no page drew it.
func TestTheSnapshotScriptCarriesTheUserAndTheBuild(t *testing.T) {
	if got := SnapScript(Features{SnapBuild: "b1"}); got != nil {
		t.Fatalf("no user: want no script, got %v", got)
	}
	if got := SnapScript(Features{SnapUser: 7}); got != nil {
		t.Fatalf("no build: want no script, got %v", got)
	}
	html := renderString(t, SnapScript(Features{SnapUser: 42, SnapBuild: "1.2.3"}))
	for _, re := range []string{
		`^<script src="/static/mirror-snap\.js\?v=[0-9a-f]{12}"`,
		` defer[ >]`,
		` data-snap-user="42"`,
		` data-snap-build="1\.2\.3"`,
	} {
		if !regexp.MustCompile(re).MatchString(html) {
			t.Errorf("%q does not match %s", html, re)
		}
	}
}

// The rendered view draws the store's tag from its navigation's features, and
// the "Updating…" chip in room it always holds: visibility hidden (the
// `invisible` utility), never display none. Red before: neither was drawn.
func TestTheRenderedViewDrawsTheSnapshotStoreAndAReservedChip(t *testing.T) {
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Nav: NavChrome{Feat: Features{SnapUser: 9, SnapBuild: "v9"}}}))
	if !strings.Contains(page, `data-snap-user="9"`) || !strings.Contains(page, `data-snap-build="v9"`) {
		t.Fatalf("the view page does not carry the snapshot store for its user and build")
	}
	chip := regexp.MustCompile(`<span id="mv-updating"[^>]*>Updating…</span>`).FindString(page)
	if chip == "" {
		t.Fatal("no Updating… chip")
	}
	if !regexp.MustCompile(`class="invisible absolute right-0 `).MatchString(chip) || strings.Contains(chip, " hidden") {
		t.Errorf("the chip must take no layout and keep its place while hidden (invisible, absolute, never `hidden`): %s", chip)
	}
	if !strings.Contains(page, `class="relative flex min-w-0 items-center gap-2"><p id="mv-status"`) {
		t.Error("the status row is not the chip's positioning box")
	}
	if bare := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main"})); strings.Contains(bare, `src="/static/mirror-snap.js`) {
		t.Error("a view rendered with no user carries the snapshot store")
	}
}

// The purge deletes the database the store writes: one name, pinned against
// the script's own spelling of it, so a rename cannot leave sign-out deleting
// a database nothing uses.
func TestTheSnapshotPurgeNamesTheStoresDatabase(t *testing.T) {
	src, err := os.ReadFile("../../web/static/mirror-snap.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "var DB = '"+SnapStoreDB+"';") {
		t.Fatalf("mirror-snap.js does not name its database %q", SnapStoreDB)
	}
	if got, want := SnapPurgeHTML(), `<script>try{indexedDB.deleteDatabase('`+SnapStoreDB+`')}catch(e){}</script>`; got != want {
		t.Fatalf("purge = %s, want %s", got, want)
	}
}
