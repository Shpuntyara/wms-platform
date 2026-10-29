//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"

	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func numberingCriticBody(f *fixture, kind string) map[string]any {
	return map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": kind, "lines": []any{map[string]any{"item_id": f.item, "quantity": "1"}}}
}

func numberingCriticCreate(t *testing.T, f *fixture, h http.Handler, body map[string]any, command string) (string, string) {
	t.Helper()
	w := f.request(h, "POST", "/api/v1/documents", body, command)
	requireStatus(t, w, 201)
	doc, ok := data(t, w)["document_id"].(string)
	if !ok || doc == "" {
		t.Fatalf("created document has no identity: %s", w.Body)
	}
	return doc, f.scalar(`SELECT number FROM warehouse.documents WHERE id=$1`, doc)
}

func numberingCriticYear(f *fixture) string {
	return f.scalar(`SELECT extract(year FROM transaction_timestamp() AT TIME ZONE timezone)::int::text FROM warehouse.warehouses WHERE id=$1`, f.wh)
}

func TestNumberingCriticAutoKindsAndDurableReplay(t *testing.T) {
	for _, kind := range []string{"receipt", "transfer"} {
		for _, field := range []string{"omitted", "empty", "null"} {
			t.Run(kind+"/"+field, func(t *testing.T) {
				f := newFixture(t)
				grantDocumentPermissions(f)
				h := f.handler(nil, nil)
				body := numberingCriticBody(f, kind)
				if field == "empty" {
					body["number"] = ""
				}
				if field == "null" {
					body["number"] = nil
				}
				key := id()
				created := f.request(h, "POST", "/api/v1/documents", body, key)
				requireStatus(t, created, 201)
				doc := data(t, created)["document_id"].(string)
				prefix := map[string]string{"receipt": "PZ", "transfer": "MM"}[kind]
				want := prefix + "/" + numberingCriticYear(f) + "/000001"
				if got := f.scalar(`SELECT number FROM warehouse.documents WHERE id=$1`, doc); got != want {
					t.Fatalf("number=%q want=%q", got, want)
				}
				for range 3 {
					replay := f.request(h, "POST", "/api/v1/documents", body, key)
					requireStatus(t, replay, 201)
					if replay.Body.String() != created.Body.String() {
						t.Fatal("same command changed its durable result")
					}
				}
				_, next := numberingCriticCreate(t, f, h, numberingCriticBody(f, kind), id())
				if next != prefix+"/"+numberingCriticYear(f)+"/000002" {
					t.Fatalf("replay consumed a number: next=%q", next)
				}
			})
		}
	}
}

func TestNumberingCriticConcurrentIndependentAndDuplicateCommands(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-command=%t", duplicate), func(t *testing.T) {
			f := newFixture(t)
			grantDocumentPermissions(f)
			h := f.handler(nil, nil)
			body, sharedKey := numberingCriticBody(f, "receipt"), id()
			type result struct {
				status int
				body   string
			}
			const count = 24
			results := make(chan result, count)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for range count {
				workers.Go(func() {
					key := sharedKey
					if !duplicate {
						key = id()
					}
					<-start
					w := f.request(h, "POST", "/api/v1/documents", body, key)
					results <- result{w.Code, w.Body.String()}
				})
			}
			close(start)
			workers.Wait()
			close(results)
			var first string
			for got := range results {
				if got.status != 201 {
					t.Fatalf("concurrent create status=%d want=201 body=%s", got.status, got.body)
				}
				if duplicate && first != "" && got.body != first {
					t.Fatal("concurrent same-ID replay returned different receipts")
				}
				first = got.body
			}
			rows, err := f.admin.Query(t.Context(), `SELECT number FROM warehouse.documents WHERE organization_id=$1 AND kind='receipt' AND state='draft' ORDER BY number`, f.org)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var numbers []string
			for rows.Next() {
				var number string
				if err := rows.Scan(&number); err != nil {
					t.Fatal(err)
				}
				numbers = append(numbers, number)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			wantCount := count
			if duplicate {
				wantCount = 1
			}
			if len(numbers) != wantCount {
				t.Fatalf("durable documents=%d want=%d", len(numbers), wantCount)
			}
			for i, number := range numbers {
				if want := fmt.Sprintf("PZ/%s/%06d", numberingCriticYear(f), i+1); number != want {
					t.Fatalf("sequence[%d]=%q want=%q", i, number, want)
				}
			}
		})
	}
}

func TestNumberingCriticRollbackAndPermissionsDoNotConsumeNumbers(t *testing.T) {
	for _, mode := range []string{"permission", "line-scale", "deferred-commit"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			h := f.handler(nil, nil)
			body := numberingCriticBody(f, "receipt")
			wantStatus := 403
			var trigger string
			if mode != "permission" {
				grantDocumentPermissions(f)
			}
			if mode == "line-scale" {
				body["lines"] = []any{map[string]any{"item_id": f.item, "quantity": "0.0001"}}
				wantStatus = 400
			}
			if mode == "deferred-commit" {
				wantStatus = 409
				trigger = "numbering_critic_" + strings.ReplaceAll(id(), "-", "")
				f.exec(`CREATE FUNCTION warehouse.` + trigger + `() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$ BEGIN IF NEW.organization_id='` + f.org + `'::uuid THEN RAISE EXCEPTION 'numbering rollback probe' USING ERRCODE='23514'; END IF; RETURN NULL; END $$`)
				f.exec(`CREATE CONSTRAINT TRIGGER ` + trigger + ` AFTER INSERT ON warehouse.documents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION warehouse.` + trigger + `() `)
				t.Cleanup(func() {
					f.cleanupExec(`DROP TRIGGER IF EXISTS ` + trigger + ` ON warehouse.documents`)
					f.cleanupExec(`DROP FUNCTION IF EXISTS warehouse.` + trigger + `()`)
				})
			}
			key := id()
			requireStatus(t, f.request(h, "POST", "/api/v1/documents", body, key), wantStatus)
			if got := f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, key); got != "0" {
				t.Fatalf("rejected create retained operation=%s", got)
			}
			if mode == "permission" {
				grantDocumentPermissions(f)
				h = f.handler(nil, nil)
			}
			if trigger != "" {
				f.exec(`DROP TRIGGER ` + trigger + ` ON warehouse.documents`)
			}
			_, number := numberingCriticCreate(t, f, h, numberingCriticBody(f, "receipt"), id())
			if want := "PZ/" + numberingCriticYear(f) + "/000001"; number != want {
				t.Fatalf("%s consumed a number: got=%q want=%q", mode, number, want)
			}
		})
	}
}

func TestNumberingCriticUnknownCommitKeepsOneNumber(t *testing.T) {
	for _, mode := range []string{"before_send", "after_ack_eof", "after_ack_generic"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			grantDocumentPermissions(f)
			cfg, err := pgxpool.ParseConfig(os.Getenv("WMS_TEST_RUNTIME_DSN"))
			if err != nil {
				t.Fatal(err)
			}
			var dropped atomic.Bool
			cfg.MaxConns = 1
			cfg.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return &documentCommitFault{Conn: conn, mode: mode, dropped: &dropped}, nil
			}
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			f.store, err = postgres.New(t.Context(), pool)
			if err != nil {
				t.Fatal(err)
			}
			h, key, body := f.handler(nil, nil), id(), numberingCriticBody(f, "receipt")
			w := f.request(h, "POST", "/api/v1/documents", body, key)
			requireStatus(t, w, 503)
			if !dropped.Load() || !strings.Contains(w.Body.String(), "same command identifier") {
				t.Fatal("transport fault did not produce same-command recovery")
			}
			committed := "1"
			if mode == "before_send" {
				committed = "0"
			}
			if got := f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1 AND completed_at IS NOT NULL`, key); got != committed {
				t.Fatalf("committed before replay=%s want=%s", got, committed)
			}
			doc, number := numberingCriticCreate(t, f, h, body, key)
			if number != "PZ/"+numberingCriticYear(f)+"/000001" {
				t.Fatalf("recovery number=%q", number)
			}
			for range 2 {
				again, same := numberingCriticCreate(t, f, h, body, key)
				if again != doc || same != number {
					t.Fatal("replay changed document identity or number")
				}
			}
			_, next := numberingCriticCreate(t, f, h, body, id())
			if next != "PZ/"+numberingCriticYear(f)+"/000002" {
				t.Fatalf("unknown COMMIT/replay consumed extra numbers: %q", next)
			}
			for _, table := range []string{"audit_entries", "outbox"} {
				if got := f.scalar(`SELECT count(*) FROM platform.`+table+` WHERE operation_id=$1`, key); got != "1" {
					t.Fatalf("%s duplicate receipt=%s", table, got)
				}
			}
		})
	}
}

func TestNumberingCriticManualLegacyAndWarehouseScope(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	h := f.handler(nil, nil)
	year := numberingCriticYear(f)
	manual := numberingCriticBody(f, "receipt")
	manual["number"] = "PZ/" + year + "/000009"
	doc, number := numberingCriticCreate(t, f, h, manual, id())
	if number != manual["number"] {
		t.Fatalf("manual legacy number changed: %q", number)
	}
	requireStatus(t, f.request(h, "POST", "/api/v1/documents", manual, id()), 409)
	_, auto := numberingCriticCreate(t, f, h, numberingCriticBody(f, "receipt"), id())
	if auto != "PZ/"+year+"/000010" {
		t.Fatalf("manual reservation collided with auto series: %q", auto)
	}
	// Importing a smaller canonical value cannot rewind an existing series.
	manual["number"] = "PZ/" + year + "/000002"
	numberingCriticCreate(t, f, h, manual, id())
	_, auto = numberingCriticCreate(t, f, h, numberingCriticBody(f, "receipt"), id())
	if auto != "PZ/"+year+"/000011" {
		t.Fatalf("manual number rewound an existing series: %q", auto)
	}
	// Arbitrary legacy numbers retain their bytes, and a different document
	// prefix is not interpreted as this receipt's canonical PZ series.
	for _, legacy := range []string{"  przyjęcie dostawcy / 17  ", "MM/" + year + "/000099"} {
		manual["number"] = legacy
		_, got := numberingCriticCreate(t, f, h, manual, id())
		if got != legacy {
			t.Fatalf("manual legacy value changed: got=%q want=%q", got, legacy)
		}
	}
	_, auto = numberingCriticCreate(t, f, h, numberingCriticBody(f, "receipt"), id())
	if auto != "PZ/"+year+"/000012" {
		t.Fatalf("noncanonical legacy number consumed receipt series: %q", auto)
	}
	manual["number"] = number
	badTransition := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 1, "state": "active", "number": "PZ/" + year + "/000888"}
	requireStatus(t, f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", badTransition, id()), 400)
	for version, state := range []string{"active", "cancelled"} {
		transition := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": version + 1, "state": state}
		requireStatus(t, f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, id()), 201)
		if got := f.scalar(`SELECT number FROM warehouse.documents WHERE id=$1`, doc); got != number {
			t.Fatalf("transition renumbered successful document: %q -> %q", number, got)
		}
	}
	_, mm := numberingCriticCreate(t, f, h, numberingCriticBody(f, "transfer"), id())
	if mm != "MM/"+year+"/000001" {
		t.Fatalf("receipt consumed transfer series: %q", mm)
	}
	manual["kind"], manual["number"] = "transfer", "MM/"+year+"/000009"
	numberingCriticCreate(t, f, h, manual, id())
	_, mm = numberingCriticCreate(t, f, h, numberingCriticBody(f, "transfer"), id())
	if mm != "MM/"+year+"/000010" {
		t.Fatalf("manual transfer did not reserve MM series: %q", mm)
	}
	manual["kind"], manual["number"] = "receipt", number
	other := id()
	f.exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, other, f.org)
	f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.create')`, f.actor, f.org, other, f.owner)
	h = f.handler(nil, nil)
	body := numberingCriticBody(f, "receipt")
	body["warehouse_id"] = other
	_, otherNumber := numberingCriticCreate(t, f, h, body, id())
	if otherNumber != "PZ/"+year+"/000001" {
		t.Fatalf("warehouses share a counter: %q", otherNumber)
	}
	// The same canonical number is valid in another warehouse of the same organization.
	manual["warehouse_id"] = other
	numberingCriticCreate(t, f, h, manual, id())
}

func TestNumberingCriticConcurrentManualCollision(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	h, year := f.handler(nil, nil), numberingCriticYear(f)
	manual, auto := numberingCriticBody(f, "receipt"), numberingCriticBody(f, "receipt")
	manual["number"] = "PZ/" + year + "/000001"
	start := make(chan struct{})
	type outcome struct {
		automatic bool
		status    int
	}
	statuses := make(chan outcome, 2)
	go func() {
		<-start
		statuses <- outcome{false, f.request(h, "POST", "/api/v1/documents", manual, id()).Code}
	}()
	go func() { <-start; statuses <- outcome{true, f.request(h, "POST", "/api/v1/documents", auto, id()).Code} }()
	close(start)
	for range 2 {
		got := <-statuses
		if (got.automatic && got.status != 201) || (!got.automatic && got.status != 201 && got.status != 409) {
			t.Fatalf("manual/auto race automatic=%t status=%d", got.automatic, got.status)
		}
	}
	_, next := numberingCriticCreate(t, f, h, auto, id())
	var numbers []string
	rows, err := f.admin.Query(t.Context(), `SELECT number FROM warehouse.documents WHERE organization_id=$1 AND state='draft' ORDER BY number`, f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		numbers = append(numbers, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(numbers)
	for i, n := range numbers {
		if want := fmt.Sprintf("PZ/%s/%06d", year, i+1); n != want {
			t.Fatalf("manual/auto race gap or collision: %v, next=%s", numbers, next)
		}
	}
}

func TestNumberingCriticAdministrativeLegacyCollision(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	h, year := f.handler(nil, nil), numberingCriticYear(f)
	// Simulate a canonical document imported directly after the migration.
	// The next automatic command must skip that durable number atomically.
	f.exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES($1,$2,$3,$4,'receipt',$5,'draft')`, id(), f.org, f.wh, f.owner, "PZ/"+year+"/000001")
	for _, want := range []string{"PZ/" + year + "/000002", "PZ/" + year + "/000003"} {
		_, got := numberingCriticCreate(t, f, h, numberingCriticBody(f, "receipt"), id())
		if got != want {
			t.Fatalf("automatic number collided with imported document: got=%q want=%q", got, want)
		}
	}
}

func TestNumberingCriticWhitespaceCannotBecomeManualOrAuto(t *testing.T) {
	for _, number := range []string{" ", "\t", "\n", "\u00a0", " \t\n"} {
		t.Run(fmt.Sprintf("%q", number), func(t *testing.T) {
			f := newFixture(t)
			grantDocumentPermissions(f)
			body := numberingCriticBody(f, "receipt")
			body["number"] = number
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.runtime.Exec(t.Context(), `SELECT warehouse.command_document($1::uuid,$2::uuid,NULL,$3::jsonb)`, id(), f.actor, raw)
			if err == nil {
				t.Fatalf("SQL accepted whitespace-only document number %q", number)
			}
			if got := f.scalar(`SELECT count(*) FROM warehouse.documents WHERE organization_id=$1 AND state='draft'`, f.org); got != "0" {
				t.Fatalf("invalid whitespace left document=%s", got)
			}
		})
	}
}

func TestNumberingCriticWarehouseYearBoundaryAndMinimumWidth(t *testing.T) {
	f := newFixture(t)
	// A fixed instant deliberately falls in different calendar years in UTC and
	// the warehouse. This must not depend on the test machine's current month.
	for _, tc := range []struct{ name, zone, instant, want string }{
		{"ahead-of-UTC", "Pacific/Kiritimati", "2026-12-31T10:30:00Z", "PZ/2027/000001"},
		{"independent-previous-year", "UTC", "2026-12-31T10:30:00Z", "PZ/2026/000001"},
		{"resume-existing-year", "Pacific/Kiritimati", "2026-12-31T10:30:00Z", "PZ/2027/000002"},
		{"behind-UTC", "Etc/GMT+12", "2027-01-01T00:30:00Z", "PZ/2026/000002"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.exec(`UPDATE warehouse.warehouses SET timezone=$2 WHERE id=$1`, f.wh, tc.zone)
			got := f.scalar(`SELECT warehouse.allocate_document_number($1::uuid,$2::uuid,'receipt',$3::timestamptz)`, f.org, f.wh, tc.instant)
			if got != tc.want {
				t.Fatalf("warehouse year number=%q want=%q", got, tc.want)
			}
		})
	}
	grantDocumentPermissions(f)
	f.exec(`UPDATE warehouse.warehouses SET timezone='UTC' WHERE id=$1`, f.wh)
	h := f.handler(nil, nil)
	_, first := numberingCriticCreate(t, f, h, numberingCriticBody(f, "transfer"), id())
	year := numberingCriticYear(f)
	if first != "MM/"+year+"/000001" {
		t.Fatalf("first transfer=%q", first)
	}
	f.exec(`UPDATE warehouse.document_number_counters SET last_value=999999 WHERE organization_id=$1 AND warehouse_id=$2 AND kind='transfer' AND number_year=$3::int`, f.org, f.wh, year)
	for _, want := range []string{"MM/" + year + "/1000000", "MM/" + year + "/1000001"} {
		_, got := numberingCriticCreate(t, f, h, numberingCriticBody(f, "transfer"), id())
		if got != want {
			t.Fatalf("large sequence truncated or wrapped: got=%q want=%q", got, want)
		}
	}
}

func TestNumberingCriticCounterCannotBeChangedByRuntimeCredentials(t *testing.T) {
	f := newFixture(t)
	for _, signature := range []string{"warehouse.allocate_document_number(uuid,uuid,text,timestamp with time zone)", "warehouse.reserve_document_number(uuid,uuid,text,text)"} {
		var allowed bool
		if err := f.runtime.QueryRow(t.Context(), `SELECT has_function_privilege(current_user,$1,'EXECUTE')`, signature).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if allowed {
			t.Fatalf("runtime credentials can allocate or reserve numbers directly: %s", signature)
		}
	}
	for _, privilege := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
		var allowed bool
		if err := f.runtime.QueryRow(t.Context(), `SELECT has_table_privilege(current_user,'warehouse.document_number_counters',$1)`, privilege).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if allowed {
			t.Fatalf("runtime credentials can %s numbering counters", privilege)
		}
	}
}

func TestNumberingCriticGeneratedCommandHistories(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	h := f.handler(nil, nil)
	year := numberingCriticYear(f)
	// This independent model deliberately has no SQL counter reads. Invalid
	// commands and replays must not change its next expected number.
	last := map[string]int{"receipt": 0, "transfer": 0}
	property := func(seed uint64) bool {
		bits := seed
		for step := 0; step < 3+int(seed%7); step++ {
			kind := []string{"receipt", "transfer"}[bits&1]
			operation := (bits >> 1) % 4
			bits = bits>>4 | bits<<60
			body, key := numberingCriticBody(f, kind), id()
			prefix := map[string]string{"receipt": "PZ", "transfer": "MM"}[kind]
			if operation == 2 {
				body["lines"] = []any{map[string]any{"item_id": f.item, "quantity": "0.0001"}}
				bad := f.request(h, "POST", "/api/v1/documents", body, key)
				if bad.Code != 400 {
					t.Logf("seed=%d step=%d invalid status=%d body=%s", seed, step, bad.Code, bad.Body)
					return false
				}
				body = numberingCriticBody(f, kind)
			}
			next := last[kind] + 1
			if operation == 1 {
				next += 2
				body["number"] = fmt.Sprintf("%s/%s/%06d", prefix, year, next)
			}
			created := f.request(h, "POST", "/api/v1/documents", body, key)
			if created.Code != 201 {
				t.Logf("seed=%d step=%d create status=%d body=%s", seed, step, created.Code, created.Body)
				return false
			}
			doc := data(t, created)["document_id"].(string)
			want := fmt.Sprintf("%s/%s/%06d", prefix, year, next)
			if got := f.scalar(`SELECT number FROM warehouse.documents WHERE id=$1`, doc); got != want {
				t.Logf("seed=%d step=%d number=%q want=%q", seed, step, got, want)
				return false
			}
			last[kind] = next
			if operation == 3 {
				for range 2 {
					replay := f.request(h, "POST", "/api/v1/documents", body, key)
					if replay.Code != 201 || replay.Body.String() != created.Body.String() {
						t.Logf("seed=%d step=%d replay changed result", seed, step)
						return false
					}
				}
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 48, Rand: rand.New(rand.NewSource(20261029))}); err != nil {
		t.Fatal(err)
	}
}
