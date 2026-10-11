// Package web holds the browser assets: tokens.css, the only place colours
// are defined; app.css and app.js; and the page templates. They are
// embedded, so the binary serves them without any network. internal/web
// renders the pages.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"path"
	"strings"
)

//go:embed static templates
var Files embed.FS

const (
	staticDirectory = "static"
	versionLength   = 12
	layoutFile      = "templates/layout.html"
	partialFiles    = "templates/partials/*.html"
	pageFiles       = "templates/pages/*.html"
)

// AssetVersion is a short hash of the static files. Pages put it in asset
// URLs, so browsers keep assets cached exactly until they change.
func AssetVersion() (string, error) {
	digest := sha256.New()
	err := fs.WalkDir(Files, staticDirectory, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := Files.ReadFile(name)
		if err != nil {
			return err
		}
		digest.Write([]byte(name))
		digest.Write(content)
		return nil
	})
	return hex.EncodeToString(digest.Sum(nil))[:versionLength], err
}

// Templates parses the layout and partials once for every page, keyed by
// the page file name without ".html". functions are the helpers pages use.
func Templates(functions template.FuncMap) (map[string]*template.Template, error) {
	pages, err := fs.Glob(Files, pageFiles)
	if err != nil {
		return nil, err
	}
	parsed := map[string]*template.Template{}
	for _, page := range pages {
		name := strings.TrimSuffix(path.Base(page), ".html")
		pageTemplate, err := template.New(path.Base(layoutFile)).Funcs(functions).ParseFS(Files, layoutFile, partialFiles, page)
		if err != nil {
			return nil, err
		}
		parsed[name] = pageTemplate
	}
	return parsed, nil
}
