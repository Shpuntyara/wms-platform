//go:build integration

package postgres_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/quick"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type documentReadCapture struct {
	mu   sync.Mutex
	sql  string
	args []any
}

func (c *documentReadCapture) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "SELECT to_jsonb(document_details)") && strings.Contains(d.SQL, "contracts_v1.document_details") {
		c.mu.Lock()
		c.sql, c.args = d.SQL, slices.Clone(d.Args)
		c.mu.Unlock()
	}
	return ctx
}

func (*documentReadCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func captureDocumentReads(t *testing.T, f *fixture) *documentReadCapture {
	t.Helper()
	c := &documentReadCapture{}
	config, err := pgxpool.ParseConfig(os.Getenv("WMS_TEST_RUNTIME_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Tracer = c
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.store, err = postgres.New(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type documentReadPlan struct {
	Node     string             `json:"Node Type"`
	Relation string             `json:"Relation Name"`
	Rows     float64            `json:"Actual Rows"`
	Loops    float64            `json:"Actual Loops"`
	Removed  float64            `json:"Rows Removed by Filter"`
	Hits     int                `json:"Shared Hit Blocks"`
	Reads    int                `json:"Shared Read Blocks"`
	Plans    []documentReadPlan `json:"Plans"`
}

func documentReadPlanRows(p documentReadPlan) float64 {
	rows := (p.Rows + p.Removed) * p.Loops
	for _, child := range p.Plans {
		rows += documentReadPlanRows(child)
	}
	return rows
}

func documentDeepCursor(t *testing.T, cursor, id string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["id"] = id
	raw, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func explainDocumentRead(t *testing.T, f *fixture, c *documentReadCapture, mode string) (json.RawMessage, documentReadPlan) {
	t.Helper()
	c.mu.Lock()
	query, args := c.sql, slices.Clone(c.args)
	c.mu.Unlock()
	if query == "" {
		t.Fatal("actual document adapter query was not captured")
	}
	conn, err := f.runtime.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(t.Context(), "SET plan_cache_mode="+mode); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DEALLOCATE document_read_plan; RESET plan_cache_mode") }()
	if _, err = conn.Exec(t.Context(), "PREPARE document_read_plan AS "+query); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	values := make([]string, len(args))
	for i, arg := range args {
		switch value := arg.(type) {
		case string:
			values[i] = quote(value)
		case bool:
			if value {
				values[i] = "true"
			} else {
				values[i] = "false"
			}
		case int:
			values[i] = fmt.Sprint(value)
		case []string:
			items := make([]string, len(value))
			for j, item := range value {
				items[j] = quote(item)
			}
			values[i] = "ARRAY[" + strings.Join(items, ",") + "]::text[]"
		default:
			t.Fatalf("unexpected document query argument %T", arg)
		}
	}
	var raw []byte
	if err = conn.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, SETTINGS, FORMAT JSON) EXECUTE document_read_plan("+strings.Join(values, ",")+")").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var result []struct {
		Plan documentReadPlan `json:"Plan"`
	}
	if err = json.Unmarshal(raw, &result); err != nil || len(result) != 1 {
		t.Fatalf("invalid executed plan: %s %v", raw, err)
	}
	evidence, err := json.Marshal(map[string]any{"sql": query, "args": args, "explain": json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return evidence, result[0].Plan
}

type readDocument struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	WarehouseID    string `json:"warehouse_id"`
	OwnerID        string `json:"owner_id"`
	Number         string `json:"number"`
	Lines          []struct {
		LineNo int `json:"line_no"`
	} `json:"lines"`
}

// A small independent model keeps generated pagination checks fast enough for
// every CI run and for repeated mutation campaigns without the large fixture.
func TestDocumentPaginationProperties(t *testing.T) {
	f := newFixture(t)
	noise := id()
	f.exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, noise, f.org)
	f.exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state)
	 SELECT (md5($1::text||':'||g)::uuid),$1::uuid,$2,CASE g%3 WHEN 0 THEN $3::uuid WHEN 1 THEN $4::uuid ELSE $5::uuid END,'shipment','pagination-'||g,'draft'
	 FROM generate_series(1,64) g`, f.org, f.wh, f.owner, f.otherOwner, noise)
	f.exec(`INSERT INTO warehouse.document_lines(document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty)
	 SELECT d.id,d.organization_id,d.warehouse_id,d.owner_id,$1,n,10 FROM warehouse.documents d CROSS JOIN generate_series(1,2) n WHERE d.organization_id=$2 AND d.number LIKE 'pagination-%'`, f.item, f.org)
	type modelDocument struct{ id, owner string }
	var model []modelDocument
	rows, err := f.admin.Query(t.Context(), `SELECT id::text,owner_id::text FROM warehouse.documents WHERE organization_id=$1 AND warehouse_id=$2 ORDER BY id`, f.org, f.wh)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var d modelDocument
		if err = rows.Scan(&d.id, &d.owner); err != nil {
			t.Fatal(err)
		}
		model = append(model, d)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	property := func(pageSize, mask uint8, filter bool) bool {
		scope := inventory.Scope{ActorID: f.actor, OrganizationID: f.org, WarehouseID: f.wh, AllOwners: mask&4 != 0}
		if mask&1 != 0 {
			scope.OwnerIDs = append(scope.OwnerIDs, f.owner)
		}
		if mask&2 != 0 {
			scope.OwnerIDs = append(scope.OwnerIDs, f.otherOwner)
		}
		if mask&8 != 0 {
			scope.OwnerIDs = append(scope.OwnerIDs, f.owner)
		} // Repeated grants must not repeat documents.
		limit := int(pageSize%17) + 1
		filters := inventory.Filters{Limit: limit}
		if filter {
			filters.OwnerID = f.owner
		}
		if filter && !scope.AllowsOwner(f.owner) {
			_, err := f.store.List(t.Context(), inventory.DocumentResource, scope, filters)
			var problem *inventory.Problem
			return errors.As(err, &problem) && problem.Status == 403
		}
		var expected, got []string
		for _, d := range model {
			if scope.AllowsOwner(d.owner) && (!filter || d.owner == f.owner) {
				expected = append(expected, d.id)
			}
		}
		for range len(model) + 1 {
			page, err := f.store.List(t.Context(), inventory.DocumentResource, scope, filters)
			if err != nil || len(page.Data) > limit {
				return false
			}
			for _, raw := range page.Data {
				var d readDocument
				if json.Unmarshal(raw, &d) != nil || d.OrganizationID != f.org || d.WarehouseID != f.wh || !scope.AllowsOwner(d.OwnerID) {
					return false
				}
				if strings.HasPrefix(d.Number, "pagination-") && (len(d.Lines) != 2 || d.Lines[0].LineNo != 1 || d.Lines[1].LineNo != 2) {
					return false
				}
				got = append(got, d.ID)
			}
			if page.NextCursor == "" {
				return slices.Equal(got, expected)
			}
			if len(page.Data) != limit || page.NextCursor == filters.Cursor {
				return false
			}
			filters.Cursor = page.NextCursor
		}
		return false
	}
	if err = quick.Check(property, &quick.Config{MaxCount: 64, Rand: rand.New(rand.NewSource(20261028))}); err != nil {
		t.Fatal(err)
	}
}

// This runs in the standard integration suite: performance regressions cannot
// disappear behind the optional long-running load tag. All rows obey normal FKs.
func TestDocumentReadsStayBoundedAsHistoryGrows(t *testing.T) {
	f, foreign := newFixture(t), newFixture(t)
	otherWarehouse, foreignWarehouse, noiseOwner := id(), id(), id()
	f.exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$1::uuid::text),($3,$4,$3::uuid::text)`, otherWarehouse, f.org, foreignWarehouse, foreign.org)
	f.exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, noiseOwner, f.org)
	capture := captureDocumentReads(t, f)
	targetPrefix := f.wh[:24]
	var reports []map[string]any
	for _, size := range []int{10000, 100000} {
		perScope := size / 4
		for _, scope := range []struct{ org, wh, owner, second, noise, item string }{
			{f.org, f.wh, f.owner, f.otherOwner, noiseOwner, f.item},
			{f.org, otherWarehouse, f.owner, f.otherOwner, noiseOwner, f.item},
			{foreign.org, foreign.wh, foreign.owner, foreign.otherOwner, foreign.otherOwner, foreign.item},
			{foreign.org, foreignWarehouse, foreign.owner, foreign.otherOwner, foreign.otherOwner, foreign.item},
		} {
			start := 1
			if size == 100000 {
				start = 2501
			}
			prefix := scope.wh[:24]
			f.exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state)
			SELECT ($1||lpad(g::text,12,'0'))::uuid,$2,$3,CASE WHEN g%100=0 THEN $4::uuid WHEN g%100=1 THEN $5::uuid ELSE $6::uuid END,'shipment',$1||g,'draft'
			FROM generate_series($7::integer,$8::integer) g`, prefix, scope.org, scope.wh, scope.owner, scope.second, scope.noise, start, perScope)
			f.exec(`INSERT INTO warehouse.document_lines(document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty)
			SELECT d.id,d.organization_id,d.warehouse_id,d.owner_id,$1,n,10 FROM warehouse.documents d CROSS JOIN generate_series(1,3) n
			WHERE d.organization_id=$2 AND d.warehouse_id=$3 AND d.number LIKE $4 AND NOT EXISTS(SELECT 1 FROM warehouse.document_lines l WHERE l.document_id=d.id)`, scope.item, scope.org, scope.wh, prefix+"%")
		}
		f.exec(`ANALYZE warehouse.documents; ANALYZE warehouse.document_lines`)
		for _, tc := range []struct {
			name      string
			owners    []string
			all, deep bool
			filter    string
		}{
			{name: "owner_first", owners: []string{f.owner}},
			{name: "owner_deep", owners: []string{f.owner}, deep: true},
			{name: "owners_first", owners: []string{f.owner, f.otherOwner}},
			{name: "owners_deep", owners: []string{f.owner, f.otherOwner}, deep: true},
			{name: "all_deep", all: true, deep: true},
			{name: "filtered_owner", all: true, filter: f.owner},
		} {
			t.Run(fmt.Sprintf("%d/%s", size, tc.name), func(t *testing.T) {
				scope := inventory.Scope{ActorID: f.actor, OrganizationID: f.org, WarehouseID: f.wh, AllOwners: tc.all, OwnerIDs: tc.owners}
				filters := inventory.Filters{Limit: 20, OwnerID: tc.filter}
				page, err := f.store.List(t.Context(), inventory.DocumentResource, scope, filters)
				if err != nil {
					t.Fatal(err)
				}
				if tc.deep {
					// Rebind a genuine cursor to a known 80th-percentile fixture ID.
					filters.Cursor = documentDeepCursor(t, page.NextCursor, fmt.Sprintf("%s%012d", targetPrefix, perScope*4/5))
					page, err = f.store.List(t.Context(), inventory.DocumentResource, scope, filters)
					if err != nil {
						t.Fatal(err)
					}
				}
				if len(page.Data) == 0 || len(page.Data) > 20 {
					t.Fatalf("unexpected page size %d", len(page.Data))
				}
				last := ""
				for _, raw := range page.Data {
					var row readDocument
					if err = json.Unmarshal(raw, &row); err != nil {
						t.Fatal(err)
					}
					if row.OrganizationID != f.org || row.WarehouseID != f.wh || (!tc.all && !slices.Contains(tc.owners, row.OwnerID)) || (tc.filter != "" && row.OwnerID != tc.filter) {
						t.Fatalf("document scope leaked: %s", raw)
					}
					if last != "" && row.ID <= last {
						t.Fatal("document order is not strictly increasing")
					}
					last = row.ID
					if strings.HasPrefix(row.ID, targetPrefix) && len(row.Lines) != 3 {
						t.Fatalf("document lost lines: %s", raw)
					}
					for i, line := range row.Lines {
						if line.LineNo != i+1 {
							t.Fatalf("document line order changed: %s", raw)
						}
					}
				}
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					raw, plan := explainDocumentRead(t, f, capture, mode)
					blocks, work := plan.Hits+plan.Reads, documentReadPlanRows(plan)
					t.Logf("documents=%d case=%s mode=%s blocks=%d row_work=%.0f", size, tc.name, mode, blocks, work)
					reports = append(reports, map[string]any{"documents": size, "lines": size * 3, "case": tc.name, "mode": mode, "blocks": blocks, "row_work": work, "plan": raw})
					if blocks > 1000 || work > 1000 {
						t.Errorf("document page scanned history: blocks=%d row_work=%.0f for at most 21 documents", blocks, work)
					}
				}
			})
		}
		// Capture the real detail adapter separately, including its line aggregate.
		scope := inventory.Scope{ActorID: f.actor, OrganizationID: f.org, WarehouseID: f.wh, OwnerIDs: []string{f.owner}}
		if _, err := f.store.Get(t.Context(), inventory.DocumentResource, scope, targetPrefix+"000000000100"); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			raw, plan := explainDocumentRead(t, f, capture, mode)
			reports = append(reports, map[string]any{"documents": size, "case": "detail", "mode": mode, "blocks": plan.Hits + plan.Reads, "row_work": documentReadPlanRows(plan), "plan": raw})
			if plan.Hits+plan.Reads > 100 || documentReadPlanRows(plan) > 50 {
				t.Errorf("document detail scanned history: %+v", plan)
			}
		}
	}
	if dir := os.Getenv("WMS_DOCUMENT_PLAN_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		raw, err := json.MarshalIndent(reports, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "document-read-plans.json"), append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
