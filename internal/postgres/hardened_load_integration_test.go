//go:build integration && hardened

package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type hardenedOracleDocument struct{ ID, Owner string }

func TestHardenedDocumentReadsAtScale(t *testing.T) {
	for _, variable := range []string{"WMS_HARDENED_ADMIN_DSN", "WMS_HARDENED_API_DSN", "WMS_HARDENED_KEY", "WMS_HARDENED_FIXTURE"} {
		if os.Getenv(variable) == "" {
			t.Fatalf("%s is required; run the disposable hardened profile fixture", variable)
		}
	}
	connect := func(variable string) *pgxpool.Pool {
		t.Helper()
		pool, err := pgxpool.New(t.Context(), os.Getenv(variable))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	admin, runtime := connect("WMS_HARDENED_ADMIN_DSN"), connect("WMS_HARDENED_API_DSN")
	key, err := base64.StdEncoding.DecodeString(os.Getenv("WMS_HARDENED_KEY"))
	if err != nil {
		t.Fatal("invalid synthetic request key")
	}
	var fixture map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("WMS_HARDENED_FIXTURE")), &fixture); err != nil {
		t.Fatal(err)
	}
	store, err := postgres.NewHardened(t.Context(), runtime, key)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	var oracle []hardenedOracleDocument
	rows, err := admin.Query(t.Context(), `SELECT id::text,owner_id::text FROM warehouse.documents WHERE organization_id=$1 AND warehouse_id=$2`, fixture["org"], fixture["wh"])
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var d hardenedOracleDocument
		if err := rows.Scan(&d.ID, &d.Owner); err != nil {
			t.Fatal(err)
		}
		oracle = append(oracle, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	actor, prefix := id(), id()[:24]
	owners := []string{id(), id(), id(), id(), id()}
	exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'test',$1::uuid::text)`, actor)
	for _, owner := range owners {
		exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, owner, fixture["org"])
	}
	exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state)
	 SELECT ($1||lpad(g::text,12,'0'))::uuid,$2,$3,
	 ($4::text[])[CASE WHEN g<=99000 THEN 1 ELSE 2+(g-99001)%4 END]::uuid,'shipment',$1||g,'draft'
	 FROM generate_series(1,100000) g`, prefix, fixture["org"], fixture["wh"], owners)
	// Refresh parent cardinality before hundreds of thousands of FK lookups;
	// otherwise a cached small-table RI plan can dominate fixture construction.
	exec(`ANALYZE warehouse.documents`)
	exec(`INSERT INTO warehouse.document_lines(document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty)
	 SELECT d.id,d.organization_id,d.warehouse_id,d.owner_id,$1,n,10
	 FROM warehouse.documents d CROSS JOIN generate_series(1,3) n WHERE left(d.id::text,24)=$2`, fixture["item"], prefix)
	exec(`ANALYZE warehouse.documents; ANALYZE warehouse.document_lines; ANALYZE platform.actor_grants`)
	for n := 1; n <= 100000; n++ {
		owner := owners[0]
		if n > 99000 {
			owner = owners[1+(n-99001)%4]
		}
		oracle = append(oracle, hardenedOracleDocument{ID: fmt.Sprintf("%s%012d", prefix, n), Owner: owner})
	}
	slices.SortFunc(oracle, func(a, b hardenedOracleDocument) int { return strings.Compare(a.ID, b.ID) })
	var reports []map[string]any
	var timings []time.Duration
	propertyCases := 0
	defer func() {
		directory := os.Getenv("WMS_HARDENED_PLAN_DIR")
		if directory == "" {
			directory = filepath.Join(t.TempDir(), "hardened-plans")
			t.Logf("WMS_HARDENED_PLAN_DIR is unset; plans are temporary at %s", directory)
		}
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Error(err)
			return
		}
		profile, _ := os.ReadFile("../../db/profiles/hardened-v1.sql")
		forward, _ := os.ReadFile("../../db/profiles/onprem-v1.sql")
		profile = append(profile, forward...)
		testSource, _ := os.ReadFile("hardened_load_integration_test.go")
		profileHash, testHash := sha256.Sum256(profile), sha256.Sum256(testSource)
		slices.Sort(timings)
		var p95 time.Duration
		if len(timings) > 0 {
			p95 = timings[(len(timings)-1)*95/100]
		}
		artifact := map[string]any{"seeded_documents": 100000, "seeded_lines": 300000, "property_cases": propertyCases,
			"rpc_calls": len(timings), "rpc_p95_ms": float64(p95.Microseconds()) / 1000,
			"profile_sha256": hex.EncodeToString(profileHash[:]), "test_sha256": hex.EncodeToString(testHash[:]), "plans": reports,
			"limit": "Local laboratory fixture and structural work bounds; no production latency/SLA claim."}
		raw, err := json.MarshalIndent(artifact, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(directory, "hardened-read-plans.json"), append(raw, '\n'), 0600); err != nil {
			t.Error(err)
		}
	}()
	setGrants := func(permitted []string, all bool) inventory.Scope {
		t.Helper()
		exec(`DELETE FROM platform.actor_grants WHERE actor_id=$1`, actor)
		if all {
			exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,permission) VALUES($1,$2,$3,'document.read')`, actor, fixture["org"], fixture["wh"])
		} else {
			for _, owner := range permitted {
				exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.read')`, actor, fixture["org"], fixture["wh"], owner)
			}
		}
		return inventory.Scope{ActorID: actor, OrganizationID: fixture["org"], WarehouseID: fixture["wh"], AllOwners: all, OwnerIDs: permitted}
	}
	list := func(scope inventory.Scope, filters inventory.Filters) (inventory.Page, error) {
		start := time.Now()
		page, err := store.List(t.Context(), inventory.DocumentResource, scope, filters)
		timings = append(timings, time.Since(start))
		return page, err
	}
	ids := func(page inventory.Page) ([]string, error) {
		result := make([]string, 0, len(page.Data))
		for _, raw := range page.Data {
			var d struct {
				ID    string
				Lines []json.RawMessage
			}
			if err := json.Unmarshal(raw, &d); err != nil {
				return nil, err
			}
			if strings.HasPrefix(d.ID, prefix) && len(d.Lines) != 3 {
				return nil, fmt.Errorf("document %s has %d lines, want 3", d.ID, len(d.Lines))
			}
			result = append(result, d.ID)
		}
		return result, nil
	}
	expected := func(scope inventory.Scope, owner, after string) []string {
		result := []string{}
		for _, d := range oracle {
			if d.ID > after && (scope.AllOwners || slices.Contains(scope.OwnerIDs, d.Owner)) && (owner == "" || d.Owner == owner) {
				result = append(result, d.ID)
			}
		}
		return result
	}
	t.Run("independent randomized pagination oracle", func(t *testing.T) {
		property := func(seed uint64) bool {
			propertyCases++
			random := rand.New(rand.NewSource(int64(seed)))
			var permitted []string
			for _, owner := range owners {
				if random.Intn(2) == 1 {
					permitted = append(permitted, owner)
				}
			}
			if len(permitted) == 0 {
				permitted = append(permitted, owners[1])
			}
			all := random.Intn(6) == 0
			scope := setGrants(permitted, all)
			filters := inventory.Filters{Limit: []int{1, 7, 23, 50, 97}[random.Intn(5)]}
			if random.Intn(3) == 0 {
				filters.OwnerID = permitted[random.Intn(len(permitted))]
			}
			first, err := list(scope, filters)
			if err != nil {
				t.Logf("case%d first page: %v", propertyCases, err)
				return false
			}
			wantFirst := expected(scope, filters.OwnerID, "")
			if len(wantFirst) > filters.Limit {
				wantFirst = wantFirst[:filters.Limit]
			}
			gotFirst, err := ids(first)
			if err != nil || !slices.Equal(gotFirst, wantFirst) {
				t.Logf("case%d first-page oracle mismatch: %v", propertyCases, err)
				return false
			}
			if first.NextCursor == "" {
				t.Logf("case%d missing next cursor", propertyCases)
				return false
			}
			after := fmt.Sprintf("%s%012d", prefix, 99760+random.Intn(100))
			filters.Cursor = documentDeepCursor(t, first.NextCursor, after)
			want := expected(scope, filters.OwnerID, after)
			var got []string
			for pages := range 1000 {
				page, err := list(scope, filters)
				if err != nil {
					t.Logf("case%d page%d: %v", propertyCases, pages, err)
					return false
				}
				part, err := ids(page)
				if err != nil || len(part) > filters.Limit {
					t.Logf("invalid page%d: %v", pages, err)
					return false
				}
				got = append(got, part...)
				if page.NextCursor == "" {
					return slices.Equal(got, want)
				}
				filters.Cursor = page.NextCursor
			}
			return false
		}
		if err := quick.Check(property, &quick.Config{MaxCount: 30, Rand: rand.New(rand.NewSource(20261028))}); err != nil {
			t.Fatal(err)
		}
	})

	// Capture the exact internally generated SELECT without copying its SQL into
	// the test. This owner-only instrumentation changes no query or parameters.
	var original, owner string
	if err := admin.QueryRow(t.Context(), `SELECT pg_get_functiondef(p.oid),pg_get_userbyid(p.proowner) FROM pg_proc p WHERE p.oid='hardened_api.read(text,text)'::regprocedure`).Scan(&original, &owner); err != nil {
		t.Fatal(err)
	}
	if strings.Count(original, "RETURN QUERY EXECUTE format(") != 1 || strings.Count(original, "\n USING c.organization_id") != 1 {
		t.Fatal("cannot instrument current read RPC exactly")
	}
	exec(`CREATE TABLE hardened_private.load_capture(id bigint GENERATED ALWAYS AS IDENTITY,query text NOT NULL);
	 CREATE FUNCTION hardened_private.capture_query(p_sql text) RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN INSERT INTO hardened_private.load_capture(query) VALUES(p_sql); RETURN p_sql; END $$`)
	exec("GRANT EXECUTE ON FUNCTION hardened_private.capture_query(text) TO " + pgx.Identifier{owner}.Sanitize())
	instrumented := strings.Replace(original, "RETURN QUERY EXECUTE format(", "RETURN QUERY EXECUTE hardened_private.capture_query(format(", 1)
	instrumented = strings.Replace(instrumented, "\n USING c.organization_id", ")\n USING c.organization_id", 1)
	exec(instrumented)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, original+`; DROP FUNCTION hardened_private.capture_query(text); DROP TABLE hardened_private.load_capture`); err != nil {
			t.Error(err)
		}
	})
	for _, tc := range []struct {
		name                string
		permitted           []string
		all, deep, filtered bool
	}{
		{"one-owner-first", owners[1:2], false, false, false}, {"one-owner-deep", owners[1:2], false, true, false},
		{"multiple-owner-first", owners[1:4], false, false, false}, {"multiple-owner-deep", owners[1:4], false, true, false},
		{"all-owner-first", nil, true, false, false}, {"all-owner-deep", nil, true, true, false},
		{"explicit-owner", owners[1:4], false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := setGrants(tc.permitted, tc.all)
			filters := inventory.Filters{Limit: 20}
			if tc.filtered {
				filters.OwnerID = owners[1]
			}
			after := ""
			if tc.deep {
				first, err := list(scope, filters)
				if err != nil {
					t.Fatal(err)
				}
				after = fmt.Sprintf("%s%012d", prefix, 99500)
				filters.Cursor = documentDeepCursor(t, first.NextCursor, after)
			}
			page, err := list(scope, filters)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ids(page)
			want := expected(scope, filters.OwnerID, after)
			if len(want) > 20 {
				want = want[:20]
			}
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("large-page oracle mismatch: %v", err)
			}
			var query string
			if err := admin.QueryRow(t.Context(), `SELECT query FROM hardened_private.load_capture ORDER BY id DESC LIMIT 1`).Scan(&query); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				planRaw, plan := hardenedExplainRead(t, admin, owner, query, mode, actor, fixture["org"], fixture["wh"], after)
				work, blocks := documentReadPlanRows(plan), plan.Hits+plan.Reads
				hydrations := hardenedLineHydrations(plan)
				reports = append(reports, map[string]any{"case": tc.name, "mode": mode, "sql": query, "after": after, "page_limit": 21, "row_work": work, "blocks": blocks, "line_hydrations": hydrations, "explain": json.RawMessage(planRaw)})
				t.Logf("documents=100000 case=%s mode=%s blocks=%d row_work=%.0f line_hydrations=%.0f", tc.name, mode, blocks, work, hydrations)
				if work > 2000 || blocks > 3000 {
					t.Errorf("read page scanned history instead of bounded candidates: blocks=%d row_work=%.0f", blocks, work)
				}
				if hydrations > 21 {
					t.Errorf("read hydrated %.0f documents for a 21-document candidate limit", hydrations)
				}
			}
		})
	}
}

func hardenedLineHydrations(plan documentReadPlan) float64 {
	loops := 0.0
	if plan.Relation == "document_lines" {
		loops = plan.Loops
	}
	for _, child := range plan.Plans {
		loops += hardenedLineHydrations(child)
	}
	return loops
}

func hardenedExplainRead(t *testing.T, pool *pgxpool.Pool, owner, query, mode, actor, org, warehouse, after string) ([]byte, documentReadPlan) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(ctx)
	}()
	for _, sql := range []string{"SET LOCAL ROLE " + pgx.Identifier{owner}.Sanitize(), "SET LOCAL plan_cache_mode=" + mode,
		"PREPARE hardened_load_plan(uuid,uuid,hardened_private.request_context,text,uuid,integer,uuid) AS " + query} {
		if _, err := tx.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	afterArg := "NULL"
	if after != "" {
		afterArg = quote(after)
	}
	arguments := fmt.Sprintf("%s,%s,ROW(%s::uuid,%s::uuid,%s::uuid)::hardened_private.request_context,'document.read',%s,21,NULL", quote(org), quote(warehouse), quote(actor), quote(org), quote(warehouse), afterArg)
	var raw []byte
	if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,SETTINGS,FORMAT JSON) EXECUTE hardened_load_plan("+arguments+")").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var result []struct {
		Plan documentReadPlan `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || len(result) != 1 {
		t.Fatalf("invalid actual plan: %v", err)
	}
	if _, err := tx.Exec(t.Context(), "DEALLOCATE hardened_load_plan"); err != nil {
		t.Fatal(err)
	}
	return raw, result[0].Plan
}
