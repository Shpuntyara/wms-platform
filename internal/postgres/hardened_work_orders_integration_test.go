//go:build integration && hardened

package postgres_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type signedOrderFixture struct {
	admin, runtime                                                    *pgxpool.Pool
	store                                                             *postgres.HardenedStore
	key                                                               []byte
	org, wh, owner, otherOwner, otherWh, item, bureau, worker, denied string
	bureauPrincipal, workerPrincipal                                  inventory.Principal
}

func newSignedOrderFixture(t *testing.T) *signedOrderFixture {
	t.Helper()
	f := &signedOrderFixture{admin: onPremCriticPool(t, "WMS_HARDENED_ADMIN_DSN"), runtime: onPremCriticPool(t, "WMS_HARDENED_API_DSN"),
		org: id(), wh: id(), owner: id(), otherOwner: id(), otherWh: id(), item: id(), bureau: id(), worker: id(), denied: id()}
	var err error
	f.key, err = base64.StdEncoding.DecodeString(os.Getenv("WMS_HARDENED_KEY"))
	if err != nil || len(f.key) != 32 {
		t.Fatal("a synthetic signed-context key is required")
	}
	f.store, err = postgres.NewHardened(t.Context(), f.runtime, f.key)
	if err != nil {
		t.Fatal("cannot construct actual signed order store")
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.admin.Exec(t.Context(), query, args...); err != nil {
			t.Fatal("cannot seed independent signed order fixture")
		}
	}
	exec(`INSERT INTO platform.organizations(id,code,name) VALUES($1,$1::uuid::text,'signed order critic')`, f.org)
	exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$3,$1::uuid::text),($2,$3,$2::uuid::text)`, f.wh, f.otherWh, f.org)
	exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$3,$1::uuid::text),($2,$3,$2::uuid::text)`, f.owner, f.otherOwner, f.org)
	exec(`INSERT INTO catalog.units(code,dimension) VALUES('ea','count') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES($1,$2,$1::uuid::text,'ea',0,'none')`, f.item, f.org)
	for _, actor := range []string{f.bureau, f.worker, f.denied} {
		exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'signed-order-critic',$1::uuid::text)`, actor)
	}
	for _, permission := range []string{"document.create", "document.read"} {
		exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, f.bureau, f.org, f.wh, f.owner, permission)
	}
	for _, permission := range []string{"document.read", "document.issue", "document.move"} {
		exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, f.worker, f.org, f.wh, f.owner, permission)
	}
	f.bureauPrincipal, err = f.store.ResolvePrincipal(t.Context(), auth.Identity{Issuer: "signed-order-critic", Subject: f.bureau})
	if err != nil {
		t.Fatal("cannot resolve order planner")
	}
	f.workerPrincipal, err = f.store.ResolvePrincipal(t.Context(), auth.Identity{Issuer: "signed-order-critic", Subject: f.worker})
	if err != nil {
		t.Fatal("cannot resolve order reader")
	}
	return f
}

func (f *signedOrderFixture) body(quantity string) json.RawMessage {
	request := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "customer": "Fabryka Alfa", "reference": "", "lines": []any{map[string]string{"item_id": f.item, "quantity": quantity}}}
	raw, _ := json.Marshal(map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "customer": "Fabryka Alfa", "reference": "", "lines": request["lines"], "requirements": request["lines"], "steps": []string{}, "request": request})
	return raw
}

func TestHardenedWorkOrdersSignedReadCreateReplayAndPermissions(t *testing.T) {
	f := newSignedOrderFixture(t)
	body, key := f.body("10"), id()
	first, err := f.store.CommandWorkOrder(t.Context(), f.bureauPrincipal, key, body, orderRegistry(t))
	if err != nil {
		t.Fatal("real signed order command failed: ", err)
	}
	again, err := f.store.CommandWorkOrder(t.Context(), f.bureauPrincipal, key, body, orderRegistry(t))
	if err != nil || string(first) != string(again) {
		t.Fatal("signed plan replay changed its durable result")
	}
	var created struct{ ID, Reference, Customer, State string }
	if json.Unmarshal(first, &created) != nil || created.ID == "" || created.Reference == "" || created.State != "planned" || created.Customer != "Fabryka Alfa" {
		t.Fatal("signed plan result lost its business identity")
	}
	if _, err := f.store.CommandWorkOrder(t.Context(), f.bureauPrincipal, key, f.body("11"), orderRegistry(t)); err == nil {
		t.Fatal("signed order reused command with changed demand")
	}
	scope, err := f.workerPrincipal.Scope("document.read", f.org, f.wh)
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.store.ListWorkOrders(t.Context(), scope, inventory.Filters{Limit: 1})
	if err != nil || len(page.Data) != 1 {
		t.Fatalf("worker cannot read actual signed plan: %v", err)
	}
	var read struct{ ID, Reference string }
	_ = json.Unmarshal(page.Data[0], &read)
	if read.ID != created.ID || read.Reference != created.Reference {
		t.Fatal("worker read another signed plan")
	}
	if _, err := f.store.CommandWorkOrder(t.Context(), f.workerPrincipal, id(), body, orderRegistry(t)); err == nil {
		t.Fatal("worker may plan customer orders without bureau permission")
	}
	for _, attack := range []struct{ name, actor, owner, wh string }{
		{"forged actor", f.denied, f.owner, f.wh}, {"forged worker create", f.worker, f.owner, f.wh},
		{"forged owner", f.bureau, f.otherOwner, f.wh}, {"forged warehouse", f.bureau, f.owner, f.otherWh},
	} {
		t.Run(attack.name, func(t *testing.T) {
			var input map[string]any
			_ = json.Unmarshal(body, &input)
			input["owner_id"], input["warehouse_id"] = attack.owner, attack.wh
			raw, _ := json.Marshal(input)
			forged := inventory.Principal{ActorID: attack.actor, Grants: []inventory.Grant{{OrganizationID: f.org, WarehouseID: attack.wh, Permission: "document.create"}}}
			if _, err := f.store.CommandWorkOrder(t.Context(), forged, id(), raw, orderRegistry(t)); err == nil {
				t.Fatal("forged Go grants escalated real signed plan scope")
			}
		})
	}
	if _, err := f.store.CommandWorkOrder(t.Context(), f.bureauPrincipal, id(), body, nil); err == nil {
		t.Fatal("signed plan bypassed module registry validation")
	}
	for _, query := range []string{
		`SELECT * FROM warehouse.work_orders`,
		`SELECT * FROM warehouse.work_order_counters`,
		`SELECT warehouse.command_work_order($1::uuid,$2::uuid,$3::jsonb)`,
	} {
		t.Run(query, func(t *testing.T) {
			var err error
			if query == `SELECT warehouse.command_work_order($1::uuid,$2::uuid,$3::jsonb)` {
				_, err = f.runtime.Exec(t.Context(), query, id(), f.bureau, body)
			} else {
				_, err = f.runtime.Exec(t.Context(), query)
			}
			var problem *pgconn.PgError
			if !errors.As(err, &problem) || problem.Code != "42501" {
				t.Fatal("stolen SQL login reached private order surface")
			}
		})
	}
}

func TestHardenedWorkOrdersRPCRejectsUnsignedTamperedAndWrongActions(t *testing.T) {
	f := newSignedOrderFixture(t)
	body, command, filter := string(f.body("10")), id(), `{"limit":2}`
	var changed map[string]any
	_ = json.Unmarshal([]byte(body), &changed)
	changed["customer"] = "Inna firma"
	tampered, _ := json.Marshal(changed)
	for _, attack := range []struct {
		name, action, query string
		args                []any
	}{
		{"unsigned read", "", `SELECT * FROM hardened_api.work_orders($1)`, []any{filter}},
		{"unsigned create", "", `SELECT hardened_api.work_order_command($1::uuid,$2)`, []any{command, body}},
		{"read action cannot create", "work_orders|" + filter, `SELECT hardened_api.work_order_command($1::uuid,$2)`, []any{command, body}},
		{"another command ID", "work_order_create|" + command + "|" + body, `SELECT hardened_api.work_order_command($1::uuid,$2)`, []any{id(), body}},
		{"tampered exact body", "work_order_create|" + command + "|" + body, `SELECT hardened_api.work_order_command($1::uuid,$2)`, []any{command, string(tampered)}},
		{"tampered read filters", "work_orders|" + filter, `SELECT * FROM hardened_api.work_orders($1)`, []any{`{"limit":200}`}},
	} {
		t.Run(attack.name, func(t *testing.T) {
			tx, err := f.runtime.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if attack.action != "" {
				var pid int
				var xid string
				if err := tx.QueryRow(t.Context(), `SELECT pg_backend_pid(),pg_current_xact_id()::text`).Scan(&pid, &xid); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256([]byte(attack.action))
				payload := fmt.Sprintf("2|%s|%s|%d|%s|%s|%s|%s|%d|%x", f.store.InstallationID(), f.store.RuntimeLogin(), pid, xid, f.bureau, f.org, f.wh, time.Now().Unix()+20, digest)
				mac := hmac.New(sha256.New, f.key)
				_, _ = mac.Write([]byte(payload))
				if _, err := tx.Exec(t.Context(), `SELECT set_config('wms.context',$1,true),set_config('wms.context_mac',$2,true)`, payload, hex.EncodeToString(mac.Sum(nil))); err != nil {
					t.Fatal("cannot establish owned signed action probe")
				}
			}
			_, err = tx.Exec(t.Context(), attack.query, attack.args...)
			var problem *pgconn.PgError
			if !errors.As(err, &problem) || problem.Code != "42501" {
				t.Fatal("order RPC accepted absent/wrong signed action or tampered payload")
			}
		})
	}
}

type armedOrderCommitFault struct {
	*documentCommitFault
	armed *atomic.Bool
}

func (c *armedOrderCommitFault) Read(b []byte) (int, error) {
	if c.armed.Load() {
		return c.documentCommitFault.Read(b)
	}
	return c.Conn.Read(b)
}
func (c *armedOrderCommitFault) Write(b []byte) (int, error) {
	if c.armed.Load() {
		return c.documentCommitFault.Write(b)
	}
	return c.Conn.Write(b)
}

func TestHardenedWorkOrdersUnknownCommitSignedReplay(t *testing.T) {
	for _, mode := range []string{"before_send", "after_ack_eof", "after_ack_generic"} {
		t.Run(mode, func(t *testing.T) {
			f := newSignedOrderFixture(t)
			cfg, err := pgxpool.ParseConfig(os.Getenv("WMS_HARDENED_API_DSN"))
			if err != nil {
				t.Fatal(err)
			}
			var armed, dropped atomic.Bool
			cfg.MaxConns = 1
			cfg.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return &armedOrderCommitFault{documentCommitFault: &documentCommitFault{Conn: conn, mode: mode, dropped: &dropped}, armed: &armed}, nil
			}
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			store, err := postgres.NewHardened(t.Context(), pool, f.key)
			if err != nil {
				t.Fatal("cannot initialize unarmed signed store")
			}
			body, key := f.body("10"), id()
			armed.Store(true)
			_, err = store.CommandWorkOrder(t.Context(), f.bureauPrincipal, key, body, orderRegistry(t))
			var problem *inventory.Problem
			if !dropped.Load() || !errors.As(err, &problem) || problem.Code != "operation_interrupted" || problem.Status != 503 {
				t.Fatal("signed COMMIT fault did not retain same-command recovery")
			}
			var count int
			if err := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM platform.operations WHERE id=$1 AND completed_at IS NOT NULL`, key).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 1
			if mode == "before_send" {
				want = 0
			}
			if count != want {
				t.Fatalf("signed committed receipts before replay=%d want=%d", count, want)
			}
			first, err := store.CommandWorkOrder(t.Context(), f.bureauPrincipal, key, body, orderRegistry(t))
			if err != nil {
				t.Fatal("signed order retry failed")
			}
			again, err := store.CommandWorkOrder(t.Context(), f.bureauPrincipal, key, body, orderRegistry(t))
			if err != nil || string(first) != string(again) {
				t.Fatal("signed order replay duplicated/changed result")
			}
			if err := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM warehouse.work_orders WHERE organization_id=$1`, f.org).Scan(&count); err != nil || count != 1 {
				t.Fatal("signed COMMIT recovery duplicated customer plan")
			}
		})
	}
}
