//go:build integration && hardened

package postgres_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/api"
	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

type hardenedAuth struct{ subject string }

func (a hardenedAuth) Authenticate(context.Context, string) (auth.Identity, error) {
	return auth.Identity{Issuer: "test", Subject: a.subject}, nil
}

func TestHardenedHTTPAndContainment(t *testing.T) {
	if os.Getenv("WMS_HARDENED_API_DSN") == "" {
		t.Fatal("run tests/hardened_profile.py --go for disposable hardened fixture")
	}
	pool := func(name string) *pgxpool.Pool {
		t.Helper()
		p, err := pgxpool.New(t.Context(), os.Getenv(name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	admin, runtime, workerPool := pool("WMS_HARDENED_ADMIN_DSN"), pool("WMS_HARDENED_API_DSN"), pool("WMS_HARDENED_WORKER_DSN")
	key, err := base64.StdEncoding.DecodeString(os.Getenv("WMS_HARDENED_KEY"))
	if err != nil {
		t.Fatal("invalid synthetic test key")
	}
	var f map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("WMS_HARDENED_FIXTURE")), &f); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	store, err := postgres.NewHardened(t.Context(), runtime, key)
	if err != nil {
		t.Fatal(err)
	}
	var _ api.Backend = store
	worker, err := postgres.NewHardenedWorker(t.Context(), workerPool)
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := modules.NewRegistry(nil, nil, nil)
	h, err := api.New(api.Config{Backend: store, Auth: hardenedAuth{subject: "actor"}, Modules: registry})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, body any, command string) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		r.Header.Set("Authorization", "Bearer verified-by-test-authenticator")
		r.Header.Set("Content-Type", "application/json")
		if command != "" {
			r.Header.Set("Idempotency-Key", command)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	t.Run("startup rejects key mismatch legacy and role confusion", func(t *testing.T) {
		badKey := append([]byte(nil), key...)
		badKey[0] ^= 1
		if _, err := postgres.NewHardened(t.Context(), runtime, badKey); err == nil {
			t.Fatal("wrong key accepted")
		}
		if _, err := postgres.New(t.Context(), runtime); err == nil {
			t.Fatal("legacy startup accepted hardened login")
		}
		if _, err := postgres.NewHardenedWorker(t.Context(), runtime); err == nil {
			t.Fatal("API login accepted for worker")
		}
		if _, err := postgres.NewHardened(t.Context(), workerPool, key); err == nil {
			t.Fatal("worker accepted for API")
		}
		exec("GRANT USAGE ON SCHEMA contracts_v1 TO " + store.RuntimeLogin())
		if _, err := postgres.NewHardened(t.Context(), runtime, key); err == nil {
			t.Fatal("legacy grants accepted on startup")
		}
		exec("REVOKE USAGE ON SCHEMA contracts_v1 FROM " + store.RuntimeLogin())
		group := "white_group_" + strings.ReplaceAll(id(), "-", "")
		exec("CREATE ROLE " + group + " NOLOGIN")
		exec("GRANT " + group + " TO " + store.RuntimeLogin())
		if _, err := postgres.NewHardened(t.Context(), runtime, key); err == nil {
			t.Fatal("role membership accepted on startup")
		}
		exec("REVOKE " + group + " FROM " + store.RuntimeLogin())
		exec("DROP ROLE " + group)
	})
	scopeQuery := "?organization_id=" + f["org"] + "&warehouse_id=" + f["wh"]
	t.Run("HTTP reads and foreign warehouse denial", func(t *testing.T) {
		for _, resource := range []string{"documents", "stock-balances", "reservations", "movements"} {
			w := request("GET", "/api/v1/"+resource+scopeQuery, nil, "")
			if w.Code != 200 || strings.Contains(w.Body.String(), f["other_document"]) {
				t.Fatalf("%s read failed: %d %s", resource, w.Code, w.Body)
			}
		}
		w := request("GET", "/api/v1/documents/"+f["other_document"]+scopeQuery, nil, "")
		if w.Code != 404 {
			t.Fatalf("foreign document read status %d", w.Code)
		}
		w = request("GET", "/api/v1/documents?organization_id="+f["other_org"]+"&warehouse_id="+f["other_wh"], nil, "")
		if w.Code != 403 {
			t.Fatalf("foreign scope status %d", w.Code)
		}
	})
	var reservation string
	t.Run("HTTP reserve replay and operation retrieval", func(t *testing.T) {
		command := id()
		body := map[string]any{"organization_id": f["org"], "warehouse_id": f["wh"], "document_line_id": f["line"], "expected_document_version": 1, "balance_id": f["balance"], "quantity": "2"}
		first := request("POST", "/api/v1/reservations", body, command)
		if first.Code != 200 && first.Code != 201 {
			t.Fatalf("reserve: %d %s", first.Code, first.Body)
		}
		result := data(t, first)
		reservation, _ = result["reservation_id"].(string)
		replayed := request("POST", "/api/v1/reservations", body, command)
		if replayed.Code != first.Code || replayed.Body.String() != first.Body.String() {
			t.Fatalf("replay mismatch: %d %s", replayed.Code, replayed.Body)
		}
		w := request("GET", "/api/v1/operations/"+command+scopeQuery, nil, "")
		if w.Code != 200 {
			t.Fatalf("operation retrieval: %d %s", w.Code, w.Body)
		}
	})
	t.Run("HTTP release", func(t *testing.T) {
		if reservation == "" {
			t.Fatal("reserve prerequisite failed")
		}
		w := request("POST", "/api/v1/reservations/"+reservation+"/releases", map[string]any{"organization_id": f["org"], "warehouse_id": f["wh"], "expected_document_version": 2, "quantity": "2", "reason": "test"}, id())
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("release: %d %s", w.Code, w.Body)
		}
	})
	t.Run("HTTP create activate receipt and movement", func(t *testing.T) {
		w := request("POST", "/api/v1/documents", map[string]any{"organization_id": f["org"], "warehouse_id": f["wh"], "owner_id": f["owner"], "kind": "receipt", "number": "hardened-receipt", "lines": []any{map[string]any{"item_id": f["item"], "quantity": "1"}}}, id())
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("create: %d %s", w.Code, w.Body)
		}
		d := data(t, w)
		document := d["document_id"].(string)
		line := d["lines"].([]any)[0].(map[string]any)["id"].(string)
		w = request("POST", "/api/v1/documents/"+document+"/transitions", map[string]any{"organization_id": f["org"], "warehouse_id": f["wh"], "expected_document_version": 1, "state": "active"}, id())
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("activate: %d %s", w.Code, w.Body)
		}
		w = request("POST", "/api/v1/movements", map[string]any{"organization_id": f["org"], "warehouse_id": f["wh"], "document_line_id": line, "expected_document_version": 2, "kind": "receipt", "target_balance_id": f["balance"], "quantity": "1"}, id())
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("movement: %d %s", w.Code, w.Body)
		}
	})
	t.Run("SQL checks current grants after principal resolution", func(t *testing.T) {
		p, err := store.ResolvePrincipal(t.Context(), auth.Identity{Issuer: "test", Subject: "actor"})
		if err != nil {
			t.Fatal(err)
		}
		exec(`DELETE FROM platform.actor_grants WHERE actor_id=$1 AND permission='reservation.create'`, f["actor"])
		_, err = store.Execute(t.Context(), p, inventory.Command{ID: id(), Kind: "reserve", OrganizationID: f["org"], WarehouseID: f["wh"], DocumentLineID: f["line"], ExpectedDocumentVersion: 3, BalanceID: f["balance"], Quantity: "1"}, registry)
		var problem *inventory.Problem
		if !errors.As(err, &problem) || problem.Status != 403 {
			t.Fatalf("revoked grant accepted: %v", err)
		}
		exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'reservation.create')`, f["actor"], f["org"], f["wh"], f["owner"])
	})
	t.Run("worker services installation and stale ack fenced", func(t *testing.T) {
		foreignOp, foreignEvent := id(), id()
		exec(`INSERT INTO platform.operations(id,actor_id,organization_id,warehouse_id,operation_type,request,result,completed_at) VALUES($1,$2,$3,$4,'foreign_fixture','{}','{}',clock_timestamp())`, foreignOp, f["actor"], f["other_org"], f["other_wh"])
		exec(`INSERT INTO platform.outbox(id,operation_id,event_type,aggregate_id,payload) VALUES($1,$2,'fixture',$3,'{"foreign":true}')`, foreignEvent, foreignOp, f["other_document"])
		events, err := worker.ClaimEvents(t.Context(), 100)
		if err != nil || len(events) == 0 {
			t.Fatalf("claim: %d %v", len(events), err)
		}
		seenOtherWarehouse := false
		for _, event := range events {
			if event.ID == foreignEvent {
				seenOtherWarehouse = true
			}
			if err := worker.CompleteEvent(t.Context(), event, true); err != nil {
				t.Fatal(err)
			}
			if err := worker.CompleteEvent(t.Context(), event, true); err == nil {
				t.Fatal("stale ack accepted")
			}
		}
		if !seenOtherWarehouse {
			t.Fatal("worker did not service another warehouse in this installation")
		}
		state, err := worker.QueueState(t.Context())
		if err != nil || state.Pending != 0 {
			t.Fatalf("installation queue did not drain: %+v %v", state, err)
		}
	})
	t.Run("concurrent replay commits one reservation", func(t *testing.T) {
		p, err := store.ResolvePrincipal(t.Context(), auth.Identity{Issuer: "test", Subject: "actor"})
		if err != nil {
			t.Fatal(err)
		}
		command := inventory.Command{ID: id(), Kind: "reserve", OrganizationID: f["org"], WarehouseID: f["wh"], DocumentLineID: f["line"], ExpectedDocumentVersion: 3, BalanceID: f["balance"], Quantity: "1"}
		expiry := time.Now().Add(time.Hour)
		command.ExpiresAt = &expiry
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for range 2 {
			wg.Go(func() { _, err := store.Execute(t.Context(), p, command, registry); results <- err })
		}
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM inventory.reservation_events WHERE operation_id=$1`, command.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("replay count=%d err=%v", count, err)
		}
	})
	t.Run("expiry and acknowledgement service all installation warehouses", func(t *testing.T) {
		foreignItem, foreignOwner, foreignLocation, foreignPosition, foreignBalance, foreignLine := id(), id(), id(), id(), id(), id()
		exec(`SELECT set_config('application_name','white-worker-scope-test',false)`)
		exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, foreignOwner, f["other_org"])
		exec(`INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES($1,$2,$1::uuid::text,'ea',0,'none')`, foreignItem, f["other_org"])
		exec(`INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES($1,$2,$3,$1::uuid::text,'storage')`, foreignLocation, f["other_org"], f["other_wh"])
		exec(`INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES($1,$2,$3,$4)`, foreignPosition, f["other_org"], f["other_wh"], foreignLocation)
		exec(`INSERT INTO inventory.stock_balances(id,organization_id,warehouse_id,owner_id,item_id,position_id,stock_state,on_hand) VALUES($1,$2,$3,$4,$5,$6,'available',10)`, foreignBalance, f["other_org"], f["other_wh"], foreignOwner, foreignItem, foreignPosition)
		// The foreign fixture document has another owner, so make a fresh shipment.
		foreignDocument := id()
		exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES($1,$2,$3,$4,'shipment',$1::uuid::text,'active')`, foreignDocument, f["other_org"], f["other_wh"], foreignOwner)
		exec(`INSERT INTO warehouse.document_lines(id,document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty) VALUES($1,$2,$3,$4,$5,$6,1,10)`, foreignLine, foreignDocument, f["other_org"], f["other_wh"], foreignOwner, foreignItem)
		exec(`SELECT inventory.reserve_stock($1,$2,$3,1,$4,1,clock_timestamp()+interval '1 hour')`, id(), f["actor"], foreignLine, foreignBalance)
		exec(`UPDATE inventory.reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE balance_id IN($1,$2) AND active_qty>0`, f["balance"], foreignBalance)
		count, err := worker.ExpireReservations(t.Context())
		if err != nil || count < 2 {
			t.Fatalf("installation expiry count=%d err=%v", count, err)
		}
		var foreignActive string
		if err := admin.QueryRow(t.Context(), `SELECT active_qty::text FROM inventory.reservations WHERE document_line_id=$1`, foreignLine).Scan(&foreignActive); err != nil || foreignActive != "0.000000" {
			t.Fatalf("other warehouse reservation did not expire: %s %v", foreignActive, err)
		}
		events, err := worker.ClaimEvents(t.Context(), 100)
		if err != nil {
			t.Fatal(err)
		}
		seenOtherOrganization := false
		for _, event := range events {
			var organization string
			if err := admin.QueryRow(t.Context(), `SELECT op.organization_id::text FROM platform.outbox o JOIN platform.operations op ON op.id=o.operation_id WHERE o.id=$1`, event.ID).Scan(&organization); err != nil {
				t.Fatal(err)
			}
			if organization == f["other_org"] {
				seenOtherOrganization = true
			}
			if err := worker.CompleteEvent(t.Context(), event, true); err != nil {
				t.Fatalf("same-installation acknowledgement denied: %v", err)
			}
		}
		if !seenOtherOrganization {
			t.Fatal("installation worker did not acknowledge another organization")
		}
	})
	t.Run("unknown COMMIT outcome retries with one document receipt", func(t *testing.T) {
		p, err := store.ResolvePrincipal(t.Context(), auth.Identity{Issuer: "test", Subject: "actor"})
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"before_send", "after_ack_eof", "after_ack_generic"} {
			t.Run(mode, func(t *testing.T) {
				cfg, err := pgxpool.ParseConfig(os.Getenv("WMS_HARDENED_API_DSN"))
				if err != nil {
					t.Fatal(err)
				}
				var dropped atomic.Bool
				dropped.Store(true)
				cfg.MaxConns = 1
				cfg.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
					conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
					if err != nil {
						return nil, err
					}
					return &documentCommitFault{Conn: conn, mode: mode, dropped: &dropped}, nil
				}
				faultPool, err := pgxpool.NewWithConfig(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(faultPool.Close)
				faultStore, err := postgres.NewHardened(t.Context(), faultPool, key)
				if err != nil {
					t.Fatal(err)
				}
				command := id()
				input := inventory.DocumentInput{OrganizationID: f["org"], WarehouseID: f["wh"], OwnerID: f["owner"], Kind: "receipt", Number: command, Lines: []inventory.DocumentLineInput{{ItemID: f["item"], Quantity: "1"}}}
				dropped.Store(false)
				_, err = faultStore.CommandDocument(t.Context(), p, command, "", input, registry)
				var problem *inventory.Problem
				if !dropped.Load() || !errors.As(err, &problem) || problem.Code != "operation_interrupted" {
					t.Fatalf("COMMIT fault result: %v", err)
				}
				var count int
				want := 1
				if mode == "before_send" {
					want = 0
				}
				if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM platform.operations WHERE id=$1`, command).Scan(&count); err != nil || count != want {
					t.Fatalf("commit state before retry %d want %d err %v", count, want, err)
				}
				for range 2 {
					if _, err := store.CommandDocument(t.Context(), p, command, "", input, registry); err != nil {
						t.Fatal(err)
					}
				}
				if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM warehouse.documents WHERE number=$1`, command).Scan(&count); err != nil || count != 1 {
					t.Fatalf("retry documents %d err %v", count, err)
				}
			})
		}
	})
	t.Run("module metadata verifies projection without runtime fact access", func(t *testing.T) {
		var owner string
		if err := admin.QueryRow(t.Context(), `SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='hardened_private'`).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		exec(`CREATE SCHEMA mod_hardened_check;
		 CREATE VIEW mod_hardened_check.document_facts_v1 AS SELECT NULL::uuid organization_id,NULL::uuid warehouse_id,NULL::uuid document_id,1::bigint content_revision,'{}'::jsonb payload WHERE false`)
		exec(`GRANT USAGE ON SCHEMA mod_hardened_check TO ` + owner)
		exec(`GRANT SELECT ON mod_hardened_check.document_facts_v1 TO ` + owner)
		m := modules.Manifest{ManifestVersion: 1, ID: "hardened.check", Version: "1.0.0", Runtime: "declarative_v2", Compatibility: modules.Compatibility{Core: ">=0.5.0 <1.0.0", SDK: ">=2.0.0 <3.0.0"}, Data: modules.ManifestData{Schema: "mod_hardened_check", TargetSchemaVersion: 1}, Guards: []modules.GuardSpec{{Point: modules.ReservationPoint, Handler: "Allow", Required: true, FailurePolicy: "deny", TimeoutMS: 100}}, Policies: []json.RawMessage{json.RawMessage(`{"handler":"Allow","any":[{"conditions":[{"field":"decision","op":"eq","value":"allow"}]}]}`)}}
		raw, _ := json.Marshal(m)
		exec(`INSERT INTO platform.modules(id,version,schema_name,state,manifest) VALUES($1,$2,$3,'active',$4)`, m.ID, m.Version, m.Data.Schema, raw)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = admin.Exec(ctx, `DELETE FROM platform.modules WHERE id='hardened.check'; DROP SCHEMA mod_hardened_check CASCADE`)
		})
		installed, err := store.InstalledModules(t.Context())
		if err != nil || len(installed) != 1 {
			t.Fatalf("metadata unavailable: %d %v", len(installed), err)
		}
		r, err := modules.NewRegistry(installed, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.ValidateModules(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Exec(t.Context(), `SELECT * FROM mod_hardened_check.document_facts_v1`); err == nil {
			t.Fatal("runtime can read module facts")
		}
		if _, err := postgres.NewHardened(t.Context(), runtime, key); err != nil {
			t.Fatalf("module metadata requires unscoped fact access: %v", err)
		}
		exec(`DROP VIEW mod_hardened_check.document_facts_v1`)
		if err := store.ValidateModules(t.Context(), r); err == nil {
			t.Fatal("missing required projection accepted")
		}
	})
}
