package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
)

func TestWorkspaceListsOnlyFreshlyResolvedPrincipalScopes(t *testing.T) {
	h, backend := testHandler(t)
	backend.principal.Grants = []inventory.Grant{
		{OrganizationID: testOrg, WarehouseID: testWarehouse, OwnerID: testBalance, Permission: "document.read"},
		{OrganizationID: testOrg, WarehouseID: testLine, Permission: "stock.read"},
		{OrganizationID: testOrg, WarehouseID: testWarehouse, OwnerID: testBalance, Permission: "document.read"},
	}
	w := request(h, "GET", "/api/v1/workspace", "", "valid", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("workspace should be available to authenticated employee, status=%d body=%s", w.Code, w.Body)
	}
	var got struct {
		Data struct {
			ActorID string `json:"actor_id"`
			Scopes  []struct {
				OrganizationID string `json:"organization_id"`
				WarehouseID    string `json:"warehouse_id"`
				Grants         []struct {
					Permission string  `json:"permission"`
					OwnerID    *string `json:"owner_id"`
				} `json:"grants"`
			} `json:"scopes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Data.ActorID != testActor || len(got.Data.Scopes) != 2 {
		t.Fatalf("unexpected workspace: %s", w.Body)
	}
	first, second := got.Data.Scopes[0], got.Data.Scopes[1]
	if first.OrganizationID != testOrg || first.WarehouseID != testWarehouse || len(first.Grants) != 1 || first.Grants[0].Permission != "document.read" || first.Grants[0].OwnerID == nil || *first.Grants[0].OwnerID != testBalance {
		t.Fatalf("owner-specific grants were broadened or duplicated: %s", w.Body)
	}
	if second.WarehouseID != testLine || len(second.Grants) != 1 || second.Grants[0].Permission != "stock.read" || second.Grants[0].OwnerID != nil {
		t.Fatalf("warehouse-wide grant was changed: %s", w.Body)
	}
	if backend.calls != 0 {
		t.Fatal("workspace should not issue unscoped resource reads")
	}
	// A second request must not reuse previously returned permissions.
	backend.principal.Grants = nil
	w = request(h, "GET", "/api/v1/workspace", "", "valid", "", "")
	var empty map[string]map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(empty["data"]["scopes"], []any{}) {
		t.Fatalf("revoked grants remained available: %s", w.Body)
	}
}

func TestWorkspaceCatalogCannotExposeForeignScopesOwnersOrOIDC(t *testing.T) {
	_, backend := testHandler(t)
	backend.principal.Grants = []inventory.Grant{
		{OrganizationID: testOrg, WarehouseID: testWarehouse, OwnerID: testBalance, Permission: "document.read"},
		{OrganizationID: testOrg, WarehouseID: testLine, Permission: "document.create"},
	}
	registry, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := WorkspaceConfig{
		OIDC:          BrowserOIDC{URL: "https://private-issuer.invalid", Realm: "internal", ClientID: "browser"},
		Organizations: []NamedOrganization{{ID: testOrg, Name: "Allowed organization"}, {ID: testCommand, Name: "Foreign organization"}},
		Warehouses:    []NamedWarehouse{{ID: testWarehouse, OrganizationID: testOrg, Name: "Allowed warehouse"}, {ID: testLine, OrganizationID: testOrg, Name: "Create-only warehouse"}},
		Owners:        []NamedOwner{{ID: testBalance, OrganizationID: testOrg, Name: "Allowed owner"}, {ID: testCommand, OrganizationID: testOrg, Name: "Foreign owner"}},
		Items:         []NamedItem{{ID: testLine, OrganizationID: testOrg, SKU: "VISIBLE", Name: "Visible item", Unit: "ea"}, {ID: testCommand, OrganizationID: testCommand, SKU: "HIDDEN", Name: "Foreign item", Unit: "ea"}},
		Positions:     []NamedPosition{{ID: testBalance, OrganizationID: testOrg, WarehouseID: testWarehouse, Code: "RECEIVING", Kind: "receiving"}, {ID: testLine, OrganizationID: testOrg, WarehouseID: testLine, Code: "HIDDEN", Kind: "storage"}},
	}
	h, err := New(Config{Backend: backend, Auth: fakeAuth{}, Modules: registry, Workspace: config})
	if err != nil {
		t.Fatal(err)
	}
	w := request(h, "GET", "/api/v1/workspace", "", "valid", "", "")
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var result struct {
		Data struct {
			Catalog WorkspaceConfig `json:"catalog"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	catalog := result.Data.Catalog
	if len(catalog.Organizations) != 1 || catalog.Organizations[0].ID != testOrg || len(catalog.Warehouses) != 1 || catalog.Warehouses[0].ID != testWarehouse || len(catalog.Owners) != 1 || catalog.Owners[0].ID != testBalance || len(catalog.Items) != 1 || catalog.Items[0].SKU != "VISIBLE" || len(catalog.Positions) != 1 || catalog.Positions[0].Code != "RECEIVING" {
		t.Fatalf("catalog was omitted or broadened beyond read grants: %s", w.Body)
	}
	// OIDC bootstrap belongs to the public bootstrap route, never this catalog.
	if bytes.Contains(w.Body.Bytes(), []byte(`"oidc"`)) {
		t.Fatal("OIDC configuration leaked into catalog")
	}
	backend.principal.Grants = nil
	w = request(h, "GET", "/api/v1/workspace", "", "valid", "", "")
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data.Catalog.Items) != 0 || len(result.Data.Catalog.Positions) != 0 || len(result.Data.Catalog.Owners) != 0 {
		t.Fatal("revoked catalog entries remained visible")
	}
}

func TestWorkspaceRequiresAuthenticationAndGet(t *testing.T) {
	for _, tc := range []struct {
		name, method, token string
		status              int
	}{
		{"no token", "GET", "", 401},
		{"invalid token", "GET", "invalid", 401},
		{"mutation rejected", "POST", "valid", 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := testHandler(t)
			w := request(h, tc.method, "/api/v1/workspace", "", tc.token, "", "")
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if tc.status == 405 && w.Header().Get("Allow") != "GET" {
				t.Fatal("missing allowed method")
			}
		})
	}
}
