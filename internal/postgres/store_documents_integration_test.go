//go:build integration

package postgres_test

import (
	"encoding/json"
	"errors"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/jackc/pgx/v5/pgconn"
	"net/http/httptest"
	"testing"
)

func TestWarehouseReceiptPutawayPickAndShipViaAPI(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	for _, permission := range []string{"document.read", "document.create", "document.transition"} {
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, f.actor, f.org, f.wh, f.owner, permission)
	}
	create := func(kind string) (string, string) {
		body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": kind, "number": id(), "lines": []any{map[string]any{"item_id": f.item, "quantity": "4"}}}
		key := id()
		w := f.request(h, "POST", "/api/v1/documents", body, key)
		requireStatus(t, w, 201)
		replay := f.request(h, "POST", "/api/v1/documents", body, key)
		requireStatus(t, replay, 201)
		if replay.Body.String() != w.Body.String() {
			t.Fatal("document retry changed result")
		}
		d := data(t, w)
		doc := d["document_id"].(string)
		line := d["lines"].([]any)[0].(map[string]any)["id"].(string)
		transition := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 1, "state": "active"}
		requireStatus(t, f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, id()), 201)
		requireStatus(t, f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, id()), 409)
		return doc, line
	}
	closeDoc := func(doc string, version int, status int) {
		body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": version, "state": "closed"}
		requireStatus(t, f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", body, id()), status)
	}
	receipt, receiptLine := create("receipt")
	closeDoc(receipt, 2, 409)
	receive := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "document_line_id": receiptLine, "expected_document_version": 2, "kind": "receipt", "target_balance_id": f.source, "quantity": "4"}
	requireStatus(t, f.request(h, "POST", "/api/v1/movements", receive, id()), 201)
	closeDoc(receipt, 3, 201)
	putaway, putawayLine := create("transfer")
	requireStatus(t, f.request(h, "POST", "/api/v1/movements", f.movementBody(putawayLine, "transfer", "4", 2, ""), id()), 201)
	closeDoc(putaway, 3, 201)
	shipment, line := create("shipment")
	reserve := f.reserveBody(line, "4", 2)
	reserve["balance_id"] = f.target
	w := f.request(h, "POST", "/api/v1/reservations", reserve, id())
	requireStatus(t, w, 201)
	reservation := data(t, w)["reservation_id"].(string)
	closeDoc(shipment, 3, 409)
	cancel := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 3, "state": "cancelled"}
	requireStatus(t, f.request(h, "POST", "/api/v1/documents/"+shipment+"/transitions", cancel, id()), 409)
	// A picking transfer moves both stock and its reservation back to the dispatch position.
	pick := f.movementBody(line, "transfer", "4", 3, reservation)
	pick["source_balance_id"], pick["target_balance_id"] = f.target, f.source
	w = f.request(h, "POST", "/api/v1/movements", pick, id())
	requireStatus(t, w, 201)
	picked := data(t, w)["new_reservation_id"].(string)
	requireStatus(t, f.request(h, "POST", "/api/v1/movements", f.movementBody(line, "issue", "4", 4, picked), id()), 201)
	closeDoc(shipment, 5, 201)
	got := f.request(h, "GET", f.scopePath("documents")+"&limit=20", nil, "")
	requireStatus(t, got, 200)
	got = f.request(h, "GET", "/api/v1/documents/"+shipment+"?organization_id="+f.org+"&warehouse_id="+f.wh, nil, "")
	requireStatus(t, got, 200)
	d := data(t, got)
	if d["state"] != "closed" {
		t.Fatal(d)
	}
	lines := d["lines"].([]any)
	if lines[0].(map[string]any)["fulfilled_qty"] != "4.000000" {
		t.Fatal(lines)
	}
	if f.scalar(`SELECT sum(on_hand)::text FROM inventory.stock_balances WHERE organization_id=$1 AND owner_id=$2`, f.org, f.owner) != "10.000000" {
		t.Fatal("receipt/dispatch inventory mismatch")
	}
	if f.scalar(`SELECT count(*) FROM inventory.reservations WHERE organization_id=$1 AND active_qty>0`, f.org) != "0" {
		t.Fatal("shipment left reservations")
	}
}

func TestDocumentCommandsRejectScopeAndInvalidLines(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": "receipt", "number": id(), "lines": []any{map[string]any{"item_id": f.item, "quantity": "1"}}}
	requireStatus(t, f.request(h, "POST", "/api/v1/documents", body, id()), 403)
	for _, p := range []string{"document.create", "document.transition", "document.read"} {
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, f.actor, f.org, f.wh, f.owner, p)
	}
	body["owner_id"] = f.otherOwner
	requireStatus(t, f.request(h, "POST", "/api/v1/documents", body, id()), 403)
	body["owner_id"] = f.owner
	body["lines"] = []any{map[string]any{"item_id": f.item, "quantity": "0.0001"}} // SKU scale is 3.
	requireStatus(t, f.request(h, "POST", "/api/v1/documents", body, id()), 400)
	body["lines"] = []any{map[string]any{"item_id": f.item, "quantity": "1"}}
	w := f.request(h, "POST", "/api/v1/documents", body, id())
	requireStatus(t, w, 201)
	doc := data(t, w)["document_id"].(string)
	key := id()
	transition := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 1, "state": "cancelled"}
	requireStatus(t, f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, key), 201)
	requireStatus(t, f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, key), 201)
	transition["expected_document_version"] = 2
	transition["state"] = "active"
	requireStatus(t, f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, id()), 409)
	if f.scalar(`SELECT count(*) FROM warehouse.documents WHERE organization_id=$1`, f.org) != "3" {
		t.Fatal("failed creates left documents")
	}
}

func TestDocumentRetryIsAtomicAndSQLChecksPermissions(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": "receipt", "number": id(), "lines": []any{map[string]any{"item_id": f.item, "quantity": "1"}}}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.runtime.Exec(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, id(), f.actor, raw)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
		t.Fatalf("direct SQL lacked authorization: %v", err)
	}
	for _, p := range []string{"document.create", "document.transition", "document.read"} {
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, f.actor, f.org, f.wh, f.owner, p)
	}
	key := id()
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { responses <- f.request(h, "POST", "/api/v1/documents", body, key) }()
	}
	first, second := <-responses, <-responses
	requireStatus(t, first, 201)
	requireStatus(t, second, 201)
	if first.Body.String() != second.Body.String() {
		t.Fatal("concurrent retry changed result")
	}
	body["number"] = id()
	requireStatus(t, f.request(h, "POST", "/api/v1/documents", body, key), 409)
	doc := data(t, first)["document_id"].(string)
	other, _ := f.doc("receipt", "1", f.otherOwner)
	requireStatus(t, f.request(h, "GET", "/api/v1/documents/"+other+"?organization_id="+f.org+"&warehouse_id="+f.wh, nil, ""), 404)
	transition := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 1, "state": "active"}
	requireStatus(t, f.request(h, "POST", "/api/v1/documents/"+other+"/transitions", transition, id()), 404)
	f.exec(`DELETE FROM platform.actor_grants WHERE actor_id=$1 AND permission='document.transition'`, f.actor)
	raw, err = json.Marshal(transition)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.runtime.Exec(t.Context(), `SELECT warehouse.command_document($1,$2,$3,$4::jsonb)`, id(), f.actor, doc, raw)
	if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
		t.Fatalf("direct transition bypassed permission: %v", err)
	}
}

func TestNewWarehouseDocumentsRetainMandatorySQLGuards(t *testing.T) {
	f := newFixture(t)
	manifests, code := installSQLTestGuard(t, f, modules.MovementPoint, "deny", modules.SQLOnlyGuard("test.required"))
	h := f.handler(manifests, code)
	for _, p := range []string{"document.create", "document.transition"} {
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, f.actor, f.org, f.wh, f.owner, p)
	}
	body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": "receipt", "number": id(), "lines": []any{map[string]any{"item_id": f.item, "quantity": "1"}}}
	w := f.request(h, "POST", "/api/v1/documents", body, id())
	requireStatus(t, w, 201)
	d := data(t, w)
	doc := d["document_id"].(string)
	line := d["lines"].([]any)[0].(map[string]any)["id"].(string)
	transition := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 1, "state": "active"}
	requireStatus(t, f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, id()), 201)
	movement := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "document_line_id": line, "expected_document_version": 2, "kind": "receipt", "target_balance_id": f.source, "quantity": "1"}
	key := id()
	requireStatus(t, f.request(h, "POST", "/api/v1/movements", movement, key), 409)
	if f.scalar(`SELECT fulfilled_qty::text FROM warehouse.document_lines WHERE id=$1`, line) != "0.000000" {
		t.Fatal("guard denial changed inventory")
	}
	if f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, key) != "0" {
		t.Fatal("guard denial left a committed operation")
	}
	f.exec(`UPDATE mod_test_required.control SET mode='allow'`)
	requireStatus(t, f.request(h, "POST", "/api/v1/movements", movement, key), 201)
}
