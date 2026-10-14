package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestFleetPWARoutesNeedNoAuth(t *testing.T) {
	s := fleetTestServer(t, nil)
	cases := []struct {
		path, ctype, cache string
	}{
		{"/fleet/manifest.webmanifest", "application/manifest+json", "public, max-age=86400"},
		{"/fleet/sw.js", "text/javascript", "no-cache"},
		{"/fleet/icon-192.png", "image/png", "public, max-age=86400"},
		{"/fleet/icon-512.png", "image/png", "public, max-age=86400"},
		{"/fleet/icon-maskable-512.png", "image/png", "public, max-age=86400"},
		{"/fleet/apple-touch-icon.png", "image/png", "public, max-age=86400"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", c.path, nil))
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), c.ctype) ||
			w.Header().Get("Cache-Control") != c.cache || w.Body.Len() == 0 {
			t.Errorf("%s: %d %q %q", c.path, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
		}
		if w.Header().Get("X-Fleet-App") != "" {
			t.Errorf("%s must not carry X-Fleet-App", c.path)
		}
	}
}

func TestFleetServiceWorkerScopeHeader(t *testing.T) {
	w := httptest.NewRecorder()
	fleetMux(fleetTestServer(t, nil)).ServeHTTP(w, httptest.NewRequest("GET", "/fleet/sw.js", nil))
	if got := w.Header().Get("Service-Worker-Allowed"); got != "/fleet" {
		t.Fatalf("Service-Worker-Allowed = %q", got)
	}
	sw := w.Body.String()
	// The worker must leave data, actions and login alone, and cache /fleet
	// only when it is the app page.
	for _, need := range []string{"/fleet/state", "/fleet/chat", "/fleet/log", "/fleet/usage", "/fleet/say",
		"/fleet/start", "/fleet/login", "/fleet/accounts", "/fleet/account", "/fleet/push", `searchParams.has("c")`, `"X-Fleet-App"`, `req.method !== "GET"`,
		"clients.claim()", "const VERSION = ",
		// Web Push: show every push, open the card on a tap.
		`addEventListener("push"`, "showNotification(", `addEventListener("notificationclick"`, `"open-card"`, "#card-"} {
		if !strings.Contains(sw, need) {
			t.Errorf("sw.js must contain %q", need)
		}
	}
}

func TestFleetManifest(t *testing.T) {
	w := httptest.NewRecorder()
	fleetMux(fleetTestServer(t, nil)).ServeHTTP(w, httptest.NewRequest("GET", "/fleet/manifest.webmanifest", nil))
	var m struct {
		Name            string `json:"name"`
		ShortName       string `json:"short_name"`
		ID              string `json:"id"`
		StartURL        string `json:"start_url"`
		Scope           string `json:"scope"`
		Display         string `json:"display"`
		BackgroundColor string `json:"background_color"`
		ThemeColor      string `json:"theme_color"`
		Icons           []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "FLEET" || m.ShortName != "FLEET" || m.ID != "/fleet" || m.StartURL != "/fleet" || m.Scope != "/fleet" ||
		m.Display != "standalone" || m.BackgroundColor == "" || m.ThemeColor == "" {
		t.Fatalf("manifest = %+v", m)
	}
	// background_color tracks the page's light --bg.
	if !strings.Contains(string(fleetPageHTML), "--bg: "+fleetBackground) {
		t.Errorf("fleet.html :root --bg is no longer %s", fleetBackground)
	}
	purposes := map[string]bool{}
	mux := fleetMux(fleetTestServer(t, nil))
	for _, ic := range m.Icons {
		purposes[ic.Sizes+" "+ic.Purpose] = true
		rw := httptest.NewRecorder()
		mux.ServeHTTP(rw, httptest.NewRequest("GET", ic.Src, nil))
		if rw.Code != http.StatusOK {
			t.Errorf("icon %s: %d", ic.Src, rw.Code)
		}
	}
	for _, want := range []string{"192x192 any", "512x512 any", "512x512 maskable"} {
		if !purposes[want] {
			t.Errorf("manifest has no %s icon", want)
		}
	}
}

func TestFleetIconsAreSmallPNGsOfTheRightSize(t *testing.T) {
	sizes := map[string]int{"icon-192.png": 192, "icon-512.png": 512, "icon-maskable-512.png": 512, "apple-touch-icon.png": 180}
	for _, name := range fleetIconNames {
		data, err := fleetAssets.ReadFile("fleet_assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > 20<<10 {
			t.Errorf("%s is %d bytes, want < 20 KB", name, len(data))
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width != sizes[name] || cfg.Height != sizes[name] {
			t.Errorf("%s: %dx%d %v, want %d", name, cfg.Width, cfg.Height, err, sizes[name])
		}
	}
}

func TestFleetAppHeaderOnlyOnTheAppPage(t *testing.T) {
	s := fleetTestServer(t, nil)
	mux := fleetMux(s)
	get := func(path string, cookie bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if cookie {
			r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	app := get("/fleet", true)
	if app.Header().Get("X-Fleet-App") != "1" {
		t.Fatal("app page must carry X-Fleet-App: 1")
	}
	csp := app.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "manifest-src 'self'") || !strings.Contains(csp, "worker-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}
	for _, path := range []string{"/fleet", "/fleet?hop=1", "/fleet?c=abc"} {
		if w := get(path, false); w.Header().Get("X-Fleet-App") != "" {
			t.Errorf("%s without auth (%d) must not carry X-Fleet-App", path, w.Code)
		}
	}
}

func TestFleetServiceWorkerVersionIsAnAssetHash(t *testing.T) {
	w := httptest.NewRecorder()
	fleetMux(fleetTestServer(t, nil)).ServeHTTP(w, httptest.NewRequest("GET", "/fleet/sw.js", nil))
	body := w.Body.String()
	if strings.Contains(body, "@VERSION@") {
		t.Fatal("the version placeholder must be substituted")
	}
	icon, _ := fleetAssets.ReadFile("fleet_assets/icon-192.png")
	v := fleetAssetVersion(fleetServiceWorkerJS, fleetManifest, icon)
	if len(v) != 12 {
		t.Fatalf("version %q", v)
	}
	served := regexp.MustCompile(`const VERSION = "fleet-([0-9a-f]{12})";`).FindStringSubmatch(body)
	if served == nil {
		t.Fatalf("no hashed VERSION in %q", body)
	}
	a := fleetServiceWorkerBody(fleetServiceWorkerJS, fleetManifest, icon)
	b := fleetServiceWorkerBody(fleetServiceWorkerJS, fleetManifest, append(append([]byte{}, icon...), 0))
	c := fleetServiceWorkerBody(fleetServiceWorkerJS, fleetManifest, icon)
	if bytes.Equal(a, b) || !bytes.Equal(a, c) {
		t.Fatal("the version must change with an asset and only then")
	}
	if !bytes.Contains(a, []byte(`"fleet-`+v+`"`)) {
		t.Fatalf("body does not carry %s", v)
	}
}
