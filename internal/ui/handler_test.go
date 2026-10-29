package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Shpuntyara/wms-platform/internal/api"
)

func testHandler(t *testing.T) (http.Handler, *int) {
	t.Helper()
	calls := new(int)
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { *calls++; w.WriteHeader(401) })
	assets := fstest.MapFS{
		"index.html":          &fstest.MapFile{Data: []byte("<html lang=\"ru\"><main>Приёмка</main></html>")},
		"style.css":           &fstest.MapFile{Data: []byte("main {color: blue}")},
		"src/app.js":          &fstest.MapFile{Data: []byte("export const app = true;")},
		"vendor/keycloak.js":  &fstest.MapFile{Data: []byte("export default class Keycloak {}")},
		"config.example.json": &fstest.MapFile{Data: []byte("private catalog labels")},
		"tests/private.js":    &fstest.MapFile{Data: []byte("test fixtures should never be served")},
	}
	h, err := New(backend, assets, api.BrowserOIDC{URL: "https://login.example.invalid/auth", Realm: "wms", ClientID: "wms-browser"})
	if err != nil {
		t.Fatal(err)
	}
	return h, calls
}

func TestHandlerServesOnlyPublicAssetsAndOIDCBootstrap(t *testing.T) {
	for _, tc := range []struct{ name, path, contentType, contains string }{
		{"application", "/app/", "text/html", "Приёмка"},
		{"style", "/app/style.css", "text/css", "color: blue"},
		{"application module", "/app/src/app.js", "javascript", "app = true"},
		{"OIDC adapter", "/app/vendor/keycloak.js", "javascript", "Keycloak"},
		{"bootstrap", "/app/config.json", "application/json", "wms-browser"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, calls := testHandler(t)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), tc.contentType) || !strings.Contains(w.Body.String(), tc.contains) {
				t.Fatalf("status=%d type=%s body=%s", w.Code, w.Header().Get("Content-Type"), w.Body)
			}
			if *calls != 0 {
				t.Fatal("public asset passed through API authentication")
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "https://login.example.invalid") {
				t.Fatal("missing browser security headers")
			}
			if tc.name == "bootstrap" {
				var data map[string]json.RawMessage
				if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
					t.Fatal(err)
				}
				if len(data) != 1 || data["oidc"] == nil {
					t.Fatal("bootstrap must contain OIDC public client only")
				}
			}
		})
	}
}

func TestHandlerRejectsTraversalDirectoriesAndPrivateFiles(t *testing.T) {
	for _, path := range []string{"/app/../go.mod", "/app/%2e%2e/go.mod", "/app/src/", "/app/tests/private.js", "/app/config.example.json", "/app/missing.js", "/app/src/../index.html", "/app/src%5capp.js"} {
		t.Run(path, func(t *testing.T) {
			h, calls := testHandler(t)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 404 || *calls != 0 {
				t.Fatalf("unexpected resource exposure status=%d calls=%d body=%s", w.Code, *calls, w.Body)
			}
		})
	}
}

func TestHandlerKeepsAPIAuthenticationAndMethodSemantics(t *testing.T) {
	h, calls := testHandler(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/documents", nil))
	if w.Code != 401 || *calls != 1 {
		t.Fatal("API request bypassed authentication")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("HEAD", "/app/style.css", nil))
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD body=%s status=%d", w.Body, w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/app/", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatal("static resource accepted mutation method")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/app", nil))
	if w.Code != 308 || w.Header().Get("Location") != "/app/" {
		t.Fatal("application entry did not redirect to canonical path")
	}
}
