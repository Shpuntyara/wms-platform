//go:build integration

package postgres_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
)

func planReadsDocumentLines(p documentReadPlan) bool {
	if p.Relation == "document_lines" {
		return true
	}
	return slices.ContainsFunc(p.Plans, planReadsDocumentLines)
}

func documentLineHydrations(p documentReadPlan) float64 {
	if p.Node == "Aggregate" && planReadsDocumentLines(p) {
		return p.Loops
	}
	var total float64
	for _, child := range p.Plans {
		total += documentLineHydrations(child)
	}
	return total
}

// Many permitted owners should increase candidate selection, not the number of
// costly 200-line JSON aggregates hydrated for a single page.
func TestDocumentAdversarialOwnerFanoutHydratesOnlyPage(t *testing.T) {
	f := newFixture(t)
	f.exec(`INSERT INTO catalog.stock_owners(id,organization_id,code)
	 SELECT md5($1::text||':critic-owner:'||g)::uuid,$1::uuid,'critic-fanout-owner-'||g FROM generate_series(1,32) g`, f.org)
	f.exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state)
	 SELECT md5(o.id::text||':critic-document:'||g)::uuid,$1,$2,o.id,'shipment','critic-fanout-doc-'||o.id::text||'-'||g,'draft'
	 FROM catalog.stock_owners o CROSS JOIN generate_series(1,32) g WHERE o.organization_id=$1 AND o.code LIKE 'critic-fanout-owner-%'`, f.org, f.wh)
	f.exec(`INSERT INTO warehouse.document_lines(document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty)
	 SELECT d.id,d.organization_id,d.warehouse_id,d.owner_id,$1,g,10
	 FROM warehouse.documents d CROSS JOIN generate_series(1,200) g WHERE d.organization_id=$2 AND d.number LIKE 'critic-fanout-doc-%'`, f.item, f.org)
	f.exec(`ANALYZE warehouse.documents; ANALYZE warehouse.document_lines`)
	var owners []string
	rows, err := f.admin.Query(t.Context(), `SELECT id::text FROM catalog.stock_owners WHERE organization_id=$1 AND code LIKE 'critic-fanout-owner-%' ORDER BY id`, f.org)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var owner string
		if err = rows.Scan(&owner); err != nil {
			t.Fatal(err)
		}
		owners = append(owners, owner)
	}
	rows.Close()
	if err = rows.Err(); err != nil || len(owners) != 32 {
		t.Fatalf("owner fixture=%d err=%v", len(owners), err)
	}
	scope := inventory.Scope{ActorID: f.actor, OrganizationID: f.org, WarehouseID: f.wh, OwnerIDs: owners}
	capture := captureDocumentReads(t, f)
	page, err := f.store.List(t.Context(), inventory.DocumentResource, scope, inventory.Filters{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var expected []string
	rows, err = f.admin.Query(t.Context(), `SELECT id::text FROM warehouse.documents WHERE organization_id=$1 AND warehouse_id=$2 AND number LIKE 'critic-fanout-doc-%' ORDER BY id LIMIT 20`, f.org, f.wh)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, value)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, raw := range page.Data {
		var doc readDocument
		if err = json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if len(doc.Lines) != 200 {
			t.Fatalf("returned document %s lost lines: %d", doc.ID, len(doc.Lines))
		}
		got = append(got, doc.ID)
	}
	if !slices.Equal(got, expected) || page.NextCursor == "" {
		t.Fatalf("document page differs from independent sorted fixture: got=%v expected=%v cursor=%q", got, expected, page.NextCursor)
	}
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		raw, plan := explainDocumentRead(t, f, capture, mode)
		hydrations := documentLineHydrations(plan)
		t.Logf("owners=32 documents=1024 lines=204800 mode=%s hydrated_documents=%.0f blocks=%d row_work=%.0f", mode, hydrations, plan.Hits+plan.Reads, documentReadPlanRows(plan))
		if dir := os.Getenv("WMS_DOCUMENT_CRITIC_PLAN_DIR"); dir != "" {
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "fanout-"+mode+".json"), append(raw, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if hydrations > 21 {
			t.Errorf("hydrated %.0f documents for a page needing only 21 including lookahead; per-owner candidate selection must precede document line aggregation", hydrations)
		}
	}
}

func TestDocumentAdversarialCursorCannotCrossAuthorizationScope(t *testing.T) {
	f := newFixture(t)
	for range 4 {
		f.doc("shipment", "1", f.otherOwner)
	}
	scope := inventory.Scope{ActorID: f.actor, OrganizationID: f.org, WarehouseID: f.wh, OwnerIDs: []string{f.owner, f.otherOwner}}
	page, err := f.store.List(t.Context(), inventory.DocumentResource, scope, inventory.Filters{Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("no genuine document cursor: %v", err)
	}
	for _, tc := range []struct {
		name  string
		scope inventory.Scope
		owner string
	}{
		{name: "revoked owner", scope: inventory.Scope{ActorID: f.actor, OrganizationID: f.org, WarehouseID: f.wh, OwnerIDs: []string{f.owner}}},
		{name: "different actor", scope: inventory.Scope{ActorID: id(), OrganizationID: f.org, WarehouseID: f.wh, OwnerIDs: scope.OwnerIDs}},
		{name: "different warehouse", scope: inventory.Scope{ActorID: f.actor, OrganizationID: f.org, WarehouseID: id(), OwnerIDs: scope.OwnerIDs}},
		{name: "different organization", scope: inventory.Scope{ActorID: f.actor, OrganizationID: id(), WarehouseID: f.wh, OwnerIDs: scope.OwnerIDs}},
		{name: "all owner grant", scope: inventory.Scope{ActorID: f.actor, OrganizationID: f.org, WarehouseID: f.wh, AllOwners: true}},
		{name: "explicit owner filter", scope: scope, owner: f.owner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.store.List(t.Context(), inventory.DocumentResource, tc.scope, inventory.Filters{Limit: 1, Cursor: page.NextCursor, OwnerID: tc.owner})
			var problem *inventory.Problem
			if !errors.As(err, &problem) || problem.Status != 400 || problem.Code != "invalid_cursor" {
				t.Fatalf("cursor crossed changed scope: %v", err)
			}
		})
	}
}
