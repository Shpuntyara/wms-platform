package inventory

import (
	"math/rand"
	"testing"
	"testing/quick"
)

func TestPrincipalAdversarialGrantScopesAreExact(t *testing.T) {
	property := func(mask uint8) bool {
		p := Principal{ActorID: "authenticated-actor"}
		grants := []Grant{
			{OrganizationID: "org", WarehouseID: "warehouse", OwnerID: "owner-a", Permission: "movement.adjust"},
			{OrganizationID: "org", WarehouseID: "warehouse", OwnerID: "owner-b", Permission: "movement.adjust"},
			{OrganizationID: "org", WarehouseID: "warehouse", Permission: "movement.adjust"},
			{OrganizationID: "org", WarehouseID: "warehouse", OwnerID: "owner-a", Permission: "movement.adjust"},
			{OrganizationID: "other-org", WarehouseID: "warehouse", Permission: "movement.adjust"},
			{OrganizationID: "org", WarehouseID: "other-warehouse", Permission: "movement.adjust"},
			{OrganizationID: "org", WarehouseID: "warehouse", Permission: "movement.create"},
			{OrganizationID: "org", WarehouseID: "warehouse", Permission: "stock.read"},
		}
		for i, g := range grants {
			if mask&(1<<i) != 0 {
				p.Grants = append(p.Grants, g)
			}
		}
		scope, err := p.Scope("movement.adjust", "org", "warehouse")
		if mask&15 == 0 {
			return err != nil
		}
		if err != nil || scope.ActorID != p.ActorID || scope.OrganizationID != "org" || scope.WarehouseID != "warehouse" {
			return false
		}
		all := mask&4 != 0
		wantA, wantB := all || mask&9 != 0, all || mask&2 != 0
		if scope.AllOwners != all || scope.AllowsOwner("owner-a") != wantA || scope.AllowsOwner("owner-b") != wantB || scope.AllowsOwner("ungranted-owner") != all {
			return false
		}
		for i := 1; i < len(scope.OwnerIDs); i++ {
			if scope.OwnerIDs[i-1] >= scope.OwnerIDs[i] {
				return false
			}
		}
		return true
	}
	// Exhaust the finite bitmask model as well as a reproducible generator.
	for mask := range 256 {
		if !property(uint8(mask)) {
			t.Fatalf("authorization model failed at mask=%08b", mask)
		}
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 128, Rand: rand.New(rand.NewSource(20261028))}); err != nil {
		t.Fatal(err)
	}
}
