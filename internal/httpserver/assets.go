package httpserver

import (
	"encoding/json"
	"io/fs"
	"log/slog"
)

// assets maps a front-end entry point to the hashed files Vite produced for
// it. Resolving through the manifest is what lets the static handler mark
// everything immutable: a changed file always has a changed name.
type assets struct {
	scripts map[string][]string
	styles  map[string][]string
}

// viteManifest is the subset of Vite's manifest.json that anteroom needs.
type viteManifest map[string]struct {
	File    string   `json:"file"`
	Name    string   `json:"name"`
	IsEntry bool     `json:"isEntry"`
	CSS     []string `json:"css"`
}

// loadAssets reads the build manifest. A missing manifest is not fatal: the
// server falls back to a plain waiting page that still shows the visitor's
// position and refreshes itself, so anteroom is useful straight from `go
// build` without a Node toolchain.
func loadAssets(fsys fs.FS, log *slog.Logger) assets {
	a := assets{scripts: map[string][]string{}, styles: map[string][]string{}}

	raw, err := fs.ReadFile(fsys, "web/static/manifest.json")
	if err != nil {
		log.Warn("anteroom: front-end assets are not built, serving the plain waiting page (run `make web`)")
		return a
	}
	var manifest viteManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		log.Error("anteroom: the front-end build manifest is unreadable", "err", err)
		return a
	}

	base := Prefix + "static/"
	for _, entry := range manifest {
		if !entry.IsEntry || entry.Name == "" {
			continue
		}
		a.scripts[entry.Name] = append(a.scripts[entry.Name], base+entry.File)
		for _, css := range entry.CSS {
			a.styles[entry.Name] = append(a.styles[entry.Name], base+css)
		}
	}
	return a
}

func (a assets) scriptsFor(entry string) []string { return a.scripts[entry] }
func (a assets) stylesFor(entry string) []string  { return a.styles[entry] }

// built reports whether the front-end for an entry point is available.
func (a assets) built(entry string) bool { return len(a.scripts[entry]) > 0 }
