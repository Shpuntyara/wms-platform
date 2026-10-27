package inventory

import "testing"

func TestValidateQuantity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, value string
		valid       bool
	}{
		{"integer", "12", true}, {"fraction", "0.125", true},
		{"maximum", "999999999999999999.999999", true},
		{"zero", "0", false}, {"fractional zero", "0.000000", false},
		{"negative", "-1", false}, {"nan", "NaN", false},
		{"infinity", "Infinity", false}, {"exponent", "1e3", false},
		{"too precise", "0.0000001", false}, {"too large", "1000000000000000000", false},
		{"whitespace", " 1", false}, {"leading zero", "01", false}, {"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateQuantity(tc.value); (err == nil) != tc.valid {
				t.Fatalf("ValidateQuantity(%q) = %v", tc.value, err)
			}
		})
	}
}

func TestPrincipalScope(t *testing.T) {
	t.Parallel()
	p := Principal{ActorID: "actor", Grants: []Grant{
		{OrganizationID: "org", WarehouseID: "warehouse", OwnerID: "owner-b", Permission: "stock.read"},
		{OrganizationID: "org", WarehouseID: "warehouse", OwnerID: "owner-a", Permission: "stock.read"},
		{OrganizationID: "org", WarehouseID: "warehouse", OwnerID: "owner-a", Permission: "stock.read"},
		{OrganizationID: "org", WarehouseID: "other", Permission: "movement.create"},
	}}
	scope, err := p.Scope("stock.read", "org", "warehouse")
	if err != nil || scope.AllOwners || len(scope.OwnerIDs) != 2 || scope.OwnerIDs[0] != "owner-a" {
		t.Fatalf("scope = %+v, %v", scope, err)
	}
	for _, tc := range []struct{ name, permission, organization, warehouse string }{
		{"different warehouse", "stock.read", "org", "other"},
		{"different organization", "stock.read", "elsewhere", "warehouse"},
		{"different permission", "movement.create", "org", "warehouse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.Scope(tc.permission, tc.organization, tc.warehouse); err == nil {
				t.Fatal("unauthorized scope accepted")
			}
		})
	}
	p.Grants = append(p.Grants, Grant{OrganizationID: "org", WarehouseID: "warehouse", Permission: "stock.read"})
	scope, err = p.Scope("stock.read", "org", "warehouse")
	if err != nil || !scope.AllOwners {
		t.Fatalf("unrestricted owner grant not honored: %+v, %v", scope, err)
	}
}
