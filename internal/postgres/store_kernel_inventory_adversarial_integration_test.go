//go:build integration

package postgres_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func assertKernelReconciles(t *testing.T, f *fixture) {
	t.Helper()
	queries := []string{
		`WITH r AS (SELECT balance_id,sum(active_qty) qty FROM inventory.reservations GROUP BY balance_id)
		 SELECT count(*) FROM inventory.stock_balances b LEFT JOIN r ON r.balance_id=b.id WHERE b.organization_id=$1 AND b.reserved_qty<>coalesce(r.qty,0)`,
		`WITH r AS (SELECT document_line_id,sum(active_qty) qty FROM inventory.reservations GROUP BY document_line_id)
		 SELECT count(*) FROM warehouse.document_lines l LEFT JOIN r ON r.document_line_id=l.id WHERE l.organization_id=$1 AND l.reserved_qty<>coalesce(r.qty,0)`,
		`WITH deltas AS (SELECT target_balance_id id,quantity qty FROM inventory.movement_lines WHERE target_balance_id IS NOT NULL UNION ALL SELECT source_balance_id,-quantity FROM inventory.movement_lines WHERE source_balance_id IS NOT NULL), total AS (SELECT id,sum(qty) qty FROM deltas GROUP BY id)
		 SELECT count(*) FROM inventory.stock_balances b LEFT JOIN total ON total.id=b.id WHERE b.organization_id=$1 AND b.on_hand<>coalesce(total.qty,0)`,
	}
	for i, query := range queries {
		if got := f.scalar(query, f.org); got != "0" {
			t.Fatalf("accounting reconciliation %d found %s mismatches", i, got)
		}
	}
}

func kernelSnapshot(f *fixture) string {
	return f.scalar(`SELECT jsonb_build_object(
	 'balances',(SELECT jsonb_agg(to_jsonb(b) ORDER BY id) FROM inventory.stock_balances b WHERE organization_id=$1),
	 'lines',(SELECT jsonb_agg(to_jsonb(l) ORDER BY id) FROM warehouse.document_lines l WHERE organization_id=$1),
	 'documents',(SELECT jsonb_agg(to_jsonb(d) ORDER BY id) FROM warehouse.documents d WHERE organization_id=$1),
	 'operations',(SELECT count(*) FROM platform.operations WHERE organization_id=$1),
	 'movements',(SELECT count(*) FROM inventory.movements WHERE organization_id=$1),
	 'audit',(SELECT count(*) FROM platform.audit_entries a JOIN platform.operations p ON p.id=a.operation_id WHERE p.organization_id=$1),
	 'outbox',(SELECT count(*) FROM platform.outbox o JOIN platform.operations p ON p.id=o.operation_id WHERE p.organization_id=$1))::text`, f.org)
}

func TestKernelInventoryGeneratedWorkflowConservesLedger(t *testing.T) {
	config := &quick.Config{MaxCount: 32, Rand: rand.New(rand.NewSource(2026102801))}
	if err := quick.Check(func(seed uint64) bool {
		f := newFixture(t)
		h := f.handler(nil, nil)
		quantity := func(q uint64) string { return fmt.Sprintf("%d.%03d", q/1000, q%1000) }
		q := 4 + seed%9996
		pick := 1 + (seed>>12)%(q-2)
		release := 1 + (seed>>26)%(q-pick-1)
		version := 1
		command := func(path string, body map[string]any) map[string]any {
			t.Helper()
			key := id()
			before := f.scalar(`SELECT count(*) FROM platform.operations WHERE organization_id=$1`, f.org)
			response := f.request(h, "POST", path, body, key)
			requireStatus(t, response, 201)
			snapshot := kernelSnapshot(f)
			for range 2 {
				replay := f.request(h, "POST", path, body, key)
				requireStatus(t, replay, 201)
				if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) || kernelSnapshot(f) != snapshot {
					t.Fatalf("seed=%d replay changed result or state (operations before=%s)", seed, before)
				}
			}
			for _, table := range []string{"operations", "audit_entries", "outbox"} {
				column := "operation_id"
				if table == "operations" {
					column = "id"
				}
				if got := f.scalar(`SELECT count(*) FROM platform.`+table+` WHERE `+column+`=$1`, key); got != "1" {
					t.Fatalf("seed=%d %s effects=%s", seed, table, got)
				}
			}
			assertKernelReconciles(t, f)
			version++
			return data(t, response)
		}
		reserved := command("/api/v1/reservations", f.reserveBody(f.line, quantity(q), version))["reservation_id"].(string)
		picked := command("/api/v1/movements", f.movementBody(f.line, "transfer", quantity(pick), version, reserved))["new_reservation_id"].(string)
		command("/api/v1/reservations/"+reserved+"/releases", map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": version, "quantity": quantity(release), "reason": "generated partial cancellation"})
		body := f.movementBody(f.line, "issue", quantity(pick), version, picked)
		body["source_balance_id"] = f.target
		command("/api/v1/movements", body)
		command("/api/v1/movements", f.movementBody(f.line, "issue", quantity(q-pick-release), version, reserved))
		want := fmt.Sprintf("%d.%06d", (q-release)/1000, (q-release)%1000*1000)
		if got := f.scalar(`SELECT fulfilled_qty::text||':'||reserved_qty::text FROM warehouse.document_lines WHERE id=$1`, f.line); got != want+":0.000000" {
			t.Logf("seed=%d fulfilled=%s want=%s", seed, got, want)
			return false
		}
		return true
	}, config); err != nil {
		t.Fatal(err)
	}
}

func TestKernelInventoryBusinessRejectionsAreAtomic(t *testing.T) {
	tests := []struct{ name, kind, qty, wantCode string }{
		{"unavailable stock", "reserve", "10.001", "insufficient_available_stock"},
		{"item precision", "reserve", "0.0001", "invalid_quantity"},
		{"stale document", "stale", "1", "document_version_conflict"},
		{"inactive document", "inactive", "1", "document_not_active"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			h := f.handler(nil, nil)
			version := 1
			if tc.kind == "stale" {
				version = 77
			}
			if tc.kind == "inactive" {
				f.exec(`UPDATE warehouse.documents SET state='draft' WHERE id=$1`, f.document)
			}
			snapshot := kernelSnapshot(f)
			response := f.request(h, "POST", "/api/v1/reservations", f.reserveBody(f.line, tc.qty, version), id())
			if response.Code < 400 || !strings.Contains(response.Body.String(), tc.wantCode) {
				t.Fatalf("error=%d %s want=%s", response.Code, response.Body, tc.wantCode)
			}
			if kernelSnapshot(f) != snapshot {
				t.Fatal("rejected command changed persistent accounting or effects")
			}
			assertKernelReconciles(t, f)
		})
	}
}

func TestKernelInventoryReleaseAndExpiryKeepExactEffects(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	body := f.reserveBody(f.line, "4.125", 1)
	body["expires_at"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	w := f.request(h, "POST", "/api/v1/reservations", body, id())
	requireStatus(t, w, 201)
	reservation := data(t, w)["reservation_id"].(string)
	key := id()
	release := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 2, "quantity": "1.125", "reason": "partial cancellation"}
	w = f.request(h, "POST", "/api/v1/reservations/"+reservation+"/releases", release, key)
	requireStatus(t, w, 201)
	snapshot := kernelSnapshot(f)
	bad := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 3, "quantity": "3.001", "reason": "overrelease"}
	w = f.request(h, "POST", "/api/v1/reservations/"+reservation+"/releases", bad, id())
	requireStatus(t, w, 409)
	if !strings.Contains(w.Body.String(), "insufficient_active_reservation") || kernelSnapshot(f) != snapshot {
		t.Fatalf("overrelease was not atomically rejected: %s", w.Body)
	}
	f.exec(`UPDATE inventory.reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, reservation)
	var expired int
	if err := f.runtime.QueryRow(t.Context(), `SELECT inventory.expire_reservations(10)`).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if expired != 1 {
		t.Fatalf("expired=%d", expired)
	}
	if err := f.runtime.QueryRow(t.Context(), `SELECT inventory.expire_reservations(10)`).Scan(&expired); err != nil || expired != 0 {
		t.Fatalf("repeat expiry=%d err=%v", expired, err)
	}
	if got := f.scalar(`SELECT reserved_qty::text FROM inventory.stock_balances WHERE id=$1`, f.source); got != "0.000000" {
		t.Fatalf("expired balance holds %s", got)
	}
	if got := f.scalar(`SELECT sum(quantity)::text FROM inventory.reservation_events WHERE reservation_id=$1 AND kind='released'`, reservation); got != "4.125000" {
		t.Fatalf("released event total=%s", got)
	}
	assertKernelReconciles(t, f)
}

func TestKernelInventoryDirectCommandsRejectStaleVersionAndWrongKind(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		version    int
		code       string
	}{
		{"stale version", "shipment", 99, "document_version_conflict"},
		{"reservation on receipt", "receipt", 1, "reservation_requires_shipment"},
		{"inactive document", "inactive", 1, "document_not_active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			doc, line := f.document, f.line
			if tc.kind == "inactive" {
				f.exec(`UPDATE warehouse.documents SET state='draft' WHERE id=$1`, f.document)
			} else if tc.kind != "shipment" {
				doc, line = f.doc(tc.kind, "2", f.owner)
			}
			_ = doc
			snapshot := kernelSnapshot(f)
			_, err := f.runtime.Exec(t.Context(), `SELECT inventory.reserve_stock($1,$2,$3,$4,$5,1)`, id(), f.actor, line, tc.version, f.source)
			var problem *pgconn.PgError
			if !errors.As(err, &problem) || problem.Message != tc.code {
				t.Fatalf("direct kernel error=%v want=%s", err, tc.code)
			}
			if kernelSnapshot(f) != snapshot {
				t.Fatal("failed direct kernel command changed persistent state")
			}
		})
	}
}

func TestKernelInventoryCompetingDocumentsCannotOverdrawSharedStock(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	_, second := f.doc("shipment", "10", f.owner)
	type result struct {
		status int
		body   []byte
	}
	results := make(chan result, 2)
	for _, line := range []string{f.line, second} {
		go func() {
			w := f.request(h, "POST", "/api/v1/reservations", f.reserveBody(line, "6", 1), id())
			results <- result{w.Code, bytes.Clone(w.Body.Bytes())}
		}()
	}
	success, conflict := 0, 0
	for range 2 {
		got := <-results
		switch got.status {
		case 201:
			success++
		case 409:
			conflict++
		default:
			t.Fatalf("concurrent result=%d %s", got.status, got.body)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	assertKernelReconciles(t, f)
	if got := f.scalar(`SELECT reserved_qty::text FROM inventory.stock_balances WHERE id=$1`, f.source); got != "6.000000" {
		t.Fatalf("last-stock competition held %s", got)
	}
}

func TestKernelInventoryTrackingMismatchCannotRelabelStock(t *testing.T) {
	f := newFixture(t)
	item, lotA, lotB := id(), id(), id()
	f.exec(`INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES($1,$2,$1::uuid::text,'ea',3,'lot')`, item, f.org)
	f.exec(`INSERT INTO catalog.lots(id,organization_id,item_id,code) VALUES($1,$3,$4,'lot-a'),($2,$3,$4,'lot-b')`, lotA, lotB, f.org, item)
	positions := []string{f.scalar(`SELECT position_id::text FROM inventory.stock_balances WHERE id=$1`, f.source), f.scalar(`SELECT position_id::text FROM inventory.stock_balances WHERE id=$1`, f.target)}
	source := f.scalar(`SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available',$6,NULL)::text`, f.org, f.wh, f.owner, item, positions[0], lotA)
	target := f.scalar(`SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available',$6,NULL)::text`, f.org, f.wh, f.owner, item, positions[1], lotB)
	f.item = item
	_, receipt := f.doc("receipt", "3", f.owner)
	f.exec(`SELECT inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,3)`, id(), f.actor, receipt, source)
	_, transfer := f.doc("transfer", "3", f.owner)
	h := f.handler(nil, nil)
	snapshot := kernelSnapshot(f)
	body := f.movementBody(transfer, "transfer", "3", 1, "")
	body["source_balance_id"], body["target_balance_id"] = source, target
	w := f.request(h, "POST", "/api/v1/movements", body, id())
	requireStatus(t, w, 409)
	if !strings.Contains(w.Body.String(), "transfer_tracking_mismatch") || kernelSnapshot(f) != snapshot {
		t.Fatalf("tracking mismatch was not atomic: %s", w.Body)
	}
	assertKernelReconciles(t, f)
}

func TestKernelInventoryExpiredReservationCannotShipButOriginalCommandReplays(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	key := id()
	body := f.reserveBody(f.line, "2", 1)
	body["expires_at"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	created := f.request(h, "POST", "/api/v1/reservations", body, key)
	requireStatus(t, created, 201)
	reservation := data(t, created)["reservation_id"].(string)
	f.exec(`UPDATE inventory.reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, reservation)
	snapshot := kernelSnapshot(f)
	w := f.request(h, "POST", "/api/v1/movements", f.movementBody(f.line, "issue", "1", 2, reservation), id())
	requireStatus(t, w, 409)
	if !strings.Contains(w.Body.String(), "reservation_expired") || kernelSnapshot(f) != snapshot {
		t.Fatalf("expired reservation shipped or changed effects: %s", w.Body)
	}
	replay := f.request(h, "POST", "/api/v1/reservations", body, key)
	requireStatus(t, replay, 201)
	if !bytes.Equal(replay.Body.Bytes(), created.Body.Bytes()) || kernelSnapshot(f) != snapshot {
		t.Fatal("completed command replay changed after reservation expiry")
	}
	assertKernelReconciles(t, f)
}

func TestKernelInventoryCorrectionCannotReferenceDifferentScope(t *testing.T) {
	f, other := newFixture(t), newFixture(t)
	foreignMovement := other.scalar(`SELECT id::text FROM inventory.movements WHERE organization_id=$1 LIMIT 1`, other.org)
	_, line := f.doc("adjustment", "1", f.owner)
	snapshot := kernelSnapshot(f)
	_, err := f.runtime.Exec(t.Context(), `SELECT inventory.post_movement($1,$2,$3,1,'adjustment_out',$4,NULL,1,NULL,'correction review',$5)`, id(), f.actor, line, f.source, foreignMovement)
	var problem *pgconn.PgError
	if !errors.As(err, &problem) || problem.Message != "correction_scope_mismatch" {
		t.Fatalf("correction error=%v", err)
	}
	if kernelSnapshot(f) != snapshot {
		t.Fatal("foreign correction changed accounting")
	}
}

func TestKernelInventoryCommandReplayBindsExactRequest(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	key := id()
	body := f.reserveBody(f.line, "2", 1)
	w := f.request(h, "POST", "/api/v1/reservations", body, key)
	requireStatus(t, w, 201)
	snapshot := kernelSnapshot(f)
	changed := f.reserveBody(f.line, "3", 1)
	w = f.request(h, "POST", "/api/v1/reservations", changed, key)
	requireStatus(t, w, 409)
	if !strings.Contains(w.Body.String(), "idempotency_key_reused") || kernelSnapshot(f) != snapshot {
		t.Fatalf("command ID lost exact input binding: %s", w.Body)
	}
	other := newFixture(t)
	_, err := f.runtime.Exec(t.Context(), `SELECT inventory.reserve_stock($1,$2,$3,1,$4,2)`, key, other.actor, f.line, f.source)
	var problem *pgconn.PgError
	if !errors.As(err, &problem) || problem.Message != "idempotency_key_reused" || kernelSnapshot(f) != snapshot {
		t.Fatalf("command ID lost actor binding: %v", err)
	}
}

func TestKernelInventoryBalanceIdentityIsConcurrentAndNullSafe(t *testing.T) {
	f := newFixture(t)
	position := f.scalar(`SELECT position_id::text FROM inventory.stock_balances WHERE id=$1`, f.source)
	snapshot := kernelSnapshot(f)
	type outcome struct {
		id  string
		err error
	}
	results := make(chan outcome, 8)
	for range 8 {
		go func() {
			var balance string
			err := f.runtime.QueryRow(t.Context(), `SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available',NULL,NULL)::text`, f.org, f.wh, f.owner, f.item, position).Scan(&balance)
			results <- outcome{balance, err}
		}()
	}
	for range 8 {
		got := <-results
		if got.err != nil || got.id != f.source {
			t.Fatalf("null identity retry id=%s err=%v", got.id, got.err)
		}
	}
	if kernelSnapshot(f) != snapshot {
		t.Fatal("concurrent ensure_balance changed existing stock or created duplicate identities")
	}
	location, freshPosition := id(), id()
	f.exec(`INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES($1,$2,$3,$1::uuid::text,'storage')`, location, f.org, f.wh)
	f.exec(`INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES($1,$2,$3,$4)`, freshPosition, f.org, f.wh, location)
	for range 8 {
		go func() {
			var balance string
			err := f.runtime.QueryRow(t.Context(), `SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available',NULL,NULL)::text`, f.org, f.wh, f.owner, f.item, freshPosition).Scan(&balance)
			results <- outcome{balance, err}
		}()
	}
	created := ""
	for range 8 {
		got := <-results
		if got.err != nil || got.id == "" || (created != "" && got.id != created) {
			t.Fatalf("concurrent first creation id=%s previous=%s err=%v", got.id, created, got.err)
		}
		created = got.id
	}
	if got := f.scalar(`SELECT count(*)::text||':'||sum(on_hand)::text||':'||sum(reserved_qty)::text FROM inventory.stock_balances WHERE organization_id=$1 AND position_id=$2`, f.org, freshPosition); got != "1:0.000000:0.000000" {
		t.Fatalf("concurrent zero identity creation=%s", got)
	}
	assertKernelReconciles(t, f)
}

func TestKernelInventoryHistoryAndBalanceIdentityAreImmutable(t *testing.T) {
	for _, tc := range []struct{ name, sql, detail string }{
		{"balance identity", `UPDATE inventory.stock_balances SET owner_id=$2 WHERE id=$1`, "stock_identity_is_immutable"},
		{"movement lines", `DELETE FROM inventory.movement_lines WHERE movement_id IN (SELECT id FROM inventory.movements WHERE organization_id=$1)`, "append_only_history"},
		{"audit", `DELETE FROM platform.audit_entries WHERE operation_id IN (SELECT id FROM platform.operations WHERE organization_id=$1)`, "append_only_history"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			snapshot := kernelSnapshot(f)
			args := []any{f.org}
			if tc.name == "balance identity" {
				args = []any{f.source, f.otherOwner}
			}
			_, err := f.admin.Exec(t.Context(), tc.sql, args...)
			var problem *pgconn.PgError
			if !errors.As(err, &problem) || problem.Message != tc.detail {
				t.Fatalf("immutable core error=%v want=%s", err, tc.detail)
			}
			if kernelSnapshot(f) != snapshot {
				t.Fatal("immutable core changed after failed statement")
			}
		})
	}
}

func TestKernelInventoryItemAccountingCannotDriftAfterUse(t *testing.T) {
	for _, column := range []string{"quantity_scale", "tracking", "base_unit"} {
		t.Run(column, func(t *testing.T) {
			f := newFixture(t)
			f.exec(`INSERT INTO catalog.units(code,dimension) VALUES('audit_kg','mass') ON CONFLICT DO NOTHING`)
			value := map[string]string{"quantity_scale": "0", "tracking": "'lot'", "base_unit": "'audit_kg'"}[column]
			_, err := f.admin.Exec(t.Context(), `UPDATE catalog.items SET `+column+`=`+value+` WHERE id=$1`, f.item)
			var problem *pgconn.PgError
			if !errors.As(err, &problem) || problem.Message != "item_accounting_requires_explicit_migration" {
				t.Fatalf("accounting metadata drift error=%v", err)
			}
			if got := f.scalar(`SELECT quantity_scale::text||':'||tracking||':'||base_unit FROM catalog.items WHERE id=$1`, f.item); got != "3:none:ea" {
				t.Fatalf("item accounting changed: %s", got)
			}
		})
	}
}

func TestKernelInventoryPrepareCannotCommitIncompleteOperation(t *testing.T) {
	for _, kind := range []string{"reserve", "movement", "release"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			key := id()
			var query string
			var args []any
			switch kind {
			case "reserve":
				query = `SELECT inventory.prepare_reservation($1,$2,$3,1,$4,1)`
				args = []any{key, f.actor, f.line, f.source}
			case "movement":
				query = `SELECT inventory.prepare_movement($1,$2,$3,1,'transfer',$4,$5,1)`
				args = []any{key, f.actor, f.line, f.source, f.target}
			case "release":
				w := f.request(f.handler(nil, nil), "POST", "/api/v1/reservations", f.reserveBody(f.line, "1", 1), id())
				requireStatus(t, w, 201)
				reservation := data(t, w)["reservation_id"].(string)
				query = `SELECT inventory.prepare_release($1,$2,$3,2,1,'abandoned preparation')`
				args = []any{key, f.actor, reservation}
			}
			snapshot := kernelSnapshot(f)
			_, err := f.runtime.Exec(t.Context(), query, args...)
			var problem *pgconn.PgError
			if !errors.As(err, &problem) || problem.Message != "incomplete_operation_cannot_commit" {
				t.Fatalf("abandoned prepared operation error=%v", err)
			}
			if kernelSnapshot(f) != snapshot {
				t.Fatal("preparation committed without posting/audit/outbox")
			}
		})
	}
}
