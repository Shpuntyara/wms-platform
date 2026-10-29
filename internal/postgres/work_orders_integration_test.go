//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func orderRegistry(t *testing.T) *modules.Registry {
	t.Helper()
	r, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type workOrderBackend interface {
	CommandWorkOrder(context.Context, inventory.Principal, string, json.RawMessage, *modules.Registry) (json.RawMessage, error)
	ListWorkOrders(context.Context, inventory.Scope, inventory.Filters) (inventory.Page, error)
}

func orderBackend(t *testing.T, store any) workOrderBackend {
	t.Helper()
	b, ok := store.(workOrderBackend)
	if !ok {
		t.Fatal("backend has no durable work-order commands")
	}
	return b
}
func orderPrincipal(f *fixture) inventory.Principal {
	return inventory.Principal{ActorID: f.actor, Grants: []inventory.Grant{{OrganizationID: f.org, WarehouseID: f.wh, OwnerID: f.owner, Permission: "document.create"}, {OrganizationID: f.org, WarehouseID: f.wh, OwnerID: f.owner, Permission: "document.read"}}}
}
func orderSnapshot(f *fixture, quantity string) json.RawMessage {
	request := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "customer": "Fabryka Alfa", "reference": "", "lines": []any{map[string]string{"item_id": f.item, "quantity": quantity}}}
	raw, _ := json.Marshal(map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "customer": "Fabryka Alfa", "reference": "", "lines": request["lines"], "requirements": request["lines"], "steps": []string{}, "request": request})
	return raw
}

func TestWorkOrdersDurableReplayScopeAndRollback(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	backend := orderBackend(t, f.store)
	p := orderPrincipal(f)
	key := id()
	raw := orderSnapshot(f, "10")
	first, err := backend.CommandWorkOrder(t.Context(), p, key, raw, orderRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := backend.CommandWorkOrder(t.Context(), p, key, raw, orderRegistry(t))
	if err != nil || string(first) != string(replay) {
		t.Fatalf("unstable replay: %v", err)
	}
	var order map[string]any
	_ = json.Unmarshal(first, &order)
	if order["id"] == nil || order["state"] != "planned" || order["reference"] == "" {
		t.Fatalf("missing durable plan/reference: %s", first)
	}
	if _, err = backend.CommandWorkOrder(t.Context(), p, key, orderSnapshot(f, "11"), orderRegistry(t)); err == nil {
		t.Fatal("changed order accepted with old key")
	}
	scope, _ := p.Scope("document.read", f.org, f.wh)
	page, err := backend.ListWorkOrders(t.Context(), scope, inventory.Filters{Limit: 1})
	if err != nil || len(page.Data) != 1 {
		t.Fatalf("order not readable: %v %+v", err, page)
	}
	if f.scalar(`SELECT on_hand::text FROM inventory.stock_balances WHERE id=$1`, f.source) != "10.000000" {
		t.Fatal("planning changed real stock")
	}
	bad := map[string]any{}
	_ = json.Unmarshal(raw, &bad)
	bad["requirements"] = []any{map[string]string{"item_id": id(), "quantity": "1"}}
	invalid, _ := json.Marshal(bad)
	if _, err := backend.CommandWorkOrder(t.Context(), p, id(), invalid, orderRegistry(t)); err == nil {
		t.Fatal("foreign BOM item accepted")
	}
	if got := f.scalar(`SELECT count(*)::text FROM warehouse.work_orders WHERE organization_id=$1`, f.org); got != "1" {
		t.Fatalf("failed plan left rows: %s", got)
	}
	if got := f.scalar(`SELECT last_value::text FROM warehouse.work_order_counters WHERE organization_id=$1`, f.org); got != "1" {
		t.Fatalf("failed plan consumed counter: %s", got)
	}
	p.Grants[1].OwnerID = f.otherOwner
	scope, _ = p.Scope("document.read", f.org, f.wh)
	scope.OwnerIDs = []string{f.otherOwner}
	// Explicit owner filters cannot widen the authenticated scope.
	if _, err = backend.ListWorkOrders(t.Context(), scope, inventory.Filters{OwnerID: f.owner, Limit: 1}); err == nil {
		t.Fatal("foreign owner filter accepted")
	}
	denied := p
	denied.Grants = nil
	if _, err = backend.CommandWorkOrder(t.Context(), denied, id(), raw, orderRegistry(t)); err == nil {
		t.Fatal("worker planned order without permission")
	}
}

func TestWorkOrdersConcurrentAndGeneratedHistories(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	backend := orderBackend(t, f.store)
	p := orderPrincipal(f)
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for n := range 24 {
		wg.Go(func() {
			_, err := backend.CommandWorkOrder(t.Context(), p, id(), orderSnapshot(f, fmt.Sprint(n+1)), orderRegistry(t))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := f.scalar(`SELECT count(DISTINCT reference)::text FROM warehouse.work_orders WHERE organization_id=$1`, f.org); got != "24" {
		t.Fatalf("concurrent numbering collision: %s", got)
	}
	for n := range 32 {
		key := id()
		raw := orderSnapshot(f, fmt.Sprint(n+1))
		first, err := backend.CommandWorkOrder(t.Context(), p, key, raw, orderRegistry(t))
		if err != nil {
			t.Fatal(err)
		}
		for range n%3 + 1 {
			again, err := backend.CommandWorkOrder(t.Context(), p, key, raw, orderRegistry(t))
			if err != nil || string(first) != string(again) {
				t.Fatalf("history replay diverged: %v", err)
			}
		}
	}
	if got := f.scalar(`SELECT count(*)::text FROM warehouse.work_orders WHERE organization_id=$1`, f.org); got != "56" {
		t.Fatalf("generated histories duplicated: %s", got)
	}
	scope, _ := p.Scope("document.read", f.org, f.wh)
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := backend.ListWorkOrders(t.Context(), scope, inventory.Filters{Limit: 7, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range page.Data {
			var row struct{ ID string }
			_ = json.Unmarshal(raw, &row)
			if seen[row.ID] {
				t.Fatal("duplicate paginated order")
			}
			seen[row.ID] = true
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 56 {
		t.Fatalf("pagination lost orders: %d", len(seen))
	}
}

func TestWorkOrdersRecoverUnknownCommit(t *testing.T) {
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
			defer pool.Close()
			store, err := postgres.New(t.Context(), pool)
			if err != nil {
				t.Fatal(err)
			}
			b := orderBackend(t, store)
			key := id()
			raw := orderSnapshot(f, "10")
			p := orderPrincipal(f)
			_, err = b.CommandWorkOrder(t.Context(), p, key, raw, orderRegistry(t))
			var problem *inventory.Problem
			if !dropped.Load() || !errors.As(err, &problem) || problem.Status != 503 || problem.Code != "operation_interrupted" {
				t.Fatalf("unknown commit not retained: dropped=%t err=%v", dropped.Load(), err)
			}
			result, err := b.CommandWorkOrder(t.Context(), p, key, raw, orderRegistry(t))
			if err != nil {
				t.Fatal(err)
			}
			again, err := b.CommandWorkOrder(t.Context(), p, key, raw, orderRegistry(t))
			if err != nil || string(result) != string(again) {
				t.Fatal("retry changed order")
			}
			if got := f.scalar(`SELECT count(*)::text FROM warehouse.work_orders WHERE organization_id=$1`, f.org); got != "1" {
				t.Fatalf("commit retry duplicated plan: %s", got)
			}
		})
	}
}
