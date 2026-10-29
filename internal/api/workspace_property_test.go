package api

import (
	"fmt"
	"testing"
	"testing/quick"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
)

func TestWorkspaceCatalogPropertyMatchesIndependentPermissionScopes(t *testing.T) {
	identity := func(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
	catalog := WorkspaceConfig{}
	for orgIndex := 1; orgIndex <= 3; orgIndex++ {
		org := identity(orgIndex)
		catalog.Organizations = append(catalog.Organizations, NamedOrganization{ID: org, Name: "organization"})
		catalog.Items = append(catalog.Items, NamedItem{ID: identity(orgIndex + 100), OrganizationID: org, SKU: fmt.Sprint(orgIndex), Name: "item", Unit: "ea"})
		for ownerIndex := 1; ownerIndex <= 3; ownerIndex++ {
			catalog.Owners = append(catalog.Owners, NamedOwner{ID: identity(orgIndex*1000 + ownerIndex), OrganizationID: org, Name: "owner"})
		}
		for warehouseIndex := 1; warehouseIndex <= 2; warehouseIndex++ {
			warehouse := identity(orgIndex*100 + warehouseIndex)
			catalog.Warehouses = append(catalog.Warehouses, NamedWarehouse{ID: warehouse, OrganizationID: org, Name: "warehouse"})
			catalog.Positions = append(catalog.Positions, NamedPosition{ID: identity(orgIndex*10000 + warehouseIndex), OrganizationID: org, WarehouseID: warehouse, Code: "A-01", Kind: "storage"})
		}
	}
	property := func(seed uint64) bool {
		p := inventory.Principal{ActorID: testActor}
		permissions := []string{"document.read", "stock.read", "document.create", "movement.create"}
		for i, w := range catalog.Warehouses {
			for j, permission := range permissions {
				if seed&(uint64(1)<<uint(i*4+j)) == 0 {
					continue
				}
				owner := ""
				if seed&(uint64(1)<<uint(32+i)) != 0 {
					owner = identity((i/2+1)*1000 + int(seed%3) + 1)
				}
				p.Grants = append(p.Grants, inventory.Grant{OrganizationID: w.OrganizationID, WarehouseID: w.ID, OwnerID: owner, Permission: permission})
			}
		}
		readScopes := []inventory.Scope{}
		for _, w := range catalog.Warehouses {
			for _, permission := range []string{"document.read", "stock.read"} {
				if s, err := p.Scope(permission, w.OrganizationID, w.ID); err == nil {
					readScopes = append(readScopes, s)
				}
			}
		}
		allowed := func(org, warehouse, owner string) bool {
			for _, scope := range readScopes {
				if scope.OrganizationID == org && (warehouse == "" || scope.WarehouseID == warehouse) && (owner == "" || scope.AllowsOwner(owner)) {
					return true
				}
			}
			return false
		}
		got := catalog.catalog(p)
		for _, v := range got["organizations"].([]NamedOrganization) {
			if !allowed(v.ID, "", "") {
				return false
			}
		}
		for _, v := range got["warehouses"].([]NamedWarehouse) {
			if !allowed(v.OrganizationID, v.ID, "") {
				return false
			}
		}
		for _, v := range got["owners"].([]NamedOwner) {
			if !allowed(v.OrganizationID, "", v.ID) {
				return false
			}
		}
		for _, v := range got["items"].([]NamedItem) {
			if !allowed(v.OrganizationID, "", "") {
				return false
			}
		}
		for _, v := range got["positions"].([]NamedPosition) {
			if !allowed(v.OrganizationID, v.WarehouseID, "") {
				return false
			}
		}
		count := 0
		for _, v := range catalog.Organizations {
			if allowed(v.ID, "", "") {
				count++
			}
		}
		if len(got["organizations"].([]NamedOrganization)) != count {
			return false
		}
		count = 0
		for _, v := range catalog.Warehouses {
			if allowed(v.OrganizationID, v.ID, "") {
				count++
			}
		}
		if len(got["warehouses"].([]NamedWarehouse)) != count {
			return false
		}
		count = 0
		for _, v := range catalog.Owners {
			if allowed(v.OrganizationID, "", v.ID) {
				count++
			}
		}
		if len(got["owners"].([]NamedOwner)) != count {
			return false
		}
		count = 0
		for _, v := range catalog.Items {
			if allowed(v.OrganizationID, "", "") {
				count++
			}
		}
		if len(got["items"].([]NamedItem)) != count {
			return false
		}
		count = 0
		for _, v := range catalog.Positions {
			if allowed(v.OrganizationID, v.WarehouseID, "") {
				count++
			}
		}
		return len(got["positions"].([]NamedPosition)) == count
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 1000}); err != nil {
		t.Fatal(err)
	}
}
