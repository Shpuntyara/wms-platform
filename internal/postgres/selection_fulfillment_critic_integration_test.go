//go:build integration

package postgres_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"testing"
	"testing/quick"
)

type selectionStock struct {
	f                         *fixture
	h                         http.Handler
	balances                  []string
	positions                 []string
	lot, serial, sku, catalog string
}

func selectionOptionalID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func selectionQuantity(micros int64) string {
	return fmt.Sprintf("%d.%06d", micros/1000000, micros%1000000)
}

func selectionFixture(t *testing.T, tracking string, total int64) *selectionStock {
	t.Helper()
	f := newFixture(t)
	s := &selectionStock{f: f, sku: "SKU-STALE-ALUMINIUM"}
	item := id()
	scale := 6
	if tracking == "serial" || tracking == "lot_serial" {
		scale = 0
	}
	f.exec(`INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES($1,$2,$3,'ea',$4,$5)`, item, f.org, s.sku, scale, tracking)
	f.item = item
	if tracking == "lot" || tracking == "lot_serial" {
		s.lot = id()
		f.exec(`INSERT INTO catalog.lots(id,organization_id,item_id,code) VALUES($1,$2,$3,'PARTIA-2027-A')`, s.lot, f.org, item)
	}
	if tracking == "serial" || tracking == "lot_serial" {
		s.serial = id()
		f.exec(`INSERT INTO catalog.serials(id,organization_id,item_id,lot_id,code) VALUES($1,$2,$3,$4,'SERYJNY-ALU-001')`, s.serial, f.org, item, selectionOptionalID(s.lot))
	}
	for _, code := range []string{"A1", "B2", "A4", "B4"} {
		position, location := id(), id()
		f.exec(`INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES($1,$2,$3,$4,'storage')`, location, f.org, f.wh, code)
		f.exec(`INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES($1,$2,$3,$4)`, position, f.org, f.wh, location)
		balance := f.scalar(`SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available',$6,$7)::text`, f.org, f.wh, f.owner, item, position, selectionOptionalID(s.lot), selectionOptionalID(s.serial))
		s.balances = append(s.balances, balance)
		s.positions = append(s.positions, position)
	}
	_, line := f.doc("receipt", selectionQuantity(total), f.owner)
	f.exec(`SELECT inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,$5::numeric)`, id(), f.actor, line, s.balances[0], selectionQuantity(total))
	for _, permission := range []string{"document.move", "document.issue", "document.read", "document.transition"} {
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, f.actor, f.org, f.wh, f.owner, permission)
	}
	s.h = f.handler(nil, nil)
	s.catalog = f.scalar(`SELECT jsonb_agg(to_jsonb(i) ORDER BY id)::text FROM catalog.items i WHERE organization_id=$1`, f.org)
	return s
}

func selectionDocument(t *testing.T, s *selectionStock, kind, quantity string) (string, string) {
	t.Helper()
	f := s.f
	body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": kind, "lines": []any{map[string]string{"item_id": f.item, "quantity": quantity}}}
	if kind == "shipment" {
		body["recipient"] = "Firma Alfa — Piotr Nowak"
	}
	// The employee selects item/stock IDs; neither a scanned code nor a SKU is
	// part of the posting request or a reason to create a new catalog item.
	created := f.request(s.h, "POST", "/api/v1/documents", body, id())
	requireStatus(t, created, 201)
	result := data(t, created)
	document := result["document_id"].(string)
	line := result["lines"].([]any)[0].(map[string]any)["id"].(string)
	activate := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 1, "state": "active"}
	requireStatus(t, f.request(s.h, "POST", "/api/v1/documents/"+document+"/transitions", activate, id()), 201)
	return document, line
}

func assertSelectionIdentity(t *testing.T, s *selectionStock, model []int64) {
	t.Helper()
	f := s.f
	if got := f.scalar(`SELECT jsonb_agg(to_jsonb(i) ORDER BY id)::text FROM catalog.items i WHERE organization_id=$1`, f.org); got != s.catalog {
		t.Fatal("a warehouse movement changed or created an item/SKU")
	}
	for i, balance := range s.balances {
		got := f.scalar(`SELECT jsonb_build_array(organization_id,warehouse_id,owner_id,item_id,position_id,coalesce(lot_id::text,''),coalesce(serial_id::text,''),stock_state,(on_hand*1000000)::bigint::text,(reserved_qty*1000000)::bigint::text)::text FROM inventory.stock_balances WHERE id=$1`, balance)
		var row []any
		if json.Unmarshal([]byte(got), &row) != nil {
			t.Fatal("invalid tracked balance evidence")
		}
		want := []string{f.org, f.wh, f.owner, f.item, s.positions[i], s.lot, s.serial, "available"}
		for column, value := range want {
			if row[column] != value {
				t.Fatalf("balance identity column%d changed: %s", column, got)
			}
		}
		if fmt.Sprint(row[8]) != strconv.FormatInt(model[i], 10) || fmt.Sprint(row[9]) != "0" {
			t.Fatalf("stock differs from independent micro-unit model: %s want=%d", got, model[i])
		}
	}
	assertKernelReconciles(t, f)
}

func selectionTransfer(t *testing.T, s *selectionStock, source, target int, amount int64) {
	t.Helper()
	f := s.f
	quantity := selectionQuantity(amount)
	document, line := selectionDocument(t, s, "transfer", quantity)
	body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "document_line_id": line, "expected_document_version": 2, "kind": "transfer", "source_balance_id": s.balances[source], "target_balance_id": s.balances[target], "quantity": quantity}
	key := id()
	first := f.request(s.h, "POST", "/api/v1/movements", body, key)
	requireStatus(t, first, 201)
	close := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 3, "state": "closed"}
	requireStatus(t, f.request(s.h, "POST", "/api/v1/documents/"+document+"/transitions", close, id()), 201)
	for range 2 {
		replayed := f.request(s.h, "POST", "/api/v1/movements", body, key)
		requireStatus(t, replayed, 201)
		if replayed.Body.String() != first.Body.String() {
			t.Fatal("MM replay after closing changed its result")
		}
	}
}

func TestSelectionMMChainsPreserveItemSKUOwnerLotAndSerial(t *testing.T) {
	for _, tracking := range []string{"none", "lot", "serial", "lot_serial"} {
		t.Run(tracking, func(t *testing.T) {
			total := int64(123456789)
			if tracking == "serial" || tracking == "lot_serial" {
				total = 1000000
			}
			s := selectionFixture(t, tracking, total)
			model := []int64{total, 0, 0, 0}
			for _, route := range [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 0}, {0, 2}} {
				selectionTransfer(t, s, route[0], route[1], total)
				model[route[0]] -= total
				model[route[1]] += total
				assertSelectionIdentity(t, s, model)
			}
		})
	}
}

func TestSelectionMMGeneratedPartialRoutesConserveExactIdentityAndQuantities(t *testing.T) {
	s := selectionFixture(t, "lot", 1000001)
	model := []int64{1000001, 0, 0, 0}
	transfers := 0
	property := func(choices []uint8) bool {
		if len(choices) > 8 {
			choices = choices[:8]
		}
		for _, choice := range choices {
			var candidates []int
			for i, quantity := range model {
				if quantity > 0 {
					candidates = append(candidates, i)
				}
			}
			source := candidates[int(choice)%len(candidates)]
			target := (source + 1 + int(choice>>3)%3) % 4
			amount := int64(choice)%model[source] + 1
			selectionTransfer(t, s, source, target, amount)
			transfers++
			model[source] -= amount
			model[target] += amount
			assertSelectionIdentity(t, s, model)
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 16, Rand: rand.New(rand.NewSource(202610291))}); err != nil {
		t.Fatal(err)
	}
	if transfers < 16 {
		t.Fatal("generated histories did not exercise enough real transfers")
	}
	t.Logf("16 generated route histories executed %d real MM transfers with exact micro-unit balances", transfers)
}

func TestSelectionMMRejectsAmbiguousTrackingAndForeignTargetWithoutEffects(t *testing.T) {
	for _, tracking := range []string{"lot", "serial", "lot_serial"} {
		t.Run(tracking, func(t *testing.T) {
			s := selectionFixture(t, tracking, 1000000)
			f := s.f
			lot, serial := "", ""
			if tracking == "lot" || tracking == "lot_serial" {
				lot = id()
				f.exec(`INSERT INTO catalog.lots(id,organization_id,item_id,code) VALUES($1,$2,$3,'PARTIA-INNA')`, lot, f.org, f.item)
			}
			if tracking == "serial" || tracking == "lot_serial" {
				serial = id()
				f.exec(`INSERT INTO catalog.serials(id,organization_id,item_id,lot_id,code) VALUES($1,$2,$3,$4,'SERYJNY-INNY')`, serial, f.org, f.item, selectionOptionalID(lot))
			}
			// The same displayed address and product can have multiple valid
			// balance IDs. The selected balance must retain its tracking identity.
			wrongTracking := f.scalar(`SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available',$6,$7)::text`, f.org, f.wh, f.owner, f.item, s.positions[1], selectionOptionalID(lot), selectionOptionalID(serial))
			wrongOwner := f.scalar(`SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available',$6,$7)::text`, f.org, f.wh, f.otherOwner, f.item, s.positions[1], selectionOptionalID(s.lot), selectionOptionalID(s.serial))
			otherItem := id()
			f.exec(`INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES($1,$2,'INNY-TOWAR','ea',6,'none')`, otherItem, f.org)
			wrongItem := f.scalar(`SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available')::text`, f.org, f.wh, f.owner, otherItem, s.positions[1])
			_, line := selectionDocument(t, s, "transfer", "1")
			for _, target := range []struct {
				name, balance string
				status        int
			}{{"another lot or serial at the same address", wrongTracking, 409}, {"another owner", wrongOwner, 404}, {"another item at the same address", wrongItem, 409}, {"source is not a destination", s.balances[0], 400}} {
				t.Run(target.name, func(t *testing.T) {
					before := kernelSnapshot(f)
					catalog := f.scalar(`SELECT jsonb_agg(to_jsonb(i) ORDER BY id)::text FROM catalog.items i WHERE organization_id=$1`, f.org)
					body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "document_line_id": line, "expected_document_version": 2, "kind": "transfer", "source_balance_id": s.balances[0], "target_balance_id": target.balance, "quantity": "1"}
					requireStatus(t, f.request(s.h, "POST", "/api/v1/movements", body, id()), target.status)
					if kernelSnapshot(f) != before || f.scalar(`SELECT jsonb_agg(to_jsonb(i) ORDER BY id)::text FROM catalog.items i WHERE organization_id=$1`, f.org) != catalog {
						t.Fatal("a rejected balance selection changed tracking, stock or operation receipts")
					}
				})
			}
		})
	}
}

func TestSelectionWZUsesSamePermissionsScopeVersionAndReplayWithoutScanner(t *testing.T) {
	s := selectionFixture(t, "lot", 10000000)
	f := s.f
	document, line := selectionDocument(t, s, "shipment", "2.125")
	reserve := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "document_line_id": line, "expected_document_version": 2, "balance_id": s.balances[0], "quantity": "2.125"}
	reserveKey := id()
	reserved := f.request(s.h, "POST", "/api/v1/reservations", reserve, reserveKey)
	requireStatus(t, reserved, 201)
	reservation := data(t, reserved)["reservation_id"].(string)
	body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "document_line_id": line, "expected_document_version": 3, "kind": "issue", "source_balance_id": s.balances[0], "reservation_id": reservation, "quantity": "2.125"}
	for _, attack := range []struct {
		name, field string
		value       any
		status      int
	}{
		{"foreign owner stock", "source_balance_id", f.foreign, 404}, {"stale document", "expected_document_version", 2, 409},
	} {
		t.Run(attack.name, func(t *testing.T) {
			snapshot := kernelSnapshot(f)
			raw, _ := json.Marshal(body)
			var altered map[string]any
			_ = json.Unmarshal(raw, &altered)
			altered[attack.field] = attack.value
			requireStatus(t, f.request(s.h, "POST", "/api/v1/movements", altered, id()), attack.status)
			if kernelSnapshot(f) != snapshot {
				t.Fatal("invalid selection changed stock or operation receipts")
			}
		})
	}
	issueKey := id()
	first := f.request(s.h, "POST", "/api/v1/movements", body, issueKey)
	requireStatus(t, first, 201)
	close := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 4, "state": "closed"}
	requireStatus(t, f.request(s.h, "POST", "/api/v1/documents/"+document+"/transitions", close, id()), 201)
	for range 2 {
		replayed := f.request(s.h, "POST", "/api/v1/movements", body, issueKey)
		requireStatus(t, replayed, 201)
		if replayed.Body.String() != first.Body.String() {
			t.Fatal("selected WZ replay duplicated or changed issue")
		}
	}
	f.exec(`DELETE FROM platform.actor_grants WHERE actor_id=$1 AND permission='movement.create'`, f.actor)
	snapshot := kernelSnapshot(f)
	requireStatus(t, f.request(s.h, "POST", "/api/v1/movements", body, issueKey), 403)
	if kernelSnapshot(f) != snapshot {
		t.Fatal("revoked permission replay changed stock")
	}
	assertSelectionIdentity(t, s, []int64{7875000, 0, 0, 0})
}
