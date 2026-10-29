package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadWorkspaceConfigUsesPublicBrowserClientAndSeparateCatalog(t *testing.T) {
	issuer := "https://wms.example.invalid/auth/realms/wms"
	config, err := loadWorkspaceConfig("", issuer, "")
	if err != nil {
		t.Fatal(err)
	}
	if config.OIDC.URL != "https://wms.example.invalid/auth" || config.OIDC.Realm != "wms" || config.OIDC.ClientID != "wms-browser" || len(config.Items) != 0 {
		t.Fatalf("wrong default public client: %+v", config)
	}
	config, err = loadWorkspaceConfig("", issuer, "warehouse-terminal")
	if err != nil || config.OIDC.ClientID != "warehouse-terminal" {
		t.Fatal("custom public client was ignored")
	}
	path := filepath.Join(t.TempDir(), "workspace.json")
	if err := os.WriteFile(path, []byte(`{"organizations":[{"id":"11111111-1111-4111-8111-111111111111","name":"Мой склад"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	config, err = loadWorkspaceConfig(path, issuer, "")
	if err != nil || len(config.Organizations) != 1 || config.OIDC.Realm != "wms" {
		t.Fatalf("catalog not loaded: %+v %v", config, err)
	}
	for _, tc := range []struct{ name, body string }{
		{"secret rejected", `{"clientSecret":"do-not-publish"}`},
		{"other issuer", `{"oidc":{"url":"https://wrong.example.invalid","realm":"wms","clientId":"browser"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadWorkspaceConfig(path, issuer, ""); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	if _, err := loadWorkspaceConfig(filepath.Join(t.TempDir(), "missing"), issuer, ""); err == nil {
		t.Fatal("missing configured catalog silently ignored")
	}
	config, err = loadWorkspaceConfig("", "https://generic-issuer.example.invalid", "")
	if err != nil || config.OIDC.URL != "" {
		t.Fatal("generic OIDC issuer must keep backend working and leave Keycloak UI unconfigured")
	}
}
