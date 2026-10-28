//go:build integration && arena

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These are requirements for the reduced-trust threat model, not claims that
// the documented trusted-runtime contract was previously a product bug.
// Every operation under test is an ordinary SELECT or a published function call.
// The dedicated runner creates an isolated local PostgreSQL database and a
// synthetic password-authenticated login; it supplies no API/OIDC context.
type arenaCredentialFixture struct {
	admin, runtime                                               *pgxpool.Pool
	org, wh, owner, actor, unprivileged, item, position          string
	balance, document, line, receiptLine, reservation, operation string
	event, leasedEvent, leaseToken                               string
}

type arenaCredentialObservation struct {
	Case           string `json:"case"`
	Query          string `json:"query"`
	Expected       string `json:"expected"`
	Actual         string `json:"actual"`
	SQLState       string `json:"sqlstate"`
	CommitSQLState string `json:"commit_sqlstate,omitempty"`
	Rows           int64  `json:"rows"`
	SideEffects    bool   `json:"durable_side_effects"`
}

func newArenaCredentialFixture(t *testing.T) *arenaCredentialFixture {
	t.Helper()
	if os.Getenv("WMS_ARENA_LOCAL_ONLY") != "1" {
		t.Fatal("run scripts/security_arena.py for an isolated local database")
	}
	adminDSN, runtimeDSN := os.Getenv("WMS_TEST_ADMIN_DSN"), os.Getenv("WMS_TEST_RUNTIME_DSN")
	if err := validateTestDatabaseDSNs(adminDSN, runtimeDSN); err != nil {
		t.Fatal("invalid isolated laboratory DSNs")
	}
	admin, err := pgxpool.New(t.Context(), adminDSN)
	if err != nil {
		t.Fatal("open laboratory administrator pool")
	}
	t.Cleanup(admin.Close)
	runtime, err := pgxpool.New(t.Context(), runtimeDSN)
	if err != nil {
		t.Fatal("open laboratory runtime pool")
	}
	t.Cleanup(runtime.Close)
	f := &arenaCredentialFixture{admin: admin, runtime: runtime, org: id(), wh: id(), owner: id(), actor: id(), unprivileged: id(), item: id(), position: id(), balance: id(), document: id(), line: id(), receiptLine: id(), reservation: id(), operation: id(), event: id(), leasedEvent: id(), leaseToken: id()}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO platform.organizations(id,code,name) VALUES($1,$1::uuid::text,'Synthetic confidential organization')`, f.org)
	exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'https://arena.invalid',$1::uuid::text),($2,'https://arena.invalid',$2::uuid::text)`, f.actor, f.unprivileged)
	exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, f.owner, f.org)
	exec(`INSERT INTO catalog.units(code,dimension) VALUES('arena_ea','count') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES($1,$2,$1::uuid::text,'arena_ea',3,'none')`, f.item, f.org)
	exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, f.wh, f.org)
	location := id()
	exec(`INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES($1,$2,$3,$1::uuid::text,'storage')`, location, f.org, f.wh)
	exec(`INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES($1,$2,$3,$4)`, f.position, f.org, f.wh, location)
	exec(`INSERT INTO inventory.stock_balances(id,organization_id,warehouse_id,owner_id,item_id,position_id,stock_state,on_hand,reserved_qty) VALUES($1,$2,$3,$4,$5,$6,'available',10,1)`, f.balance, f.org, f.wh, f.owner, f.item, f.position)
	exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES($1,$2,$3,$4,'shipment',$1::uuid::text,'active')`, f.document, f.org, f.wh, f.owner)
	exec(`INSERT INTO warehouse.document_lines(id,document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty,reserved_qty) VALUES($1,$2,$3,$4,$5,$6,1,10,1)`, f.line, f.document, f.org, f.wh, f.owner, f.item)
	receiptDocument := id()
	exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES($1,$2,$3,$4,'receipt',$1::uuid::text,'active')`, receiptDocument, f.org, f.wh, f.owner)
	exec(`INSERT INTO warehouse.document_lines(id,document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty) VALUES($1,$2,$3,$4,$5,$6,1,10)`, f.receiptLine, receiptDocument, f.org, f.wh, f.owner, f.item)
	exec(`INSERT INTO inventory.reservations(id,organization_id,warehouse_id,owner_id,item_id,document_line_id,balance_id,quantity,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,1,clock_timestamp()-interval '1 minute')`, f.reservation, f.org, f.wh, f.owner, f.item, f.line, f.balance)
	exec(`INSERT INTO platform.operations(id,actor_id,organization_id,warehouse_id,operation_type,request,result,completed_at) VALUES($1,$2,$3,$4,'arena_fixture','{}',jsonb_build_object('synthetic_confidential_result','fixture','document_id',$5::uuid),clock_timestamp())`, f.operation, f.actor, f.org, f.wh, f.document)
	movement := id()
	exec(`INSERT INTO inventory.movements(id,operation_id,document_line_id,organization_id,warehouse_id,owner_id,item_id,kind) VALUES($1,$2,$3,$4,$5,$6,$7,'receipt')`, movement, f.operation, f.receiptLine, f.org, f.wh, f.owner, f.item)
	exec(`INSERT INTO inventory.movement_lines(movement_id,target_balance_id,quantity) VALUES($1,$2,1)`, movement, f.balance)
	exec(`INSERT INTO platform.outbox(id,operation_id,event_type,aggregate_id,payload,available_at) VALUES($1,$2,'arena.fixture',$3,'{"synthetic_confidential_payload":"fixture"}','2000-01-01')`, f.event, f.operation, f.document)
	exec(`INSERT INTO platform.outbox(id,operation_id,event_type,aggregate_id,payload,available_at,lease_token,leased_until,attempts) VALUES($1,$2,'arena.leased_fixture',$3,'{}','2000-01-01',$4,clock_timestamp()+interval '1 hour',1)`, f.leasedEvent, f.operation, f.document, f.leaseToken)
	for _, permission := range []string{"stock.read", "reservation.read", "reservation.create", "reservation.release", "movement.read", "movement.create", "operation.read", "document.read", "document.create", "document.transition"} {
		exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, f.actor, f.org, f.wh, f.owner, permission)
	}
	return f
}

func arenaState(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()
	// Hash all mutable business surfaces so a denied operation cannot leave a
	// write to a different fixture unnoticed. The laboratory has no other workers.
	var state string
	err := db.QueryRow(t.Context(), `SELECT md5(jsonb_build_array(
	 (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM warehouse.documents x),
	 (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM warehouse.document_lines x),
	 (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM inventory.stock_balances x),
	 (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM inventory.reservations x),
	 (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM inventory.reservation_events x),
	 (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM inventory.movements x),
	 (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM inventory.movement_lines x),
	 (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM platform.operations x),
	 (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM platform.audit_entries x),
	 (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]') FROM platform.outbox x)
	)::text)`).Scan(&state)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func arenaSQLState(err error) string {
	if err == nil {
		return "00000"
	}
	if problem, ok := errors.AsType[*pgconn.PgError](err); ok {
		return problem.Code
	}
	return "non_sql_error"
}

func arenaLog(t *testing.T, observation arenaCredentialObservation) {
	t.Helper()
	raw, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("ARENA_MATRIX " + string(raw))
}

func TestRedTeamCredentialsDeniedWithoutAPIContext(t *testing.T) {
	type scenario struct {
		name  string
		query func(*arenaCredentialFixture) (string, []any)
		read  bool
	}
	var scenarios []scenario
	for _, view := range []string{"stock_balances", "documents", "document_details", "document_lines", "reservations", "movements", "operations", "actor_grants"} {
		scenarios = append(scenarios, scenario{"read_foreign_organization_" + view, func(f *arenaCredentialFixture) (string, []any) {
			return "SELECT count(*) FROM contracts_v1." + view + " WHERE organization_id=$1", []any{f.org}
		}, true})
	}
	scenarios = append(scenarios,
		scenario{"reserve_without_actor_grants", func(f *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM inventory.reserve_stock($1,$2,$3,1,$4,1)`, []any{id(), f.unprivileged, f.line, f.balance}
		}, false},
		scenario{"reserve_with_supplied_granted_actor", func(f *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM inventory.reserve_stock($1,$2,$3,1,$4,1)`, []any{id(), f.actor, f.line, f.balance}
		}, false},
		scenario{"release_without_actor_grants", func(f *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM inventory.release_reservation($1,$2,$3,1,1,'arena permission regression')`, []any{id(), f.unprivileged, f.reservation}
		}, false},
		scenario{"movement_without_actor_grants", func(f *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,1)`, []any{id(), f.unprivileged, f.receiptLine, f.balance}
		}, false},
		scenario{"prepare_reservation_without_context", func(f *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM inventory.prepare_reservation($1,$2,$3,1,$4,1)`, []any{id(), f.actor, f.line, f.balance}
		}, false},
		scenario{"prepare_release_without_context", func(f *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM inventory.prepare_release($1,$2,$3,1,1,'arena permission regression')`, []any{id(), f.actor, f.reservation}
		}, false},
		scenario{"prepare_movement_without_context", func(f *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM inventory.prepare_movement($1,$2,$3,1,'receipt',NULL,$4,1)`, []any{id(), f.actor, f.receiptLine, f.balance}
		}, false},
		scenario{"ensure_balance_without_context", func(f *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM inventory.ensure_balance($1,$2,$3,$4,$5,'quarantine')`, []any{f.org, f.wh, f.owner, f.item, f.position}
		}, false},
		scenario{"lock_document_without_context", func(f *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM warehouse.lock_document($1,1)`, []any{f.document}
		}, false},
		scenario{"operation_result_supplied_actor_without_context", func(f *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM platform.get_operation_result($1,$2,$3,$4)`, []any{f.operation, f.actor, f.org, f.wh}
		}, false},
		scenario{"document_create_supplied_actor_without_context", func(f *arenaCredentialFixture) (string, []any) {
			input, err := json.Marshal(map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": "receipt", "number": id(), "lines": []map[string]string{{"item_id": f.item, "quantity": "1"}}})
			if err != nil {
				panic(err)
			}
			return `SELECT count(*) FROM warehouse.command_document($1,$2,NULL,$3::jsonb)`, []any{id(), f.actor, string(input)}
		}, false},
		scenario{"background_claim_without_worker_identity", func(_ *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM platform.claim_outbox(100)`, nil
		}, false},
		scenario{"background_complete_without_worker_identity", func(f *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM platform.complete_outbox($1,$2,true)`, []any{f.leasedEvent, f.leaseToken}
		}, false},
		scenario{"background_expiry_without_worker_identity", func(_ *arenaCredentialFixture) (string, []any) {
			return `SELECT count(*) FROM inventory.expire_reservations(1000)`, nil
		}, false},
	)
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			f := newArenaCredentialFixture(t)
			if s.read {
				baseTables := map[string]string{"stock_balances": "inventory.stock_balances", "documents": "warehouse.documents", "document_details": "warehouse.documents", "document_lines": "warehouse.document_lines", "reservations": "inventory.reservations", "movements": "inventory.movements", "operations": "platform.operations", "actor_grants": "platform.actor_grants"}
				var seededRows int64
				if err := f.admin.QueryRow(t.Context(), "SELECT count(*) FROM "+baseTables[strings.TrimPrefix(s.name, "read_foreign_organization_")]+" WHERE organization_id=$1", f.org).Scan(&seededRows); err != nil || seededRows == 0 {
					t.Fatalf("read fixture must exist independently: rows=%d error=%v", seededRows, err)
				}
			}
			before := arenaState(t, f.admin)
			query, args := s.query(f)
			tx, err := f.runtime.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			var rows int64
			err = tx.QueryRow(t.Context(), query, args...).Scan(&rows)
			observation := arenaCredentialObservation{Case: s.name, Query: query, Expected: "denied_without_authenticated_API_or_worker_context", Actual: "allowed", SQLState: arenaSQLState(err), Rows: rows}
			if err != nil {
				_ = tx.Rollback(t.Context())
			} else {
				observation.CommitSQLState = arenaSQLState(tx.Commit(t.Context()))
			}
			observation.SideEffects = before != arenaState(t, f.admin)
			denied := err != nil && (observation.SQLState == "42501" || observation.SQLState == "28000")
			if s.read && err == nil && rows == 0 {
				denied = true
				observation.Actual = "filtered_to_zero_rows"
			}
			if denied && err != nil {
				observation.Actual = "denied"
			}
			arenaLog(t, observation)
			if !denied || observation.SideEffects {
				t.Errorf("missing reduced-trust boundary: actual=%s SQLSTATE=%s commit_SQLSTATE=%s rows=%d durable_side_effects=%v", observation.Actual, observation.SQLState, observation.CommitSQLState, rows, observation.SideEffects)
			}
		})
	}
}

func TestRedTeamCredentialsExistingPrivilegeCharacterization(t *testing.T) {
	f := newArenaCredentialFixture(t)
	for _, role := range []string{"wms_test_owner", "pg_read_server_files", "pg_write_server_files", "pg_execute_server_program"} {
		t.Run("not_member_"+role, func(t *testing.T) {
			var member bool
			if err := f.runtime.QueryRow(t.Context(), `SELECT pg_has_role(current_user,$1,'MEMBER')`, role).Scan(&member); err != nil {
				t.Fatal(err)
			}
			t.Logf("existing boundary: role=%s member=%v", role, member)
			if member {
				t.Errorf("runtime is member of privileged role %s", role)
			}
		})
	}
	for _, table := range []string{"platform.actor_grants", "platform.operations", "platform.outbox", "inventory.stock_balances"} {
		t.Run("base_table_"+table, func(t *testing.T) {
			before := arenaState(t, f.admin)
			var count int64
			err := f.runtime.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&count)
			observation := arenaCredentialObservation{Case: "base_table_" + table, Query: "SELECT count(*) FROM " + table, Expected: "existing_deny", Actual: "denied", SQLState: arenaSQLState(err), Rows: count, SideEffects: before != arenaState(t, f.admin)}
			arenaLog(t, observation)
			if observation.SQLState != "42501" || observation.SideEffects {
				t.Errorf("base table boundary failed: SQLSTATE=%s", observation.SQLState)
			}
		})
	}
}

// Additional ordinary SELECT/EXECUTE requirements against the public profile.
// No signing key or authenticated request context is supplied to this test.
func TestRedTeamCredentialsHardenedRPCDenials(t *testing.T) {
	f := newArenaCredentialFixture(t)
	cases := []struct {
		name, query string
		args        []any
		unsignedGUC bool
	}{
		{name: "private_signing_keys", query: `SELECT count(*) FROM hardened_private.signing_keys`},
		{name: "private_bindings", query: `SELECT count(*) FROM hardened_private.bindings`},
		{name: "private_context", query: `SELECT count(*) FROM hardened_private.context('arena permission regression')`},
		{name: "private_crypto", query: `SELECT count(*) FROM hardened_crypto.hmac('fixture'::text,'fixture'::text,'sha256'::text)`},
		{name: "api_read_without_context", query: `SELECT count(*) FROM hardened_api.read('documents','{"limit":1}')`},
		{name: "api_command_without_context", query: `SELECT count(*) FROM hardened_api.command($1,'document','{}')`, args: []any{id()}},
		{name: "api_resolve_without_context", query: `SELECT count(*) FROM hardened_api.resolve('["https://arena.invalid","fixture"]')`},
		{name: "api_read_unsigned_GUC", query: `SELECT count(*) FROM hardened_api.read('documents','{"limit":1}')`, unsignedGUC: true},
		{name: "api_cannot_use_worker_profile", query: `SELECT count(*) FROM hardened_worker.profile()`},
		{name: "api_cannot_claim_worker_outbox", query: `SELECT count(*) FROM hardened_worker.claim_outbox(1)`},
		{name: "api_cannot_complete_worker_outbox", query: `SELECT count(*) FROM hardened_worker.complete_outbox($1,$2,true)`, args: []any{f.leasedEvent, f.leaseToken}},
		{name: "api_cannot_expire_worker_reservations", query: `SELECT count(*) FROM hardened_worker.expire_reservations(1)`},
		{name: "api_cannot_read_worker_queue", query: `SELECT count(*) FROM hardened_worker.queue_state()`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := arenaState(t, f.admin)
			tx, err := f.runtime.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if c.unsignedGUC {
				// Caller-writable transport without a signature must grant no access.
				if _, err := tx.Exec(t.Context(), `SELECT set_config('wms.context','unsigned-synthetic-fixture',true),set_config('wms.context_mac','',true)`); err != nil {
					t.Fatal(err)
				}
			}
			var rows int64
			err = tx.QueryRow(t.Context(), c.query, c.args...).Scan(&rows)
			observation := arenaCredentialObservation{Case: c.name, Query: c.query, Expected: "denied_without_authenticated_API_or_worker_context", Actual: "allowed", SQLState: arenaSQLState(err), Rows: rows}
			if err != nil {
				_ = tx.Rollback(t.Context())
			} else {
				observation.CommitSQLState = arenaSQLState(tx.Commit(t.Context()))
			}
			observation.SideEffects = before != arenaState(t, f.admin)
			if observation.SQLState == "42501" || observation.SQLState == "28000" {
				observation.Actual = "denied"
			}
			arenaLog(t, observation)
			if observation.Actual != "denied" || observation.SideEffects {
				t.Errorf("hardened permission boundary failed: SQLSTATE=%s rows=%d durable_side_effects=%v", observation.SQLState, rows, observation.SideEffects)
			}
		})
	}
}

func TestRedTeamCredentialsHardenedSafeMetadataCharacterization(t *testing.T) {
	f := newArenaCredentialFixture(t)
	checks := []struct{ name, query string }{
		{"profile_contains_only_public_metadata", `SELECT ((SELECT count(*) FROM jsonb_object_keys(p))=3 AND p->>'profile'='onprem-v1' AND p->>'login'=session_user AND (p->>'installation_id')::uuid<>'00000000-0000-0000-0000-000000000000'::uuid)::text FROM (SELECT hardened_api.profile() AS p) profile`},
		{"api_namespace_usage_without_create", `SELECT (has_schema_privilege(current_user,'hardened_api','USAGE') AND NOT has_schema_privilege(current_user,'hardened_api','CREATE') AND NOT has_database_privilege(current_user,current_database(),'CREATE'))::text`},
		{"core_private_worker_namespaces_are_isolated", `SELECT bool_and(NOT has_schema_privilege(current_user,n.oid,'USAGE') AND NOT has_schema_privilege(current_user,n.oid,'CREATE'))::text FROM pg_namespace n WHERE n.nspname IN ('platform','catalog','warehouse','inventory','contracts_v1','hardened_private','hardened_crypto','hardened_worker')`},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			before := arenaState(t, f.admin)
			var result string
			if err := f.runtime.QueryRow(t.Context(), c.query).Scan(&result); err != nil {
				t.Fatal(err)
			}
			if result != "true" || before != arenaState(t, f.admin) {
				t.Fatalf("safe metadata characterization failed: result=%s", result)
			}
		})
	}
}
