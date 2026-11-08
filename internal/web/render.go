package web

// The app's static files (GET /static/{file}) and the helpers its
// templates (app.go) may call.

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"net/http"
	"path"
	"time"
)

// assets holds the app's static files: its stylesheets, scripts and icon.
// The templates are appTemplates (app.go).
//
//go:embed static/*
var assets embed.FS

// templateFuncs are the few helpers templates use; view models carry
// display strings already.
var templateFuncs = template.FuncMap{
	"stamp": stamp,
}

// staticFile is one embedded static file, served with an ETag.
type staticFile struct {
	data  []byte
	ctype string
	etag  string
}

// staticFiles are the files under /static/, and assetVersion a hash of
// them all, put in the pages' URLs so a new build is fetched at once.
var staticFiles, assetVersion = loadStatic()

func loadStatic() (map[string]staticFile, string) {
	types := map[string]string{
		".css": "text/css; charset=utf-8",
		".js":  "text/javascript; charset=utf-8",
		".svg": "image/svg+xml",
	}
	entries, err := assets.ReadDir("static")
	if err != nil {
		panic("web: embedded static files: " + err.Error())
	}
	files := map[string]staticFile{}
	all := sha256.New()
	for _, e := range entries {
		data, err := assets.ReadFile("static/" + e.Name())
		if err != nil {
			panic("web: embedded static file: " + err.Error())
		}
		ctype, ok := types[path.Ext(e.Name())]
		if !ok {
			panic("web: no content type for static file " + e.Name())
		}
		sum := sha256.Sum256(data)
		files[e.Name()] = staticFile{data: data, ctype: ctype, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		all.Write([]byte(e.Name()))
		all.Write(sum[:])
	}
	return files, hex.EncodeToString(all.Sum(nil)[:6])
}

// serveStatic is GET /static/{file}. Browsers revalidate with the ETag;
// pages link the files with ?v=<assetVersion>.
func serveStatic(w http.ResponseWriter, r *http.Request) {
	f, ok := staticFiles[r.PathValue("file")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", f.etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.data))
}
