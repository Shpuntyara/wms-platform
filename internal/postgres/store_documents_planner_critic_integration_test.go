//go:build integration

package postgres_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
)

func assertPlannerCriticCandidateBounds(t *testing.T, raw json.RawMessage, deep bool) {
	t.Helper()
	var evidence struct {
		Explain []struct {
			Plan map[string]any `json:"Plan"`
		} `json:"explain"`
	}
	if err := json.Unmarshal(raw, &evidence); err != nil || len(evidence.Explain) != 1 {
		t.Fatalf("invalid captured executed plan: %v", err)
	}
	indexConditions := 0
	var walk func(map[string]any, bool)
	walk = func(node map[string]any, candidates bool) {
		if node["Subplan Name"] == "CTE document_page" {
			candidates = true
		}
		if candidates {
			for _, field := range []string{"Rows Removed by Filter", "Rows Removed by Index Recheck"} {
				if removed, ok := node[field].(float64); ok && removed > 0 {
					t.Errorf("candidate scan discarded out-of-range rows: %s=%.0f", field, removed)
				}
			}
			if condition, ok := node["Index Cond"].(string); ok {
				indexConditions++
				for _, field := range []string{"organization_id", "warehouse_id", "owner_id"} {
					if !strings.Contains(condition, field) {
						t.Errorf("candidate index condition omits %s: %s", field, condition)
					}
				}
				if deep && !regexp.MustCompile(`\bid\s*>\s*`).MatchString(condition) {
					t.Errorf("candidate cursor is filtered after the index seek: %s", condition)
				}
			}
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				if value, ok := child.(map[string]any); ok {
					walk(value, candidates)
				}
			}
		}
	}
	walk(evidence.Explain[0].Plan, false)
	if indexConditions == 0 {
		t.Error("candidate page did not execute an index seek")
	}
}

// Global owner frequencies deliberately disagree with their frequencies inside
// the requested warehouse. UUID suffixes and ownership are deterministic;
// the random namespace only isolates repeated runs in the disposable database.
func TestDocumentPlannerCriticSkewedOwnerHistories(t *testing.T) {
	f := newFixture(t)
	otherWarehouse, noiseOwner := id(), id()
	f.exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, otherWarehouse, f.org)
	f.exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, noiseOwner, f.org)
	capture := captureDocumentReads(t, f)
	prefix := f.org[:24]
	type modelRow struct{ id, owner string }
	var model []modelRow
	rows, err := f.admin.Query(t.Context(), `SELECT id::text,owner_id::text FROM warehouse.documents WHERE organization_id=$1 AND warehouse_id=$2`, f.org, f.wh)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var row modelRow
		if err := rows.Scan(&row.id, &row.owner); err != nil {
			t.Fatal(err)
		}
		model = append(model, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var plans []map[string]any
	savePlans := func() {
		if dir := os.Getenv("WMS_DOCUMENT_CRITIC_PLAN_DIR"); dir != "" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Error(err)
				return
			}
			source, err := os.ReadFile("store.go")
			if err != nil {
				t.Error(err)
				return
			}
			testSource, err := os.ReadFile("store_documents_planner_critic_integration_test.go")
			if err != nil {
				t.Error(err)
				return
			}
			report := map[string]any{"passed": !t.Failed(), "complete": len(plans) == 8, "distribution": "target warehouse: two permitted owners at suffixes 0/1 modulo 1000; other warehouse: permitted owners on every row", "store_sha256": fmt.Sprintf("%x", sha256.Sum256(source)), "test_sha256": fmt.Sprintf("%x", sha256.Sum256(testSource)), "plans": plans}
			raw, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(filepath.Join(dir, "document-planner-critic-plans.json"), append(raw, '\n'), 0600); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(savePlans)
	for _, size := range []int{10000, 100000} {
		perWarehouse, start := size/2, 1
		if size == 100000 {
			start = 5001
		}
		for _, target := range []bool{true, false} {
			warehouse, offset := otherWarehouse, 1000000
			if target {
				warehouse, offset = f.wh, 0
			}
			f.exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state)
			SELECT ($1||lpad((g+$7)::text,12,'0'))::uuid,$2,$3,
			 CASE WHEN $8::boolean THEN CASE g%1000 WHEN 0 THEN $4::uuid WHEN 1 THEN $5::uuid ELSE $6::uuid END ELSE CASE g%2 WHEN 0 THEN $4::uuid ELSE $5::uuid END END,
			 'shipment','planner-critic-'||g,'draft' FROM generate_series($9::integer,$10::integer) g`, prefix, f.org, warehouse, f.owner, f.otherOwner, noiseOwner, offset, target, start, perWarehouse)
			f.exec(`INSERT INTO warehouse.document_lines(document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty)
			 SELECT d.id,d.organization_id,d.warehouse_id,d.owner_id,$1,1,1 FROM warehouse.documents d
			 WHERE d.organization_id=$2 AND d.warehouse_id=$3 AND NOT EXISTS(SELECT 1 FROM warehouse.document_lines l WHERE l.document_id=d.id)`, f.item, f.org, warehouse)
		}
		for g := start; g <= perWarehouse; g++ {
			if g%1000 == 0 {
				model = append(model, modelRow{fmt.Sprintf("%s%012d", prefix, g), f.owner})
			}
			if g%1000 == 1 {
				model = append(model, modelRow{fmt.Sprintf("%s%012d", prefix, g), f.otherOwner})
			}
		}
		slices.SortFunc(model, func(a, b modelRow) int { return strings.Compare(a.id, b.id) })
		f.exec(`ANALYZE warehouse.documents; ANALYZE warehouse.document_lines`)
		for _, deep := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/deep=%t", size, deep), func(t *testing.T) {
				scope := inventory.Scope{ActorID: f.actor, OrganizationID: f.org, WarehouseID: f.wh, OwnerIDs: []string{f.owner, f.otherOwner, f.owner}}
				filters := inventory.Filters{Limit: 20}
				first, err := f.store.List(t.Context(), inventory.DocumentResource, scope, filters)
				if err != nil {
					t.Fatal(err)
				}
				after := ""
				if deep {
					if first.NextCursor == "" { // The 10k fixture has fewer than one full page.
						// Obtain an authentic cursor binding with a smaller page; only
						// the deterministic ID boundary is replaced below.
						seed, err := f.store.List(t.Context(), inventory.DocumentResource, scope, inventory.Filters{Limit: 1})
						if err != nil || seed.NextCursor == "" {
							t.Fatalf("cursor seed failed: %v", err)
						}
						first.NextCursor = seed.NextCursor
					}
					after = fmt.Sprintf("%s%012d", prefix, perWarehouse*4/5)
					filters.Cursor = documentDeepCursor(t, first.NextCursor, after)
				}
				page, err := f.store.List(t.Context(), inventory.DocumentResource, scope, filters)
				if err != nil {
					t.Fatal(err)
				}
				var want, got []string
				for _, row := range model {
					if row.id > after {
						want = append(want, row.id)
					}
				}
				more := len(want) > 20
				if more {
					want = want[:20]
				}
				for _, raw := range page.Data {
					var document readDocument
					if err := json.Unmarshal(raw, &document); err != nil {
						t.Fatal(err)
					}
					if document.OrganizationID != f.org || document.WarehouseID != f.wh || !scope.AllowsOwner(document.OwnerID) || len(document.Lines) != 1 {
						t.Fatalf("scoped document or line hydration changed: %s", raw)
					}
					got = append(got, document.ID)
				}
				if !slices.Equal(got, want) || (page.NextCursor != "") != more {
					t.Fatalf("independent page oracle mismatch got=%v want=%v cursor=%t want-more=%t", got, want, page.NextCursor != "", more)
				}
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					raw, plan := explainDocumentRead(t, f, capture, mode)
					blocks, work := plan.Hits+plan.Reads, documentReadPlanRows(plan)
					plans = append(plans, map[string]any{"documents": size, "lines": size, "deep": deep, "mode": mode, "blocks": blocks, "row_work": work, "plan": raw})
					savePlans()
					t.Logf("documents=%d deep=%t mode=%s blocks=%d row_work=%.0f", size, deep, mode, blocks, work)
					if blocks > 1000 || work > 1000 {
						t.Errorf("skewed-owner document page scanned history: blocks=%d row_work=%.0f for at most21 candidates", blocks, work)
					}
					assertPlannerCriticCandidateBounds(t, raw, deep)
				}
			})
		}
	}
}
