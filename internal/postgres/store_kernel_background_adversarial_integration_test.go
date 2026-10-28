//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"errors"
	"github.com/Shpuntyara/wms-platform/internal/jobs"
	"github.com/jackc/pgx/v5/pgconn"
)

func seedKernelOutbox(t *testing.T, f *fixture, count int) []string {
	t.Helper()
	label := "test.core-background." + id()
	rows, err := f.admin.Query(t.Context(), `INSERT INTO platform.outbox(id,operation_id,event_type,aggregate_id,payload,available_at)
 SELECT e.id,o.id,$3||'.'||e.g,$2,jsonb_build_object('event_id',e.id::text),'2000-01-01'::timestamptz+e.g*interval '1 millisecond'
 FROM (SELECT gen_random_uuid() AS id,g FROM generate_series(1,$4) g) e CROSS JOIN
 (SELECT id FROM platform.operations WHERE organization_id=$1 LIMIT 1) o RETURNING id::text`, f.org, f.document, label, count)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, value)
	}
	if err = rows.Err(); err != nil || len(ids) != count {
		t.Fatalf("seeded=%d want=%d error=%v", len(ids), count, err)
	}
	return ids
}

// Keep unrelated tests' durable events intact. SKIP LOCKED must avoid these rows.
func isolateKernelOutbox(t *testing.T, f *fixture, allowed []string) {
	t.Helper()
	tx, err := f.admin.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(context.Background()); err != nil {
			t.Errorf("unlock outbox fixture: %v", err)
		}
	})
	if _, err = tx.Exec(t.Context(), `SELECT id FROM platform.outbox WHERE id<>ALL($1::text[]::uuid[]) FOR UPDATE`, allowed); err != nil {
		t.Fatal(err)
	}
}

func kernelClaim(t *testing.T, f *fixture, limit int) []jobs.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	events, err := f.store.ClaimEvents(ctx, limit)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestKernelBackgroundRejectsInvalidBatchSizes(t *testing.T) {
	f := newFixture(t)
	for _, function := range []struct {
		name    string
		maximum int
	}{{"platform.claim_outbox", 100}, {"inventory.expire_reservations", 1000}} {
		for _, value := range []any{nil, -1, 0, function.maximum + 1} {
			t.Run(fmt.Sprint(function.name, "/", value), func(t *testing.T) {
				var ignored int
				query := "SELECT count(*) FROM " + function.name + "($1::integer)"
				err := f.runtime.QueryRow(t.Context(), query, value).Scan(&ignored)
				var problem *pgconn.PgError
				if !errors.As(err, &problem) || problem.Message != "invalid_batch_size" {
					t.Fatalf("invalid batch accepted: function=%s size=%v error=%v", function.name, value, err)
				}
			})
		}
	}
}

func TestKernelOutboxConcurrentClaimsAndLeaseRecovery(t *testing.T) {
	f := newFixture(t)
	ids := seedKernelOutbox(t, f, 12)
	isolateKernelOutbox(t, f, ids)
	targets := map[string]bool{}
	for _, event := range ids {
		targets[event] = true
	}
	type result struct {
		events []jobs.Event
		err    error
	}
	results := make(chan result, 4)
	for range 4 {
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			events, err := f.store.ClaimEvents(ctx, 3)
			results <- result{events, err}
		}()
	}
	claimed := map[string]jobs.Event{}
	for range 4 {
		r := <-results
		if r.err != nil || len(r.events) != 3 {
			t.Fatalf("concurrent batch size=%d error=%v", len(r.events), r.err)
		}
		for _, event := range r.events {
			if !targets[event.ID] || event.Token == "" {
				t.Fatalf("invalid lease: %+v", event)
			}
			if _, duplicate := claimed[event.ID]; duplicate {
				t.Fatalf("event leased twice concurrently: %s", event.ID)
			}
			var payload struct {
				EventID string `json:"event_id"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.EventID != event.ID {
				t.Fatalf("lease identity changed: %s %s %v", event.ID, event.Payload, err)
			}
			claimed[event.ID] = event
		}
	}
	if len(claimed) != 12 {
		t.Fatalf("lost claims: %d", len(claimed))
	}
	if additional := kernelClaim(t, f, 100); len(additional) != 0 {
		t.Fatalf("active leases claimed again: %d", len(additional))
	}
	event := claimed[ids[0]]
	if err := f.store.CompleteEvent(t.Context(), jobs.Event{ID: event.ID, Token: id()}, true); err == nil {
		t.Fatal("foreign lease token acknowledged event")
	}
	f.exec(`UPDATE platform.outbox SET leased_until=clock_timestamp()-interval '1 second' WHERE id=$1`, event.ID)
	if err := f.store.CompleteEvent(t.Context(), event, true); err == nil {
		t.Fatal("expired lease acknowledged event")
	}
	again := kernelClaim(t, f, 1)
	if len(again) != 1 || again[0].ID != event.ID || again[0].Token == event.Token {
		t.Fatalf("lease not recovered with new generation: %+v", again)
	}
	if f.scalar(`SELECT attempts FROM platform.outbox WHERE id=$1`, event.ID) != "2" {
		t.Fatal("retry attempt was not advanced exactly once")
	}
	if err := f.store.CompleteEvent(t.Context(), event, true); err == nil {
		t.Fatal("previous generation acknowledged replacement lease")
	}
	if err := f.store.CompleteEvent(t.Context(), again[0], true); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CompleteEvent(t.Context(), again[0], true); err == nil {
		t.Fatal("acknowledgement was accepted twice")
	}
	if got := f.scalar(`SELECT (delivered_at IS NOT NULL AND dead_at IS NULL AND lease_token IS NULL AND leased_until IS NULL)::text FROM platform.outbox WHERE id=$1`, event.ID); got != "true" {
		t.Fatal("successful ack did not retire lease")
	}
	if additional := kernelClaim(t, f, 100); len(additional) != 0 {
		t.Fatal("delivered event re-entered queue")
	}
}

func TestKernelOutboxRetryAvailabilityAndDeadLetter(t *testing.T) {
	f := newFixture(t)
	ids := seedKernelOutbox(t, f, 1)
	isolateKernelOutbox(t, f, ids)
	for attempt := 1; attempt <= 12; attempt++ {
		batch := kernelClaim(t, f, 1)
		if len(batch) != 1 || batch[0].ID != ids[0] {
			t.Fatalf("attempt %d lost event: %+v", attempt, batch)
		}
		if got := f.scalar(`SELECT attempts FROM platform.outbox WHERE id=$1`, ids[0]); got != strconv.Itoa(attempt) {
			t.Fatalf("attempt counter=%s expected=%d", got, attempt)
		}
		if err := f.store.CompleteEvent(t.Context(), batch[0], false); err != nil {
			t.Fatal(err)
		}
		if got := f.scalar(`SELECT (delivered_at IS NULL AND lease_token IS NULL AND leased_until IS NULL AND (dead_at IS NOT NULL)=$2)::text FROM platform.outbox WHERE id=$1`, ids[0], attempt == 12); got != "true" {
			t.Fatalf("failed delivery did not preserve retry/dead state at attempt %d", attempt)
		}
		wantDelay := min(3600, 1<<attempt)
		if got := f.scalar(`SELECT (extract(epoch FROM available_at-clock_timestamp()) BETWEEN $2::integer-1 AND $2::integer+1)::text FROM platform.outbox WHERE id=$1`, ids[0], wantDelay); got != "true" {
			t.Fatalf("retry delay was not exponential/capped at attempt %d", attempt)
		}
		if err := f.store.CompleteEvent(t.Context(), batch[0], false); err == nil {
			t.Fatal("retired lease reused for another failure")
		}
		if early := kernelClaim(t, f, 100); len(early) != 0 {
			t.Fatalf("future/dead event claimed before available_at at attempt %d", attempt)
		}
		f.exec(`UPDATE platform.outbox SET available_at='2000-01-01' WHERE id=$1`, ids[0])
	}
	if events := kernelClaim(t, f, 100); len(events) != 0 {
		t.Fatal("dead-letter event was reclaimed")
	}
	if got := f.scalar(`SELECT (dead_at IS NOT NULL AND delivered_at IS NULL AND attempts=12)::text FROM platform.outbox WHERE id=$1`, ids[0]); got != "true" {
		t.Fatal("retry exhaustion lost durable dead-letter")
	}
}

func kernelExpiringReservation(t *testing.T, f *fixture) string {
	t.Helper()
	var raw []byte
	err := f.runtime.QueryRow(t.Context(), `SELECT inventory.reserve_stock($1,$2,$3,$4::bigint,$5,3,clock_timestamp()+interval '20 milliseconds')`, id(), f.actor, f.line, f.scalar(`SELECT version FROM warehouse.documents WHERE id=$1`, f.document), f.source).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ReservationID string `json:"reservation_id"`
	}
	if err = json.Unmarshal(raw, &result); err != nil || result.ReservationID == "" {
		t.Fatalf("reservation result=%s error=%v", raw, err)
	}
	if _, err = f.admin.Exec(t.Context(), `SELECT pg_sleep(0.035)`); err != nil {
		t.Fatal(err)
	}
	return result.ReservationID
}

func TestKernelExpirySkipsLockedDocumentAndRollsBackAtomically(t *testing.T) {
	f := newFixture(t)
	reservation := kernelExpiringReservation(t, f)
	tx, err := f.admin.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM warehouse.documents WHERE id=$1 FOR UPDATE`, f.document); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ExpireReservations(t.Context()); err != nil {
		t.Fatalf("locked document blocked expiry worker: %v", err)
	}
	if f.scalar(`SELECT active_qty::text FROM inventory.reservations WHERE id=$1`, reservation) != "3.000000" {
		t.Fatal("expiry bypassed document lock")
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	version := f.scalar(`SELECT version FROM warehouse.documents WHERE id=$1`, f.document)
	before := f.scalar(`SELECT count(*) FROM platform.operations WHERE organization_id=$1`, f.org)
	f.exec(fmt.Sprintf(`CREATE FUNCTION platform.core_expiry_failure_probe() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='wms.reservation.expired' AND NEW.aggregate_id='%s'::uuid THEN RAISE EXCEPTION 'forced_expiry_atomic_rollback' USING ERRCODE='23514'; END IF; RETURN NEW; END $$;CREATE TRIGGER core_expiry_failure_probe BEFORE INSERT ON platform.outbox FOR EACH ROW EXECUTE FUNCTION platform.core_expiry_failure_probe()`, f.document))
	t.Cleanup(func() {
		f.cleanupExec(`DROP TRIGGER IF EXISTS core_expiry_failure_probe ON platform.outbox;DROP FUNCTION IF EXISTS platform.core_expiry_failure_probe()`)
	})
	if _, err = f.store.ExpireReservations(t.Context()); err == nil {
		t.Fatal("injected outbox failure did not abort expiry")
	}
	if got := f.scalar(`SELECT (r.active_qty=3 AND b.reserved_qty=3 AND l.reserved_qty=3 AND d.version=$2::bigint)::text FROM inventory.reservations r JOIN inventory.stock_balances b ON b.id=r.balance_id JOIN warehouse.document_lines l ON l.id=r.document_line_id JOIN warehouse.documents d ON d.id=l.document_id WHERE r.id=$1`, reservation, version); got != "true" {
		t.Fatal("failed expiry partially changed inventory/document")
	}
	if got := f.scalar(`SELECT count(*) FROM platform.operations WHERE organization_id=$1`, f.org); got != before {
		t.Fatal("failed expiry left committed operation")
	}
	f.exec(`DROP TRIGGER core_expiry_failure_probe ON platform.outbox;DROP FUNCTION platform.core_expiry_failure_probe()`)
	for range 2 {
		if _, err = f.store.ExpireReservations(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.scalar(`SELECT (r.active_qty=0 AND r.released_qty=3 AND b.reserved_qty=0 AND l.reserved_qty=0 AND d.version=$2::bigint+1)::text FROM inventory.reservations r JOIN inventory.stock_balances b ON b.id=r.balance_id JOIN warehouse.document_lines l ON l.id=r.document_line_id JOIN warehouse.documents d ON d.id=l.document_id WHERE r.id=$1`, reservation, version); got != "true" {
		t.Fatal("expiry recovery did not release exactly once")
	}
	if got := f.scalar(`SELECT count(*) FROM inventory.reservation_events WHERE reservation_id=$1 AND kind='released' AND reason='expired'`, reservation); got != "1" {
		t.Fatalf("expiry history events=%s", got)
	}
	if got := f.scalar(`SELECT count(*) FROM platform.operations o JOIN platform.audit_entries a ON a.operation_id=o.id JOIN platform.outbox e ON e.operation_id=o.id WHERE o.organization_id=$1 AND o.operation_type='reservation_expiry' AND o.result->>'reservation_id'=$2 AND e.event_type='wms.reservation.expired'`, f.org, reservation); got != "1" {
		t.Fatalf("expiry atomic evidence rows=%s", got)
	}
}
