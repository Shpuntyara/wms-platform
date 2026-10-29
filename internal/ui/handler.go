// Package ui serves the browser application beside the authenticated API.
package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/api"
)

func New(backend http.Handler, assets fs.FS, oidc api.BrowserOIDC) (http.Handler, error) {
	if backend == nil || assets == nil {
		return nil, errors.New("API handler and browser assets required")
	}
	publicConfig, err := json.Marshal(map[string]api.BrowserOIDC{"oidc": oidc})
	if err != nil {
		return nil, err
	}
	if _, err = api.DecodeWorkspaceConfig(publicConfig); err != nil {
		return nil, err
	}
	if info, err := fs.Stat(assets, "index.html"); err != nil || info.IsDir() {
		return nil, errors.New("browser index asset unavailable")
	}
	connect := "'self'"
	if oidc.URL != "" {
		u, _ := url.Parse(oidc.URL)
		connect += " " + u.Scheme + "://" + u.Host
	}
	policy := "default-src 'self'; script-src 'self'; style-src 'self'; connect-src " + connect + "; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" && !strings.HasPrefix(r.URL.Path, "/app/") {
			backend.ServeHTTP(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", policy)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/app" {
			http.Redirect(w, r, "/app/", http.StatusPermanentRedirect)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/app/")
		if name == "config.json" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(publicConfig))
			return
		}
		if name == "" {
			name = "index.html"
		}
		if !publicAsset(name) {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(name, ".html"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case strings.HasSuffix(name, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case strings.HasSuffix(name, ".js"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case strings.HasSuffix(name, ".svg"):
			w.Header().Set("Content-Type", "image/svg+xml")
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	}), nil
}

func publicAsset(name string) bool {
	if !fs.ValidPath(name) || strings.Contains(name, "\\") {
		return false
	}
	if name == "index.html" || name == "style.css" || name == "favicon.svg" {
		return true
	}
	return strings.Count(name, "/") == 1 && (strings.HasPrefix(name, "src/") || strings.HasPrefix(name, "vendor/")) && strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".test.js") && !strings.HasSuffix(name, ".spec.js")
}
