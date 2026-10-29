//go:build integration && hardened

package postgres_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type signedHandoffFixture struct {
	*handoffFixture
	signed *signedOrderFixture
}

func TestHardenedHandoffsProfileReapplicationPreservesOwnerAndACL(t *testing.T) {
	f := newSignedHandoffFixture(t)
	before := f.scalar(`SELECT jsonb_agg(jsonb_build_array(p.proname,p.proowner,p.proacl) ORDER BY p.proname)::text FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='hardened_api' AND p.proname IN('handoff_context','handoffs','handoff','handoff_command')`)
	source, err := os.ReadFile(filepath.Join("..", "..", "db", "profiles", "handoffs-v1.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = f.admin.Exec(t.Context(), string(source)); err != nil {
			t.Fatal("HANDOFF_ASSERTION additive handoff RPC profile cannot be safely reapplied:", err)
		}
	}
	after := f.scalar(`SELECT jsonb_agg(jsonb_build_array(p.proname,p.proowner,p.proacl) ORDER BY p.proname)::text FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='hardened_api' AND p.proname IN('handoff_context','handoffs','handoff','handoff_command')`)
	if before != after {
		t.Fatal("HANDOFF_ASSERTION profile reapplication changed function ownership or grants")
	}
}

func newSignedHandoffFixture(t *testing.T) *signedHandoffFixture {
	t.Helper()
	signed := newSignedOrderFixture(t)
	f := &fixture{t: t, admin: signed.admin, runtime: signed.runtime, org: signed.org, wh: signed.wh, owner: signed.owner, otherOwner: signed.otherOwner, item: signed.item, actor: signed.bureau}
	f.exec(`UPDATE catalog.items SET quantity_scale=3 WHERE id=$1`, f.item)
	pos, loc := id(), id()
	f.exec(`INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES($1,$2,$3,$1::uuid::text,'storage')`, loc, f.org, f.wh)
	f.exec(`INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES($1,$2,$3,$4)`, pos, f.org, f.wh, loc)
	if err := f.admin.QueryRow(t.Context(), `SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available')`, f.org, f.wh, f.owner, f.item, pos).Scan(&f.source); err != nil {
		t.Fatal(err)
	}
	_, line := f.doc("receipt", "10", f.owner)
	f.exec(`SELECT inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,10)`, id(), f.actor, line, f.source)
	f.exec(`DELETE FROM platform.actor_grants WHERE actor_id=$1`, f.actor)
	return &signedHandoffFixture{handoffFixture: configureHandoffFixture(t, f), signed: signed}
}
func (f *signedHandoffFixture) principal(actor string) inventory.Principal {
	p := inventory.Principal{ActorID: actor}
	for _, permission := range []string{"handoff.read", "handoff.send", "handoff.accept"} {
		p.Grants = append(p.Grants, inventory.Grant{OrganizationID: f.org, WarehouseID: f.wh, OwnerID: f.owner, Permission: permission})
	}
	return p
}
func handoffRaw(body map[string]any) json.RawMessage { raw, _ := json.Marshal(body); return raw }
func signedHandoffID(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var result struct {
		ID string `json:"handoff_id"`
	}
	if json.Unmarshal(raw, &result) != nil || result.ID == "" {
		t.Fatal("signed handoff did not return compact identity")
	}
	return result.ID
}

func TestHardenedHandoffsSignedSendAcceptReadReplayAndContainment(t *testing.T) {
	f := newSignedHandoffFixture(t)
	store := f.signed.store
	body, key := handoffRaw(f.sendBody("1.125")), id()
	first, err := store.CommandHandoff(t.Context(), f.principal(f.sender), key, "", body, orderRegistry(t))
	if err != nil {
		t.Fatal("signed send failed", err)
	}
	handoff := signedHandoffID(t, first)
	again, err := store.CommandHandoff(t.Context(), f.principal(f.sender), key, "", body, orderRegistry(t))
	if err != nil || string(first) != string(again) {
		t.Fatal("HANDOFF_ASSERTION signed send replay changed")
	}
	scope := inventory.Scope{ActorID: f.receiver, OrganizationID: f.org, WarehouseID: f.wh, OwnerIDs: []string{f.owner}}
	if _, err = store.HandoffContext(t.Context(), scope); err != nil {
		t.Fatal("signed department context failed", err)
	}
	page, err := store.ListHandoffs(t.Context(), scope, inventory.Filters{Limit: 1})
	if err != nil || len(page.Data) != 1 {
		t.Fatal("signed handoff list failed", err)
	}
	if _, err = store.GetHandoff(t.Context(), scope, handoff); err != nil {
		t.Fatal("signed handoff detail failed", err)
	}
	if _, err = store.CommandHandoff(t.Context(), f.principal(f.sender), id(), handoff, handoffRaw(f.acceptBody(1)), orderRegistry(t)); err == nil {
		t.Fatal("HANDOFF_ASSERTION sender signed own receipt")
	}
	accepted, err := store.CommandHandoff(t.Context(), f.principal(f.receiver), id(), handoff, handoffRaw(f.acceptBody(1)), orderRegistry(t))
	if err != nil {
		t.Fatal("signed independent accept failed", err)
	}
	_ = accepted
	fresh, err := store.GetHandoff(t.Context(), scope, handoff)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	_ = json.Unmarshal(fresh, &record)
	if record["sender_actor_id"] != f.sender || record["receiver_actor_id"] != f.receiver || record["quantity"] != "1.125000" || record["state"] != "accepted" {
		t.Fatal("HANDOFF_ASSERTION signed record changed authenticated custody")
	}
	for _, actor := range []string{f.outsider, f.receiver} {
		forged := f.principal(actor)
		if _, err = store.CommandHandoff(t.Context(), forged, id(), "", body, orderRegistry(t)); err == nil {
			t.Fatal("HANDOFF_ASSERTION forged Go grants bypassed source membership")
		}
	}
	if _, err = store.CommandHandoff(t.Context(), f.principal(f.sender), id(), "", body, nil); err == nil {
		t.Fatal("HANDOFF_ASSERTION signed handoff bypassed module registry")
	}
	f.exec(`UPDATE warehouse.department_members SET enabled=false WHERE actor_id=$1`, f.sender)
	if _, err = store.CommandHandoff(t.Context(), f.principal(f.sender), key, "", body, orderRegistry(t)); err == nil {
		t.Fatal("HANDOFF_ASSERTION revoked source membership still replays signed send")
	}
	for _, query := range []string{
		`SELECT * FROM warehouse.handoffs`, `SELECT * FROM warehouse.department_employees`, `SELECT * FROM warehouse.department_members`, `SELECT * FROM warehouse.departments`, `SELECT * FROM warehouse.department_positions`, `SELECT * FROM warehouse.department_routes`, `SELECT * FROM warehouse.handoff_transit_positions`,
		`SELECT warehouse.command_handoff(NULL,NULL,NULL,'{}'::jsonb)`, `SELECT warehouse.read_handoff(NULL,NULL,NULL,NULL)`, `SELECT * FROM warehouse.read_handoffs(NULL,NULL,NULL,'{}'::jsonb)`, `SELECT warehouse.handoff_context(NULL,NULL,NULL)`,
	} {
		t.Run(query, func(t *testing.T) {
			_, err := f.runtime.Exec(t.Context(), query)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
				t.Fatal("HANDOFF_ASSERTION stolen SQL login reached private custody surface")
			}
		})
	}
}

func TestHardenedHandoffsRPCRejectsUnsignedTamperedAndWrongActions(t *testing.T) {
	f := newSignedHandoffFixture(t)
	body, command, filter := string(handoffRaw(f.sendBody("1"))), id(), `{"limit":1}`
	for _, attack := range []struct {
		name, action, query string
		args                []any
		tamperActor         bool
	}{
		{"unsigned context", "", `SELECT hardened_api.handoff_context()`, nil, false},
		{"unsigned list", "", `SELECT * FROM hardened_api.handoffs($1)`, []any{filter}, false},
		{"unsigned detail", "", `SELECT hardened_api.handoff($1::uuid)`, []any{command}, false},
		{"unsigned command", "", `SELECT hardened_api.handoff_command($1::uuid,NULL,$2)`, []any{command, body}, false},
		{"read action cannot send", "handoffs|" + filter, `SELECT hardened_api.handoff_command($1::uuid,NULL,$2)`, []any{command, body}, false},
		{"another command ID", "handoff_command|" + command + "||" + body, `SELECT hardened_api.handoff_command($1::uuid,NULL,$2)`, []any{id(), body}, false},
		{"another handoff ID", "handoff_command|" + command + "||" + body, `SELECT hardened_api.handoff_command($1::uuid,$2::uuid,$3)`, []any{command, id(), body}, false},
		{"tampered exact body", "handoff_command|" + command + "||" + body, `SELECT hardened_api.handoff_command($1::uuid,NULL,$2)`, []any{command, string(handoffRaw(f.sendBody("2")))}, false},
		{"tampered list filters", "handoffs|" + filter, `SELECT * FROM hardened_api.handoffs($1)`, []any{`{"limit":200}`}, false},
		{"another detail ID", "handoff|" + command, `SELECT hardened_api.handoff($1::uuid)`, []any{id()}, false},
		{"another context action", "handoffs|" + filter, `SELECT hardened_api.handoff_context()`, nil, false},
		{"tampered authenticated actor", "handoff_context", `SELECT hardened_api.handoff_context()`, nil, true},
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
				if err = tx.QueryRow(t.Context(), `SELECT pg_backend_pid(),pg_current_xact_id()::text`).Scan(&pid, &xid); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256([]byte(attack.action))
				payload := fmt.Sprintf("2|%s|%s|%d|%s|%s|%s|%s|%d|%x", f.signed.store.InstallationID(), f.signed.store.RuntimeLogin(), pid, xid, f.sender, f.org, f.wh, time.Now().Unix()+20, digest)
				mac := hmac.New(sha256.New, f.signed.key)
				_, _ = mac.Write([]byte(payload))
				if attack.tamperActor {
					payload = fmt.Sprintf("2|%s|%s|%d|%s|%s|%s|%s|%d|%x", f.signed.store.InstallationID(), f.signed.store.RuntimeLogin(), pid, xid, f.outsider, f.org, f.wh, time.Now().Unix()+20, digest)
				}
				if _, err = tx.Exec(t.Context(), `SELECT set_config('wms.context',$1,true),set_config('wms.context_mac',$2,true)`, payload, hex.EncodeToString(mac.Sum(nil))); err != nil {
					t.Fatal("cannot establish owned signed handoff probe")
				}
			}
			_, err = tx.Exec(t.Context(), attack.query, attack.args...)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
				t.Fatal("HANDOFF_ASSERTION custody RPC accepted absent/wrong signed action or changed payload")
			}
		})
	}
}
func TestHardenedHandoffsUnknownCommitSignedReplay(t *testing.T) {
	for _, action := range []string{"send", "accept"} {
		for _, mode := range []string{"before_send", "after_ack_eof", "after_ack_generic"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				f := newSignedHandoffFixture(t)
				actor, handoff, body := f.sender, "", handoffRaw(f.sendBody("1.125"))
				if action == "accept" {
					raw, err := f.signed.store.CommandHandoff(t.Context(), f.principal(f.sender), id(), "", body, orderRegistry(t))
					if err != nil {
						t.Fatal(err)
					}
					handoff = signedHandoffID(t, raw)
					actor = f.receiver
					body = handoffRaw(f.acceptBody(1))
				}
				cfg, err := pgxpool.ParseConfig(os.Getenv("WMS_HARDENED_API_DSN"))
				if err != nil {
					t.Fatal(err)
				}
				cfg.MaxConns = 1
				var armed, dropped atomic.Bool
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
				store, err := postgres.NewHardened(t.Context(), pool, f.signed.key)
				if err != nil {
					t.Fatal("cannot initialize signed custody store")
				}
				key := id()
				armed.Store(true)
				_, err = store.CommandHandoff(t.Context(), f.principal(actor), key, handoff, body, orderRegistry(t))
				var problem *inventory.Problem
				if !dropped.Load() || !errors.As(err, &problem) || problem.Code != "operation_interrupted" || problem.Status != 503 {
					t.Fatal("HANDOFF_ASSERTION signed COMMIT fault did not retain identical command recovery")
				}
				want := "1"
				if mode == "before_send" {
					want = "0"
				}
				if f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1 AND completed_at IS NOT NULL`, key) != want {
					t.Fatal("HANDOFF_ASSERTION signed durable receipt before retry differs from wire fault")
				}
				first, err := store.CommandHandoff(t.Context(), f.principal(actor), key, handoff, body, orderRegistry(t))
				if err != nil {
					t.Fatal("signed handoff retry failed", err)
				}
				again, err := store.CommandHandoff(t.Context(), f.principal(actor), key, handoff, body, orderRegistry(t))
				if err != nil || string(first) != string(again) {
					t.Fatal("HANDOFF_ASSERTION signed retry changed durable result")
				}
				if f.scalar(`SELECT count(*) FROM warehouse.handoffs WHERE organization_id=$1`, f.org) != "1" || f.scalar(`SELECT on_hand::text FROM inventory.stock_balances WHERE id=$1`, f.source) != "8.875000" {
					t.Fatal("HANDOFF_ASSERTION signed retry duplicated inventory write")
				}
			})
		}
	}
}
