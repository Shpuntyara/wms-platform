//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func businessDocumentInput(f *fixture, kind string) map[string]any {
	return map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": kind,
		"recipient": "Fabryka Alfa — Piotr Nowak", "order_reference": "ZAM/2026/001",
		"lines": []any{map[string]any{"item_id": f.item, "quantity": "1"}}}
}

func businessSQLCreate(t *testing.T, f *fixture, command string, body map[string]any) (string, json.RawMessage) {
	t.Helper()
	raw, _ := json.Marshal(body)
	var response json.RawMessage
	if err := f.runtime.QueryRow(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, command, f.actor, raw).Scan(&response); err != nil {
		t.Fatalf("real SQL business document command failed: %v", err)
	}
	var result struct {
		ID string `json:"document_id"`
	}
	if err := json.Unmarshal(response, &result); err != nil || result.ID == "" {
		t.Fatalf("missing durable document identity: %s (%v)", response, err)
	}
	return result.ID, response
}

func TestDocumentBusinessSQLAutomaticWZAndImmutableContext(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	body, key := businessDocumentInput(f, "shipment"), id()
	document, first := businessSQLCreate(t, f, key, body)
	want := "WZ/" + numberingCriticYear(f) + "/000001"
	if got := f.scalar(`SELECT number FROM warehouse.documents WHERE id=$1`, document); got != want {
		t.Fatalf("automatic shipment number=%q want=%q", got, want)
	}
	for range 2 {
		again, replay := businessSQLCreate(t, f, key, body)
		if again != document || string(replay) != string(first) {
			t.Fatal("replayed issue changed document identity/result")
		}
	}
	scope := inventory.Scope{ActorID: f.actor, OrganizationID: f.org, WarehouseID: f.wh, OwnerIDs: []string{f.owner}}
	raw, err := f.store.Get(t.Context(), inventory.DocumentResource, scope, document)
	if err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	if detail["recipient"] != body["recipient"] || detail["order_reference"] != body["order_reference"] || detail["number"] != want {
		t.Fatalf("document lost its recipient/order identity: %s", raw)
	}
	body["recipient"] = "Inna firma"
	changed, _ := json.Marshal(body)
	if _, err := f.runtime.Exec(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, key, f.actor, changed); err == nil || !strings.Contains(err.Error(), "idempotency_key_reused") {
		t.Fatalf("same command accepted a different recipient: %v", err)
	}
	if got := f.scalar(`SELECT last_value::text FROM warehouse.document_number_counters WHERE organization_id=$1 AND warehouse_id=$2 AND kind='shipment'`, f.org, f.wh); got != "1" {
		t.Fatal("recipient conflict/replay consumed another WZ number")
	}
}

func TestDocumentBusinessSQLWorkerSpecificPermissions(t *testing.T) {
	for _, tc := range []struct{ name, permission, kind string }{
		{"issue worker", "document.issue", "shipment"}, {"move worker", "document.move", "transfer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, f.actor, f.org, f.wh, f.owner, tc.permission)
			businessSQLCreate(t, f, id(), businessDocumentInput(f, tc.kind))
			otherKind := "shipment"
			if tc.kind == "shipment" {
				otherKind = "transfer"
			}
			for _, denied := range []struct{ name, kind, owner string }{
				{"cannot create receipt", "receipt", f.owner}, {"cannot escape owner", tc.kind, f.otherOwner},
				{"cannot create the other worker document kind", otherKind, f.owner},
			} {
				t.Run(denied.name, func(t *testing.T) {
					body, key := businessDocumentInput(f, denied.kind), id()
					body["owner_id"] = denied.owner
					raw, _ := json.Marshal(body)
					if _, err := f.runtime.Exec(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, key, f.actor, raw); err == nil {
						t.Fatal("worker issue/move grant escalated document scope")
					}
					if count := f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, key); count != "0" {
						t.Fatal("forbidden command left a durable operation")
					}
				})
			}
		})
	}
}

func TestDocumentBusinessSQLRequiresRecipientEvenWithManualNumber(t *testing.T) {
	for _, number := range []string{"", "WZ-ZEWNETRZNE-17"} {
		for _, recipient := range []any{nil, "", " \u00a0\u3000", "Firma\nAlfa", 17, strings.Repeat("ą", 201)} {
			t.Run(fmt.Sprintf("number=%q/recipient=%v", number, recipient), func(t *testing.T) {
				f := newFixture(t)
				grantDocumentPermissions(f)
				body, key := businessDocumentInput(f, "shipment"), id()
				body["number"], body["recipient"] = number, recipient
				raw, _ := json.Marshal(body)
				if _, err := f.runtime.Exec(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, key, f.actor, raw); err == nil {
					t.Fatal("new WZ accepted an invalid recipient")
				}
				if got := f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, key); got != "0" {
					t.Fatal("invalid WZ retained an operation")
				}
			})
		}
	}
}

func TestDocumentBusinessWZConcurrentRollbackAndManualCollision(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	const total = 16
	type reply struct {
		document string
		err      error
	}
	results := make(chan reply, total)
	for range total {
		go func() {
			raw, _ := json.Marshal(businessDocumentInput(f, "shipment"))
			var result json.RawMessage
			err := f.runtime.QueryRow(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, id(), f.actor, raw).Scan(&result)
			var document struct {
				ID string `json:"document_id"`
			}
			if err == nil {
				err = json.Unmarshal(result, &document)
			}
			results <- reply{document.ID, err}
		}()
	}
	var numbers []string
	for range total {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		numbers = append(numbers, f.scalar(`SELECT number FROM warehouse.documents WHERE id=$1`, r.document))
	}
	sort.Strings(numbers)
	year := numberingCriticYear(f)
	for index, number := range numbers {
		if want := fmt.Sprintf("WZ/%s/%06d", year, index+1); number != want {
			t.Fatalf("serial=%q want=%q", number, want)
		}
	}
	tx, err := f.admin.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(businessDocumentInput(f, "shipment"))
	_, err = tx.Exec(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, id(), f.actor, raw)
	_ = tx.Rollback(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	body := businessDocumentInput(f, "shipment")
	body["number"] = "WZ/" + year + "/000041"
	businessSQLCreate(t, f, id(), body)
	doc, _ := businessSQLCreate(t, f, id(), businessDocumentInput(f, "shipment"))
	if got := f.scalar(`SELECT number FROM warehouse.documents WHERE id=$1`, doc); got != "WZ/"+year+"/000042" {
		t.Fatalf("rollback/manual collision damaged WZ series: %q", got)
	}
}

func TestDocumentBusinessWZUnknownCommitRecovery(t *testing.T) {
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
			h, key, body := f.handler(nil, nil), id(), businessDocumentInput(f, "shipment")
			first := f.request(h, "POST", "/api/v1/documents", body, key)
			requireStatus(t, first, 503)
			if !dropped.Load() || !strings.Contains(first.Body.String(), "same command identifier") {
				t.Fatal("actual COMMIT fault did not require same-command replay")
			}
			committed := "1"
			if mode == "before_send" {
				committed = "0"
			}
			if got := f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1 AND completed_at IS NOT NULL`, key); got != committed {
				t.Fatalf("committed before retry=%s want=%s", got, committed)
			}
			doc, number := numberingCriticCreate(t, f, h, body, key)
			if number != "WZ/"+numberingCriticYear(f)+"/000001" {
				t.Fatalf("recovery WZ=%q", number)
			}
			again, replayed := numberingCriticCreate(t, f, h, body, key)
			if again != doc || replayed != number {
				t.Fatal("lost response replay duplicated WZ identity")
			}
			_, next := numberingCriticCreate(t, f, h, body, id())
			if next != "WZ/"+numberingCriticYear(f)+"/000002" {
				t.Fatalf("lost COMMIT response consumed another number: %q", next)
			}
		})
	}
}

func TestDocumentBusinessWZPropertyRejectedAndReplayedCommandsPreserveSeries(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	year := numberingCriticYear(f)
	serial := 0
	property := func(choices []bool) bool {
		if len(choices) > 12 {
			choices = choices[:12]
		}
		for _, accept := range choices {
			body, key := businessDocumentInput(f, "shipment"), id()
			if !accept {
				body["lines"] = []any{map[string]any{"item_id": f.item, "quantity": "0"}}
			}
			raw, _ := json.Marshal(body)
			var first json.RawMessage
			err := f.runtime.QueryRow(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, key, f.actor, raw).Scan(&first)
			if !accept {
				if err == nil || f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, key) != "0" {
					return false
				}
				continue
			}
			if err != nil {
				return false
			}
			serial++
			var response struct {
				ID string `json:"document_id"`
			}
			if json.Unmarshal(first, &response) != nil {
				return false
			}
			if f.scalar(`SELECT number FROM warehouse.documents WHERE id=$1`, response.ID) != fmt.Sprintf("WZ/%s/%06d", year, serial) {
				return false
			}
			var replay json.RawMessage
			if err := f.runtime.QueryRow(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, key, f.actor, raw).Scan(&replay); err != nil || string(replay) != string(first) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 24, Rand: rand.New(rand.NewSource(8122026))}); err != nil {
		t.Fatal(err)
	}
}
