//go:build integration && hardened

package postgres_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Every identifier and business row belongs to this test. The first warehouse
// is reused only as the historical profile's administrative login binding so
// the RED proves that a working first scope cannot reach a legitimate second.
func TestHardenedOnPremiseMultiWarehouse(t *testing.T) {
	connect := func(variable string) *pgxpool.Pool {
		t.Helper()
		if os.Getenv(variable) == "" {
			t.Fatalf("%s required: run the isolated profile fixture", variable)
		}
		pool, err := pgxpool.New(t.Context(), os.Getenv(variable))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	admin, runtime, workerPool := connect("WMS_HARDENED_ADMIN_DSN"), connect("WMS_HARDENED_API_DSN"), connect("WMS_HARDENED_WORKER_DSN")
	key, err := base64.StdEncoding.DecodeString(os.Getenv("WMS_HARDENED_KEY"))
	if err != nil || len(key) != 32 {
		t.Fatal("invalid synthetic request key")
	}
	var base map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("WMS_HARDENED_FIXTURE")), &base); err != nil {
		t.Fatal(err)
	}
	store, err := postgres.NewHardened(t.Context(), runtime, key)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := postgres.NewHardenedWorker(t.Context(), workerPool)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	actor, narrow := id(), id()
	exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'test',$1::uuid::text),($2,'test',$2::uuid::text)`, actor, narrow)
	type scopeFixture struct{ org, wh, owner, item, balance, document, line string }
	scopes := []scopeFixture{
		{org: base["org"], wh: base["wh"]},
		{org: base["org"], wh: id()},
		{org: id(), wh: id()},
	}
	var grants []inventory.Grant
	permissions := []string{"document.read", "document.create", "document.transition", "stock.read", "reservation.create", "reservation.release", "reservation.read", "movement.read", "movement.create", "operation.read"}
	for i := range scopes {
		s := &scopes[i]
		if i == 2 {
			exec(`INSERT INTO platform.organizations(id,code,name) VALUES($1,$1::uuid::text,'one client additional organization')`, s.org)
		}
		if i > 0 {
			exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, s.wh, s.org)
		}
		s.owner, s.item, s.document, s.line = id(), id(), id(), id()
		exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, s.owner, s.org)
		exec(`INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES($1,$2,$1::uuid::text,'ea',0,'none')`, s.item, s.org)
		location, position := id(), id()
		exec(`INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES($1,$2,$3,$1::uuid::text,'storage')`, location, s.org, s.wh)
		exec(`INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES($1,$2,$3,$4)`, position, s.org, s.wh, location)
		if err := admin.QueryRow(t.Context(), `SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available')`, s.org, s.wh, s.owner, s.item, position).Scan(&s.balance); err != nil {
			t.Fatal(err)
		}
		for _, permission := range permissions {
			exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, actor, s.org, s.wh, s.owner, permission)
			grants = append(grants, inventory.Grant{OrganizationID: s.org, WarehouseID: s.wh, OwnerID: s.owner, Permission: permission})
		}
		if i == 0 {
			exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.read')`, narrow, s.org, s.wh, s.owner)
		}
		receipt, receiptLine := id(), id()
		exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES($1,$2,$3,$4,'receipt',$1::uuid::text,'active'),($5,$2,$3,$4,'shipment',$5::uuid::text,'active')`, receipt, s.org, s.wh, s.owner, s.document)
		exec(`INSERT INTO warehouse.document_lines(id,document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty) VALUES($1,$2,$3,$4,$5,$6,1,10),($7,$8,$3,$4,$5,$6,1,10)`, receiptLine, receipt, s.org, s.wh, s.owner, s.item, s.line, s.document)
		exec(`SELECT inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,10)`, id(), actor, receiptLine, s.balance)
	}
	principal := inventory.Principal{ActorID: actor, Grants: grants}
	t.Run("one store resolves all authorized organizations and warehouses", func(t *testing.T) {
		p, err := store.ResolvePrincipal(t.Context(), auth.Identity{Issuer: "test", Subject: actor})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range scopes {
			if _, err := p.Scope("reservation.create", s.org, s.wh); err != nil {
				t.Errorf("legitimate scope omitted by shared store org=%s wh=%s: %v", s.org, s.wh, err)
			}
		}
	})
	t.Run("same store reads writes and replays each authorized warehouse", func(t *testing.T) {
		for i, s := range scopes {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				scope, err := principal.Scope("document.read", s.org, s.wh)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Get(t.Context(), inventory.DocumentResource, scope, s.document); err != nil {
					t.Errorf("legitimate document read denied: %v", err)
				}
				expires := time.Now().Add(time.Hour)
				command := inventory.Command{ID: id(), Kind: "reserve", OrganizationID: s.org, WarehouseID: s.wh, DocumentLineID: s.line, ExpectedDocumentVersion: 1, BalanceID: s.balance, Quantity: "2", ExpiresAt: &expires}
				first, err := store.Execute(t.Context(), principal, command, registry)
				if err != nil {
					t.Fatalf("legitimate warehouse command denied: %v", err)
				}
				replayed, err := store.Execute(t.Context(), principal, command, registry)
				if err != nil || string(first) != string(replayed) {
					t.Fatalf("replay changed result: %v", err)
				}
				var result struct {
					Reservation string `json:"reservation_id"`
				}
				if err := json.Unmarshal(first, &result); err != nil {
					t.Fatal(err)
				}
				_, err = store.Execute(t.Context(), principal, inventory.Command{ID: id(), Kind: "release", OrganizationID: s.org, WarehouseID: s.wh, ReservationID: result.Reservation, ExpectedDocumentVersion: 2, Quantity: "1", Reason: "onprem test"}, registry)
				if err != nil {
					t.Fatalf("release denied: %v", err)
				}
			})
		}
	})
	t.Run("one actor cannot borrow another actors warehouse rights", func(t *testing.T) {
		p, err := store.ResolvePrincipal(t.Context(), auth.Identity{Issuer: "test", Subject: narrow})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range scopes[1:] {
			if _, err := p.Scope("document.read", s.org, s.wh); err == nil {
				t.Fatal("narrow actor gained another warehouse")
			}
			forged := inventory.Scope{ActorID: narrow, OrganizationID: s.org, WarehouseID: s.wh, AllOwners: true}
			if _, err := store.Get(t.Context(), inventory.DocumentResource, forged, s.document); err == nil {
				t.Fatal("SQL accepted a forged user scope")
			}
		}
	})
	t.Run("scope mixing and failed commands preserve rollback and accounting", func(t *testing.T) {
		s := scopes[1]
		command := inventory.Command{ID: id(), Kind: "reserve", OrganizationID: s.org, WarehouseID: s.wh, DocumentLineID: scopes[2].line, ExpectedDocumentVersion: 1, BalanceID: s.balance, Quantity: "1"}
		if _, err := store.Execute(t.Context(), principal, command, registry); err == nil {
			t.Fatal("cross-organization line accepted")
		}
		var count int
		if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM platform.operations WHERE id=$1`, command.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("denied command left receipt count=%d err=%v", count, err)
		}
		for _, s := range scopes {
			var valid bool
			if err := admin.QueryRow(t.Context(), `SELECT b.on_hand=coalesce((SELECT sum(CASE WHEN l.target_balance_id=b.id THEN l.quantity ELSE -l.quantity END) FROM inventory.movement_lines l WHERE l.target_balance_id=b.id OR l.source_balance_id=b.id),0)
			 AND b.reserved_qty=coalesce((SELECT sum(active_qty) FROM inventory.reservations WHERE balance_id=b.id),0) FROM inventory.stock_balances b WHERE id=$1`, s.balance).Scan(&valid); err != nil || !valid {
				t.Fatalf("accounting mismatch: %v", err)
			}
		}
	})
	t.Run("one worker expires all installation warehouses", func(t *testing.T) {
		for _, s := range scopes {
			document, line := id(), id()
			exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES($1,$2,$3,$4,'shipment',$1::uuid::text,'active')`, document, s.org, s.wh, s.owner)
			exec(`INSERT INTO warehouse.document_lines(id,document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty) VALUES($1,$2,$3,$4,$5,$6,1,1)`, line, document, s.org, s.wh, s.owner, s.item)
			exec(`SELECT inventory.reserve_stock($1,$2,$3,1,$4,1,clock_timestamp()+interval '1 hour')`, id(), actor, line, s.balance)
			exec(`UPDATE inventory.reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE balance_id=$1 AND active_qty>0`, s.balance)
		}
		if _, err := worker.ExpireReservations(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, s := range scopes {
			var held string
			if err := admin.QueryRow(t.Context(), `SELECT reserved_qty::text FROM inventory.stock_balances WHERE id=$1`, s.balance).Scan(&held); err != nil || held != "0.000000" {
				t.Errorf("warehouse expiry incomplete: held=%s err=%v", held, err)
			}
		}
	})
	t.Run("worker drains all installation warehouses and fences stale ack", func(t *testing.T) {
		seen := map[string]bool{}
		for range 20 {
			events, err := worker.ClaimEvents(t.Context(), 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) == 0 {
				break
			}
			for _, event := range events {
				var warehouse string
				if err := admin.QueryRow(t.Context(), `SELECT p.warehouse_id::text FROM platform.outbox o JOIN platform.operations p ON p.id=o.operation_id WHERE o.id=$1`, event.ID).Scan(&warehouse); err != nil {
					t.Fatal(err)
				}
				seen[warehouse] = true
				if err := worker.CompleteEvent(t.Context(), event, true); err != nil {
					t.Fatal(err)
				}
				if err := worker.CompleteEvent(t.Context(), event, true); err == nil {
					t.Fatal("stale worker acknowledgement accepted")
				}
			}
		}
		for _, s := range scopes {
			if !seen[s.wh] {
				t.Errorf("shared worker did not drain legitimate warehouse %s", s.wh)
			}
		}
	})
}
