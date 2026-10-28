//go:build integration

package postgres_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The oracle stores integer millionths; it never repeats SQL numeric arithmetic.
func onpremMicros(q int64) string { return fmt.Sprintf("%d.%06d", q/1_000_000, q%1_000_000) }

type onpremAccountingScope struct{ wh, balance, item string }
type onpremAccountingCommand struct {
	sql    string
	args   []any
	result string
}

func (c *onpremAccountingCommand) execute(t *testing.T, f *fixture) {
	t.Helper()
	if err := f.runtime.QueryRow(t.Context(), c.sql, c.args...).Scan(&c.result); err != nil {
		t.Fatal(err)
	}
}

func onpremAccountingWarehouse(t *testing.T, f *fixture, item string) onpremAccountingScope {
	t.Helper()
	s := onpremAccountingScope{wh: id(), item: item}
	location, position := id(), id()
	f.exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, s.wh, f.org)
	f.exec(`INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES($1,$2,$3,'micro-bin','storage')`, location, f.org, s.wh)
	f.exec(`INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES($1,$2,$3,$4)`, position, f.org, s.wh, location)
	s.balance = f.scalar(`SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available')::text`, f.org, s.wh, f.owner, item, position)
	return s
}

func onpremAccountingDoc(t *testing.T, f *fixture, s onpremAccountingScope, kind, qty string) string {
	t.Helper()
	document, line := id(), id()
	f.exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES($1,$2,$3,$4,$5,$1::uuid::text,'active')`, document, f.org, s.wh, f.owner, kind)
	f.exec(`INSERT INTO warehouse.document_lines(id,document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty) VALUES($1,$2,$3,$4,$5,$6,1,$7::numeric)`, line, document, f.org, s.wh, f.owner, s.item, qty)
	return line
}

func onpremAccountingItem(f *fixture) string {
	item := id()
	f.exec(`INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES($1,$2,$1::uuid::text,'ea',6,'none')`, item, f.org)
	return item
}

func onpremAccountingMovement(f *fixture, line, kind string, source, target any, qty string) onpremAccountingCommand {
	return onpremAccountingCommand{sql: `SELECT inventory.post_movement($1,$2,$3,1,$4,$5,$6,$7::numeric)::text`, args: []any{id(), f.actor, line, kind, source, target, qty}}
}

func onpremAccountingFailure(t *testing.T, f *fixture, sql, code, message string, args ...any) {
	t.Helper()
	before := onpremAccountingSnapshot(f)
	_, err := f.runtime.Exec(t.Context(), sql, args...)
	var problem *pgconn.PgError
	if !errors.As(err, &problem) || problem.Code != code || (message != "" && problem.Message != message) {
		t.Fatalf("error=%v want SQLSTATE=%s message=%q", err, code, message)
	}
	if onpremAccountingSnapshot(f) != before {
		t.Fatal("failed command changed stock, document, operation, audit or outbox state")
	}
	assertKernelReconciles(t, f)
}

func onpremAccountingSnapshot(f *fixture) string {
	return kernelSnapshot(f) + f.scalar(`SELECT jsonb_build_object(
	 'reservations',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM inventory.reservations r WHERE organization_id=$1),
	 'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM inventory.reservation_events e JOIN inventory.reservations r ON r.id=e.reservation_id WHERE r.organization_id=$1),
	 'ledger',(SELECT jsonb_agg(to_jsonb(l) ORDER BY l.id) FROM inventory.movement_lines l JOIN inventory.movements m ON m.id=l.movement_id WHERE m.organization_id=$1),
	 'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM platform.operations o WHERE organization_id=$1))::text`, f.org)
}

func TestOnPremAccountingMicroquantityConcurrentWarehousesProperty(t *testing.T) {
	const seed = 202610280717
	rng := rand.New(rand.NewSource(seed))
	for sample := range 12 {
		t.Run(fmt.Sprintf("seed_%d_case_%d", seed, sample), func(t *testing.T) {
			f := newFixture(t)
			item := onpremAccountingItem(f)
			quantities := [3]int64{1 + rng.Int63n(999_999), 1 + rng.Int63n(1_999_999), 1 + rng.Int63n(2_999_999)}
			if sample == 0 {
				quantities = [3]int64{1, 999_999, 1_000_001}
			}
			total := quantities[0] + quantities[1] + quantities[2]
			type lane struct {
				scope        onpremAccountingScope
				lines        [3]string
				reservations [3]string
			}
			lanes := make([]lane, 2)
			var commands []onpremAccountingCommand
			for w := range lanes {
				lanes[w].scope = onpremAccountingWarehouse(t, f, item)
				s := lanes[w].scope
				line := onpremAccountingDoc(t, f, s, "receipt", onpremMicros(total))
				receipt := onpremAccountingMovement(f, line, "receipt", nil, s.balance, onpremMicros(total))
				receipt.execute(t, f)
				commands = append(commands, receipt)
				for j, qty := range quantities {
					lanes[w].lines[j] = onpremAccountingDoc(t, f, s, "shipment", onpremMicros(qty))
					commands = append(commands, onpremAccountingCommand{sql: `SELECT inventory.reserve_stock($1,$2,$3,1,$4,$5::numeric,$6)::text`, args: []any{id(), f.actor, lanes[w].lines[j], s.balance, onpremMicros(qty), time.Now().UTC().Add(time.Hour)}})
				}
			}
			// Distinct documents compete on each shared balance, on both warehouses.
			type outcome struct {
				index  int
				result string
				err    error
			}
			results := make(chan outcome, 6)
			for i := range commands {
				if i%4 == 0 {
					continue
				} // receipts already committed
				go func() {
					var result string
					err := f.runtime.QueryRow(t.Context(), commands[i].sql, commands[i].args...).Scan(&result)
					results <- outcome{i, result, err}
				}()
			}
			for range 6 {
				got := <-results
				if got.err != nil {
					t.Fatal(got.err)
				}
				commands[got.index].result = got.result
				var result struct {
					Reservation string `json:"reservation_id"`
				}
				if err := json.Unmarshal([]byte(got.result), &result); err != nil {
					t.Fatal(err)
				}
				lanes[got.index/4].reservations[got.index%4-1] = result.Reservation
			}
			for _, lane := range lanes {
				if got := f.scalar(`SELECT on_hand::text||':'||reserved_qty::text FROM inventory.stock_balances WHERE id=$1`, lane.scope.balance); got != onpremMicros(total)+":"+onpremMicros(total) {
					t.Fatalf("seed=%d quantities=%v reservation state=%s", seed, quantities, got)
				}
				f.exec(`UPDATE inventory.reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, lane.reservations[2])
				commands = append(commands,
					onpremAccountingCommand{sql: `SELECT inventory.release_reservation($1,$2,$3,2,$4::numeric,'micro oracle cancellation')::text`, args: []any{id(), f.actor, lane.reservations[0], onpremMicros(quantities[0])}},
					onpremAccountingCommand{sql: `SELECT inventory.post_movement($1,$2,$3,2,'issue',$4,NULL,$5::numeric,$6)::text`, args: []any{id(), f.actor, lane.lines[1], lane.scope.balance, onpremMicros(quantities[1]), lane.reservations[1]}})
			}
			mixed := make(chan outcome, 6)
			for i := 8; i < len(commands); i++ {
				go func() {
					var result string
					err := f.runtime.QueryRow(t.Context(), commands[i].sql, commands[i].args...).Scan(&result)
					mixed <- outcome{i, result, err}
				}()
			}
			for range 2 {
				go func() {
					var expired int
					err := f.runtime.QueryRow(t.Context(), `SELECT inventory.expire_reservations(1000)`).Scan(&expired)
					mixed <- outcome{-1, "", err}
				}()
			}
			for range 6 {
				got := <-mixed
				if got.err != nil {
					t.Fatal(got.err)
				}
				if got.index >= 0 {
					commands[got.index].result = got.result
				}
			}
			for _, lane := range lanes {
				if got := f.scalar(`SELECT on_hand::text||':'||reserved_qty::text FROM inventory.stock_balances WHERE id=$1`, lane.scope.balance); got != onpremMicros(total-quantities[1])+":0.000000" {
					t.Fatalf("integer oracle quantities=%v balance=%s", quantities, got)
				}
				for j, line := range lane.lines {
					fulfilled := int64(0)
					if j == 1 {
						fulfilled = quantities[1]
					}
					if got := f.scalar(`SELECT fulfilled_qty::text||':'||reserved_qty::text FROM warehouse.document_lines WHERE id=$1`, line); got != onpremMicros(fulfilled)+":0.000000" {
						t.Fatalf("line=%d state=%s", j, got)
					}
				}
				for kind, expected := range map[string]int64{"reserved": total, "released": quantities[0] + quantities[2], "consumed": quantities[1]} {
					got := f.scalar(`SELECT coalesce(sum(e.quantity),0)::text FROM inventory.reservation_events e JOIN inventory.reservations r ON r.id=e.reservation_id WHERE r.balance_id=$1 AND e.kind=$2`, lane.scope.balance, kind)
					if got != onpremMicros(expected) {
						t.Fatalf("event=%s sum=%s want=%s", kind, got, onpremMicros(expected))
					}
				}
			}
			assertKernelReconciles(t, f)
			snapshot := onpremAccountingSnapshot(f)
			for _, command := range commands {
				var replay string
				if err := f.runtime.QueryRow(t.Context(), command.sql, command.args...).Scan(&replay); err != nil || replay != command.result {
					t.Fatalf("replay result changed: err=%v got=%s want=%s", err, replay, command.result)
				}
			}
			if onpremAccountingSnapshot(f) != snapshot {
				t.Fatal("same-ID replay changed accounting or effects after expiry")
			}
		})
	}
}

func TestOnPremAccountingNumericBoundaryAndAtomicOverflow(t *testing.T) {
	f := newFixture(t)
	item := onpremAccountingItem(f)
	const maximum = "999999999999999999.999999"
	var scopes []onpremAccountingScope
	for range 2 {
		s := onpremAccountingWarehouse(t, f, item)
		scopes = append(scopes, s)
		line := onpremAccountingDoc(t, f, s, "receipt", maximum)
		command := onpremAccountingMovement(f, line, "receipt", nil, s.balance, maximum)
		command.execute(t, f)
	}
	// Aggregate may exceed the per-row numeric(24,6) range and int64 micros.
	maxMicros, ok := new(big.Int).SetString(strings.ReplaceAll(maximum, ".", ""), 10)
	if !ok {
		t.Fatal("invalid independent maximum")
	}
	maxMicros.Mul(maxMicros, big.NewInt(2))
	divisor := big.NewInt(1_000_000)
	whole, fraction := new(big.Int), new(big.Int)
	whole.QuoRem(maxMicros, divisor, fraction)
	want := whole.String() + fmt.Sprintf(".%06d", fraction.Int64())
	if got := f.scalar(`SELECT sum(on_hand)::text FROM inventory.stock_balances WHERE organization_id=$1 AND item_id=$2`, f.org, item); got != want {
		t.Fatalf("aggregate=%s want=%s", got, want)
	}
	line := onpremAccountingDoc(t, f, scopes[0], "receipt", "0.000001")
	onpremAccountingFailure(t, f, `SELECT inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,0.000001)`, "22003", "", id(), f.actor, line, scopes[0].balance)
	shipment := onpremAccountingDoc(t, f, scopes[0], "shipment", maximum)
	for _, qty := range []string{"0.0000001", "0.0000005", "0.0000009", "1.0000001", "NaN", "Infinity", "-Infinity"} {
		onpremAccountingFailure(t, f, `SELECT inventory.reserve_stock($1,$2,$3,1,$4,$5::numeric)`, "22023", "invalid_quantity", id(), f.actor, shipment, scopes[0].balance, qty)
		onpremAccountingFailure(t, f, `SELECT inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,$5::numeric)`, "22023", "invalid_quantity", id(), f.actor, line, scopes[0].balance, qty)
	}
}

func TestOnPremAccountingCrossWarehouseCorrectionAndReplayStayAtomic(t *testing.T) {
	f := newFixture(t)
	item := onpremAccountingItem(f)
	a, b := onpremAccountingWarehouse(t, f, item), onpremAccountingWarehouse(t, f, item)
	lineA := onpremAccountingDoc(t, f, a, "receipt", "0.000003")
	command := onpremAccountingMovement(f, lineA, "receipt", nil, a.balance, "0.000003")
	command.execute(t, f)
	var original struct {
		Movement string `json:"movement_id"`
	}
	if err := json.Unmarshal([]byte(command.result), &original); err != nil {
		t.Fatal(err)
	}
	lineB := onpremAccountingDoc(t, f, b, "adjustment", "0.000001")
	onpremAccountingFailure(t, f, `SELECT inventory.post_movement($1,$2,$3,1,'adjustment_in',NULL,$4,0.000001,NULL,'recount',$5)`, "P0001", "correction_scope_mismatch", id(), f.actor, lineB, b.balance, original.Movement)
	lineBReceipt := onpremAccountingDoc(t, f, b, "receipt", "0.000003")
	onpremAccountingFailure(t, f, `SELECT inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,0.000003)`, "22023", "idempotency_key_reused", command.args[0], f.actor, lineBReceipt, b.balance)
	onpremAccountingFailure(t, f, `SELECT inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,0.000003)`, "P0001", "stock_scope_mismatch", id(), f.actor, lineBReceipt, a.balance)
}

func TestOnPremAccountingOwnerImportsRejectNonFiniteQuantities(t *testing.T) {
	f := newFixture(t)
	for _, target := range []struct{ table, column, row string }{
		{"inventory.stock_balances", "on_hand", f.source},
		{"inventory.stock_balances", "reserved_qty", f.source},
		{"warehouse.document_lines", "requested_qty", f.line},
		{"warehouse.document_lines", "reserved_qty", f.line},
		{"warehouse.document_lines", "fulfilled_qty", f.line},
	} {
		for _, special := range []string{"NaN", "Infinity", "-Infinity"} {
			t.Run(target.table+"_"+target.column+"_"+special, func(t *testing.T) {
				before := onpremAccountingSnapshot(f)
				// Identifiers come only from this fixed local test table, values are bound.
				_, err := f.admin.Exec(t.Context(), `UPDATE `+target.table+` SET `+target.column+`=$1::numeric WHERE id=$2`, special, target.row)
				var problem *pgconn.PgError
				if !errors.As(err, &problem) || (problem.Code != "23514" && problem.Code != "22003") {
					t.Fatalf("owner import persisted %s or failed unexpectedly: %v", special, err)
				}
				if onpremAccountingSnapshot(f) != before {
					t.Fatal("failed owner import changed persistent accounting")
				}
			})
		}
	}
}
