//go:build integration

package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func criticOrderPayload(t *testing.T, raw json.RawMessage, change func(map[string]any)) json.RawMessage {
	t.Helper()
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	change(input)
	result, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestWorkOrdersCriticReplayCannotSwitchOwnerAfterGrantChange(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	backend := orderBackend(t, f.store)
	command := id()
	original := orderSnapshot(f, "4")
	first, err := backend.CommandWorkOrder(t.Context(), orderPrincipal(f), command, original, orderRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), f.owner) {
		t.Fatal("fixture did not create owner A's plan")
	}
	f.exec(`DELETE FROM platform.actor_grants WHERE actor_id=$1 AND organization_id=$2 AND warehouse_id=$3 AND owner_id=$4 AND permission='document.create'`, f.actor, f.org, f.wh, f.owner)
	f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.create')`, f.actor, f.org, f.wh, f.otherOwner)
	principalB := inventory.Principal{ActorID: f.actor, Grants: []inventory.Grant{{OrganizationID: f.org, WarehouseID: f.wh, OwnerID: f.otherOwner, Permission: "document.create"}}}
	forged := criticOrderPayload(t, original, func(input map[string]any) { input["owner_id"] = f.otherOwner })
	result, err := backend.CommandWorkOrder(t.Context(), principalB, command, forged, orderRegistry(t))
	if err == nil {
		t.Fatalf("replay bypassed revoked owner A by checking outer owner B and retaining owner A request: %s", result)
	}
	if got := f.scalar(`SELECT count(*)::text FROM warehouse.work_orders WHERE organization_id=$1`, f.org); got != "1" {
		t.Fatalf("forged replay changed stored plans: %s", got)
	}
}

func TestWorkOrdersCriticRequestScopeMustMatchSnapshot(t *testing.T) {
	for _, field := range []string{"organization_id", "warehouse_id", "owner_id"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t)
			grantDocumentPermissions(f)
			forged := criticOrderPayload(t, orderSnapshot(f, "1"), func(input map[string]any) { input["request"].(map[string]any)[field] = id() })
			_, err := orderBackend(t, f.store).CommandWorkOrder(t.Context(), orderPrincipal(f), id(), forged, orderRegistry(t))
			if err == nil {
				t.Fatal("stored plan and replay identity disagree on " + field)
			}
			if got := f.scalar(`SELECT count(*)::text FROM warehouse.work_orders WHERE organization_id=$1`, f.org); got != "0" {
				t.Fatal("invalid context left a plan")
			}
		})
	}
}

func TestWorkOrdersCriticDatabaseGrantTruthOverridesForgedPrincipal(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	p := orderPrincipal(f)
	f.exec(`DELETE FROM platform.actor_grants WHERE actor_id=$1 AND organization_id=$2 AND permission='document.create'`, f.actor, f.org)
	if _, err := orderBackend(t, f.store).CommandWorkOrder(t.Context(), p, id(), orderSnapshot(f, "1"), orderRegistry(t)); err == nil {
		t.Fatal("cached principal bypassed current database permission")
	}
	if got := f.scalar(`SELECT count(*)::text FROM warehouse.work_orders WHERE organization_id=$1`, f.org); got != "0" {
		t.Fatal("denied plan left rows")
	}
	if got := f.scalar(`SELECT count(*)::text FROM warehouse.work_order_counters WHERE organization_id=$1`, f.org); got != "0" {
		t.Fatal("denied plan consumed number")
	}
	if _, err := f.runtime.Exec(t.Context(), `SELECT * FROM warehouse.work_orders LIMIT 1`); err == nil {
		t.Fatal("compatibility runtime received raw table reads")
	}
	if _, err := f.runtime.Exec(t.Context(), `UPDATE warehouse.work_order_counters SET last_value=0 WHERE organization_id=$1`, f.org); err == nil {
		t.Fatal("compatibility runtime received raw counter writes")
	}
}

func TestWorkOrdersCriticRawSQLRejectsInvisibleBusinessLabels(t *testing.T) {
	for _, tc := range []struct{ name, field, value string }{
		{"blank Unicode customer", "customer", "\u00a0\u2003"},
		{"control customer", "customer", "Fabryka\nAlfa"},
		{"blank Unicode reference", "reference", "\u00a0\u2003"},
		{"control reference", "reference", "ZAM\t1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			grantDocumentPermissions(f)
			raw := criticOrderPayload(t, orderSnapshot(f, "1"), func(input map[string]any) {
				input[tc.field] = tc.value
				input["request"].(map[string]any)[tc.field] = tc.value
			})
			_, err := orderBackend(t, f.store).CommandWorkOrder(t.Context(), orderPrincipal(f), id(), raw, orderRegistry(t))
			if err == nil {
				t.Fatal("raw SQL accepted unusable " + tc.field)
			}
			if got := f.scalar(`SELECT count(*)::text FROM platform.operations WHERE organization_id=$1 AND operation_type='work_order_create'`, f.org); got != "0" {
				t.Fatal("invalid label left completed operation")
			}
		})
	}
}

func TestWorkOrdersCriticRandomPaginationAndCrossScopeCursors(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.read')`, f.actor, f.org, f.wh, f.otherOwner)
	forbidden, otherWh, otherActor := id(), id(), id()
	f.exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, forbidden, f.org)
	f.exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, otherWh, f.org)
	f.exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'work-order-critic',$1::uuid::text)`, otherActor)
	rng := rand.New(rand.NewPCG(20261029, 0x574f524b))
	want := []string{}
	for n := range 192 {
		owner := []string{f.owner, f.otherOwner, forbidden}[rng.IntN(3)]
		warehouse := f.wh
		if rng.IntN(4) == 0 {
			warehouse = otherWh
		}
		identifier := id()
		f.exec(`INSERT INTO warehouse.work_orders(id,organization_id,warehouse_id,owner_id,customer,reference,lines,requirements,steps) VALUES($1,$2,$3,$4,'Klient paginacji',$5,'[]','[]','[]')`, identifier, f.org, warehouse, owner, fmt.Sprintf("CRITIC-%d-%s", n, identifier))
		if warehouse == f.wh && (owner == f.owner || owner == f.otherOwner) {
			want = append(want, identifier)
		}
	}
	slices.Sort(want)
	p := orderPrincipal(f)
	p.Grants = append(p.Grants, inventory.Grant{OrganizationID: f.org, WarehouseID: f.wh, OwnerID: f.otherOwner, Permission: "document.read"})
	scope, err := p.Scope("document.read", f.org, f.wh)
	if err != nil {
		t.Fatal(err)
	}
	backend := orderBackend(t, f.store)
	first, err := backend.ListWorkOrders(t.Context(), scope, inventory.Filters{Limit: 3})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("critic cursor fixture failed: %v", err)
	}
	for _, tc := range []struct {
		name   string
		scope  inventory.Scope
		filter inventory.Filters
	}{
		{"actor", inventory.Scope{ActorID: otherActor, OrganizationID: f.org, WarehouseID: f.wh, OwnerIDs: scope.OwnerIDs}, inventory.Filters{Limit: 3}},
		{"warehouse", inventory.Scope{ActorID: f.actor, OrganizationID: f.org, WarehouseID: otherWh, OwnerIDs: scope.OwnerIDs}, inventory.Filters{Limit: 3}},
		{"owner filter", scope, inventory.Filters{Limit: 3, OwnerID: f.owner}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter := tc.filter
			filter.Cursor = first.NextCursor
			_, err := backend.ListWorkOrders(t.Context(), tc.scope, filter)
			problem, ok := err.(*inventory.Problem)
			if !ok || problem.Status != 400 || problem.Code != "invalid_cursor" {
				t.Fatalf("cursor crossed %s: %v", tc.name, err)
			}
		})
	}
	got := []string{}
	cursor := ""
	for range 192 {
		page, err := backend.ListWorkOrders(t.Context(), scope, inventory.Filters{Limit: 1 + rng.IntN(17), Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range page.Data {
			var row struct{ ID, OwnerID string }
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			got = append(got, row.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !slices.Equal(got, want) {
		t.Fatalf("independent pagination oracle differs: got=%d want=%d equal=%v", len(got), len(want), slices.Equal(got, want))
	}
	// A fabricated all-owner scope still cannot override current SQL grants.
	forged := scope
	forged.AllOwners = true
	forged.OwnerIDs = nil
	page, err := backend.ListWorkOrders(t.Context(), forged, inventory.Filters{Limit: 200})
	if err != nil || len(page.Data) != len(want) {
		t.Fatalf("SQL grant truth diverged for forged scope: %d/%d %v", len(page.Data), len(want), err)
	}
	f.exec(`DELETE FROM platform.actor_grants WHERE actor_id=$1 AND organization_id=$2 AND permission='document.read'`, f.actor, f.org)
	if _, err := backend.ListWorkOrders(t.Context(), forged, inventory.Filters{Limit: 200}); err == nil {
		t.Fatal("revoked SQL read grant bypassed with cached scope")
	}
}

func TestWorkOrdersCriticActualNestedPlansAtScale(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	noiseOwner, otherWarehouse := id(), id()
	f.exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, noiseOwner, f.org)
	f.exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, otherWarehouse, f.org)
	f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.read')`, f.actor, f.org, f.wh, f.otherOwner)
	config, err := pgx.ParseConfig(os.Getenv("WMS_TEST_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	var notices []json.RawMessage
	config.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if at := strings.Index(n.Message, "{"); at >= 0 && json.Valid([]byte(n.Message[at:])) {
			notices = append(notices, json.RawMessage(n.Message[at:]))
		}
	}
	conn, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	_, err = conn.Exec(t.Context(), `LOAD 'auto_explain'; SET auto_explain.log_level='notice'; SET auto_explain.log_format='json'; SET auto_explain.log_analyze=on; SET auto_explain.log_buffers=on; SET auto_explain.log_timing=off; SET auto_explain.log_nested_statements=on; SET auto_explain.log_min_duration=0; SET ROLE wms_go_runtime;`)
	if err != nil {
		t.Fatalf("actual nested plans require the installed PostgreSQL auto_explain module: %v", err)
	}
	prefix := id()[:24]
	plans := []map[string]any{}
	for _, size := range []int{10000, 100000} {
		start := 1
		if size == 100000 {
			start = 5001
		}
		perWarehouse := size / 2
		for _, target := range []bool{true, false} {
			warehouse, offset := otherWarehouse, 1000000
			if target {
				warehouse, offset = f.wh, 0
			}
			f.exec(`INSERT INTO warehouse.work_orders(id,organization_id,warehouse_id,owner_id,customer,reference,lines,requirements,steps)
			SELECT ($1||lpad((g+$7)::text,12,'0'))::uuid,$2,$3,
			CASE WHEN $8::boolean THEN CASE g%1000 WHEN 0 THEN $4::uuid WHEN 1 THEN $5::uuid ELSE $6::uuid END ELSE CASE g%2 WHEN 0 THEN $4::uuid ELSE $5::uuid END END,
			'Klient planów','NESTED-'||g,'[]','[]','[]' FROM generate_series($9::integer,$10::integer) g`, prefix, f.org, warehouse, f.owner, f.otherOwner, noiseOwner, offset, target, start, perWarehouse)
		}
		f.exec(`ANALYZE warehouse.work_orders`)
		for _, deep := range []bool{false, true} {
			after := "00000000-0000-0000-0000-000000000000"
			if deep {
				after = fmt.Sprintf("%s%012d", prefix, perWarehouse*4/5)
			}
			want := []string{}
			for n := 1; n <= perWarehouse; n++ {
				identifier := fmt.Sprintf("%s%012d", prefix, n)
				if n%1000 <= 1 && identifier > after {
					want = append(want, identifier)
				}
			}
			if len(want) > 21 {
				want = want[:21]
			}
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				if _, err = conn.Exec(t.Context(), "SET plan_cache_mode="+mode); err != nil {
					t.Fatal(err)
				}
				notices = nil
				raw, _ := json.Marshal(map[string]any{"limit": 21, "after": after})
				rows, err := conn.Query(t.Context(), `SELECT * FROM warehouse.read_work_orders($1::uuid,$2::uuid,$3::uuid,$4::jsonb)`, f.actor, f.org, f.wh, raw)
				if err != nil {
					t.Fatal(err)
				}
				got := []string{}
				for rows.Next() {
					var data []byte
					if err = rows.Scan(&data); err != nil {
						t.Fatal(err)
					}
					var row struct{ ID string }
					if err = json.Unmarshal(data, &row); err != nil {
						t.Fatal(err)
					}
					got = append(got, row.ID)
				}
				rows.Close()
				if rows.Err() != nil {
					t.Fatal(rows.Err())
				}
				if !slices.Equal(got, want) {
					t.Fatalf("actual RPC differs from independent sparse-owner oracle: got=%v want=%v", got, want)
				}
				found := false
				for _, notice := range notices {
					var plan struct {
						Query string `json:"Query Text"`
						Plan  documentReadPlan
					}
					if err = json.Unmarshal(notice, &plan); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(plan.Query, "WITH page AS MATERIALIZED") {
						continue
					}
					found = true
					work, blocks := documentReadPlanRows(plan.Plan), plan.Plan.Hits+plan.Plan.Reads
					plans = append(plans, map[string]any{"orders": size, "deep": deep, "mode": mode, "row_work": work, "blocks": blocks, "explain": notice})
					t.Logf("actual nested RPC orders=%d deep=%t mode=%s work=%.0f blocks=%d", size, deep, mode, work, blocks)
					if work <= 0 || work > 1000 || blocks <= 0 || blocks > 1000 {
						t.Errorf("work-order page scanned history instead of seeking its scoped candidates: work=%.0f blocks=%d", work, blocks)
					}
					criticAssertNestedCandidateBounds(t, notice)
				}
				if !found {
					t.Fatalf("no actual nested execution plan captured; received%d notices", len(notices))
				}
			}
		}
	}
	directory := os.Getenv("WMS_WORK_ORDER_PLAN_DIR")
	if directory == "" {
		directory = filepath.Join("..", "..", "work", "work-order-plans")
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", "0013_work_orders.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 8 {
		t.Errorf("incomplete nested plan evidence: %d/8", len(plans))
	}
	var functionDefinition string
	if err = f.admin.QueryRow(t.Context(), `SELECT pg_get_functiondef('warehouse.read_work_orders(uuid,uuid,uuid,jsonb)'::regprocedure)`).Scan(&functionDefinition); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(map[string]any{"collection": "Actual auto_explain nested plans from warehouse.read_work_orders, runtime role; no copied query and no global server settings", "plans": plans, "complete": len(plans) == 8, "passed": !t.Failed(), "migration_sha256": fmt.Sprintf("%x", sha256.Sum256(source)), "actual_read_function_sha256_lf": fmt.Sprintf("%x", sha256.Sum256([]byte(strings.ReplaceAll(functionDefinition, "\r\n", "\n"))))}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "work-order-read-plans.json"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func criticAssertNestedCandidateBounds(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var evidence struct{ Plan map[string]any }
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	seeks := 0
	hydrated := float64(0)
	var walk func(map[string]any, bool)
	walk = func(node map[string]any, candidate bool) {
		candidate = candidate || node["Subplan Name"] == "CTE page"
		if candidate {
			for _, field := range []string{"Rows Removed by Filter", "Rows Removed by Index Recheck"} {
				if value, ok := node[field].(float64); ok && value > 0 {
					t.Errorf("candidate scan discarded unrelated rows: %s=%v", field, value)
				}
			}
			if condition, ok := node["Index Cond"].(string); ok {
				seeks++
				for _, field := range []string{"organization_id", "owner_id", "warehouse_id"} {
					if !strings.Contains(condition, field) {
						t.Errorf("candidate seek omits %s: %s", field, condition)
					}
				}
				if !regexp.MustCompile(`\bid\s*>\s*`).MatchString(condition) {
					t.Errorf("cursor was not applied inside candidate seek: %s", condition)
				}
			}
		} else if node["Relation Name"] == "work_orders" {
			rows, _ := node["Actual Rows"].(float64)
			loops, _ := node["Actual Loops"].(float64)
			hydrated += rows * loops
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				if value, ok := child.(map[string]any); ok {
					walk(value, candidate)
				}
			}
		}
	}
	walk(evidence.Plan, false)
	if seeks == 0 {
		t.Error("actual nested work-order page did not execute a scoped index seek")
	}
	if hydrated > 21 {
		t.Errorf("hydrated %.0f work orders for a21-row page", hydrated)
	}
}
