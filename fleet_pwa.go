package main

import (
	"embed"
	"net/http"
)

// The FLEET page as an installable web app: a manifest, a service worker and
// icons. They hold nothing secret and are the same for every visitor, so they
// need no auth: a home-screen launch and the worker's install fetch them
// before (or without) a login cookie.

//go:embed fleet_sw.js
var fleetServiceWorkerJS []byte

//go:embed fleet_assets/*.png
var fleetAssets embed.FS

// fleetBackground is the page's light --bg (fleet.html :root).
const fleetBackground = "#f5f5f2"

var fleetManifest = []byte(`{
  "name": "FLEET",
  "short_name": "FLEET",
  "id": "/fleet",
  "start_url": "/fleet",
  "scope": "/fleet",
  "display": "standalone",
  "background_color": "` + fleetBackground + `",
  "theme_color": "` + fleetBackground + `",
  "icons": [
    {"src": "/fleet/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any"},
    {"src": "/fleet/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any"},
    {"src": "/fleet/icon-maskable-512.png", "sizes": "512x512", "type": "image/png", "purpose": "maskable"}
  ]
}
`)

var fleetIconNames = []string{"icon-192.png", "icon-512.png", "icon-maskable-512.png", "apple-touch-icon.png"}

func registerFleetPWARoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /fleet/manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		writeFleetStatic(w, "application/manifest+json", "public, max-age=86400", fleetManifest)
	})
	mux.HandleFunc("GET /fleet/sw.js", func(w http.ResponseWriter, r *http.Request) {
		// The worker sits at /fleet/sw.js, so by default it may control only
		// /fleet/...; the page registers it with scope /fleet, which needs
		// this header. no-cache so a new version is picked up at once.
		w.Header().Set("Service-Worker-Allowed", "/fleet")
		writeFleetStatic(w, "text/javascript; charset=utf-8", "no-cache", fleetServiceWorkerJS)
	})
	for _, name := range fleetIconNames {
		data, err := fleetAssets.ReadFile("fleet_assets/" + name)
		if err != nil {
			panic("fleet: embedded icon missing: " + name)
		}
		mux.HandleFunc("GET /fleet/"+name, func(w http.ResponseWriter, r *http.Request) {
			writeFleetStatic(w, "image/png", "public, max-age=86400", data)
		})
	}
}

func writeFleetStatic(w http.ResponseWriter, contentType, cacheControl string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", cacheControl)
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}
