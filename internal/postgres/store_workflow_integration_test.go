//go:build integration

package postgres_test

import "testing"

func TestMovementAPIReceiptTransferAndCorrection(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	_, receipt := f.doc("receipt", "3", f.owner)
	body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "document_line_id": receipt, "expected_document_version": 1, "kind": "receipt", "target_balance_id": f.target, "quantity": "3"}
	w := f.request(h, "POST", "/api/v1/movements", body, id())
	requireStatus(t, w, 201)
	receiptID := data(t, w)["movement_id"].(string)
	_, transfer := f.doc("transfer", "2", f.owner)
	body = f.movementBody(transfer, "transfer", "2", 1, "")
	body["source_balance_id"], body["target_balance_id"] = f.target, f.source
	requireStatus(t, f.request(h, "POST", "/api/v1/movements", body, id()), 201)
	_, adjustment := f.doc("adjustment", "1", f.owner)
	body = f.movementBody(adjustment, "adjustment_out", "1", 1, "")
	body["source_balance_id"], body["correction_of"], body["reason"] = f.target, receiptID, "Correction after stock count"
	requireStatus(t, f.request(h, "POST", "/api/v1/movements", body, id()), 403)
	f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'movement.adjust')`, f.actor, f.org, f.wh, f.owner)
	w = f.request(h, "POST", "/api/v1/movements", body, id())
	requireStatus(t, w, 201)
	for balance, want := range map[string]string{f.source: "12.000000", f.target: "0.000000"} {
		if got := f.scalar(`SELECT on_hand::text FROM inventory.stock_balances WHERE id=$1`, balance); got != want {
			t.Fatalf("on_hand=%s want=%s", got, want)
		}
	}
	if got := f.scalar(`SELECT correction_of::text FROM inventory.movements WHERE id=$1`, data(t, w)["movement_id"]); got != receiptID {
		t.Fatalf("correction lost original movement reference: %s", got)
	}
	if got := f.scalar(`WITH deltas AS (
      SELECT target_balance_id AS balance_id, quantity AS qty FROM inventory.movement_lines WHERE target_balance_id IS NOT NULL
      UNION ALL SELECT source_balance_id, -quantity FROM inventory.movement_lines WHERE source_balance_id IS NOT NULL
    ), totals AS (SELECT balance_id,sum(qty) AS qty FROM deltas GROUP BY balance_id)
    SELECT count(*)::text FROM inventory.stock_balances b LEFT JOIN totals t ON t.balance_id=b.id
    WHERE b.organization_id=$1 AND b.on_hand<>coalesce(t.qty,0)`, f.org); got != "0" {
		t.Fatalf("API movements diverged from the ledger: %s", got)
	}
}

func TestPickingRelocatesReservationBeforeShipment(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	w := f.request(h, "POST", "/api/v1/reservations", f.reserveBody(f.line, "4", 1), id())
	requireStatus(t, w, 201)
	original := data(t, w)["reservation_id"].(string)
	w = f.request(h, "POST", "/api/v1/movements", f.movementBody(f.line, "transfer", "2", 2, original), id())
	requireStatus(t, w, 201)
	picked := data(t, w)["new_reservation_id"].(string)
	if got := f.scalar(`SELECT fulfilled_qty::text || '/' || reserved_qty::text FROM warehouse.document_lines WHERE id=$1`, f.line); got != "0.000000/4.000000" {
		t.Fatalf("picking changed shipment fulfillment: %s", got)
	}
	body := f.movementBody(f.line, "issue", "2", 3, picked)
	body["source_balance_id"] = f.target
	requireStatus(t, f.request(h, "POST", "/api/v1/movements", body, id()), 201)
	requireStatus(t, f.request(h, "POST", "/api/v1/movements", f.movementBody(f.line, "issue", "2", 4, original), id()), 201)
	if got := f.scalar(`SELECT fulfilled_qty::text || '/' || reserved_qty::text FROM warehouse.document_lines WHERE id=$1`, f.line); got != "4.000000/0.000000" {
		t.Fatalf("partial shipments violated document totals: %s", got)
	}
	if got := f.scalar(`SELECT sum(active_qty)::text FROM inventory.reservations WHERE document_line_id=$1`, f.line); got != "0.000000" {
		t.Fatalf("fulfilled shipment still reserves stock: %s", got)
	}
}

func TestWritesRejectCrossOwnerWarehouseAndOrganization(t *testing.T) {
	f, other := newFixture(t), newFixture(t)
	h := f.handler(nil, nil)
	warehouse, location, position := id(), id(), id()
	f.exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, warehouse, f.org)
	f.exec(`INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES($1,$2,$3,'remote-bin','storage')`, location, f.org, warehouse)
	f.exec(`INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES($1,$2,$3,$4)`, position, f.org, warehouse, location)
	foreignWarehouseBalance := f.scalar(`SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available')::text`, f.org, warehouse, f.owner, f.item, position)
	for _, tc := range []struct {
		name, org, line, balance string
		status                   int
	}{
		{"owner", f.org, f.line, f.foreign, 404},
		{"warehouse", f.org, f.line, foreignWarehouseBalance, 404},
		{"organization balance", f.org, f.line, other.source, 404},
		{"organization document", f.org, other.line, f.source, 404},
		{"forged scope", other.org, other.line, other.source, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := id()
			body := f.reserveBody(tc.line, "1", 1)
			body["organization_id"], body["balance_id"] = tc.org, tc.balance
			requireStatus(t, f.request(h, "POST", "/api/v1/reservations", body, command), tc.status)
			if got := f.scalar(`SELECT count(*)::text FROM platform.operations WHERE id=$1`, command); got != "0" {
				t.Fatal("out-of-scope write created an operation")
			}
		})
	}
}
