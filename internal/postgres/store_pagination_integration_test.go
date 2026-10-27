//go:build integration

package postgres_test

import (
	"errors"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
)

func TestPaginationPreservesEveryOwnerScopeShape(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name        string
		all         bool
		owners      []string
		filterOwner string
		want        int
		forbidden   bool
	}{
		{name: "empty scope", want: 0},
		{name: "single owner", owners: []string{f.owner}, want: 2},
		{name: "multiple owners", owners: []string{f.owner, f.otherOwner}, want: 3},
		{name: "all owners", all: true, want: 3},
		{name: "authorized owner filter", owners: []string{f.owner, f.otherOwner}, filterOwner: f.otherOwner, want: 1},
		{name: "forbidden owner filter", owners: []string{f.owner}, filterOwner: f.otherOwner, forbidden: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := inventory.Scope{ActorID: f.actor, OrganizationID: f.org, WarehouseID: f.wh, AllOwners: tc.all, OwnerIDs: tc.owners}
			for range 8 {
				page, err := f.store.List(t.Context(), inventory.StockResource, scope, inventory.Filters{Limit: 1, OwnerID: tc.filterOwner})
				if tc.forbidden {
					var problem *inventory.Problem
					if !errors.As(err, &problem) || problem.Status != 403 {
						t.Fatalf("foreign owner accepted: %v", err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				total := len(page.Data)
				seen := map[string]bool{}
				for page.NextCursor != "" {
					if seen[page.NextCursor] {
						t.Fatal("pagination cursor repeated")
					}
					seen[page.NextCursor] = true
					page, err = f.store.List(t.Context(), inventory.StockResource, scope, inventory.Filters{Limit: 1, OwnerID: tc.filterOwner, Cursor: page.NextCursor})
					if err != nil {
						t.Fatal(err)
					}
					total += len(page.Data)
				}
				if total != tc.want {
					t.Fatalf("rows=%d want=%d", total, tc.want)
				}
			}
		})
	}
}
