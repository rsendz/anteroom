package httpserver

import (
	"io"
	"log/slog"
	"slices"
	"testing"
	"testing/fstest"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func manifestFS(t *testing.T, body string) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{"web/static/manifest.json": &fstest.MapFile{Data: []byte(body)}}
}

func TestLoadAssetsResolvesEntriesThroughTheManifest(t *testing.T) {
	// Shaped like a real Vite manifest: keyed by source path, with the hashed
	// output and its stylesheet alongside.
	a := loadAssets(manifestFS(t, `{
	  "src/queue/main.ts":  {"file": "assets/queue-abc123.js", "name": "queue", "isEntry": true, "css": ["assets/queue-def456.css"]},
	  "src/admin/main.tsx": {"file": "assets/admin-ghi789.js", "name": "admin", "isEntry": true, "css": ["assets/admin-jkl012.css"]}
	}`), quietLogger())

	want := Prefix + "static/assets/queue-abc123.js"
	if got := a.scriptsFor("queue"); !slices.Equal(got, []string{want}) {
		t.Errorf("scriptsFor(queue) = %v, want [%s]", got, want)
	}
	wantCSS := Prefix + "static/assets/queue-def456.css"
	if got := a.stylesFor("queue"); !slices.Equal(got, []string{wantCSS}) {
		t.Errorf("stylesFor(queue) = %v, want [%s]", got, wantCSS)
	}
	if !a.built("queue") || !a.built("admin") {
		t.Error("both entries are in the manifest, so both should report as built")
	}
}

// The fallback this guards is what makes `go build ./cmd/anteroom` useful with
// no Node toolchain present: the waiting page still renders, without a bundle.
func TestLoadAssetsWithoutAManifestFallsBackQuietly(t *testing.T) {
	a := loadAssets(fstest.MapFS{}, quietLogger())

	if a.built("queue") {
		t.Error("queue reported as built with no manifest at all")
	}
	if got := a.scriptsFor("queue"); got != nil {
		t.Errorf("scriptsFor(queue) = %v, want nil", got)
	}
	if got := a.stylesFor("queue"); got != nil {
		t.Errorf("stylesFor(queue) = %v, want nil", got)
	}
}

func TestLoadAssetsWithAnUnreadableManifestFallsBack(t *testing.T) {
	// A truncated manifest must degrade the same way a missing one does,
	// rather than taking the server down at startup.
	a := loadAssets(manifestFS(t, `{"src/queue/main.ts": {`), quietLogger())

	if a.built("queue") {
		t.Error("queue reported as built from an unparseable manifest")
	}
}

func TestLoadAssetsIgnoresNonEntryChunks(t *testing.T) {
	// Shared chunks appear in the manifest but must never be injected as
	// script tags; only entry points are.
	a := loadAssets(manifestFS(t, `{
	  "src/queue/main.ts": {"file": "assets/queue-abc123.js", "name": "queue", "isEntry": true},
	  "_shared-xyz.js":    {"file": "assets/shared-xyz.js", "name": "shared", "isEntry": false}
	}`), quietLogger())

	if a.built("shared") {
		t.Error("a non-entry chunk was treated as an entry point")
	}
	if !a.built("queue") {
		t.Error("the real entry point was skipped")
	}
}
