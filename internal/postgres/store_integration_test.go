//go:build integration

package postgres_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/api"
	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/clientmodules/dispatchquality"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func id() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type tokenAuth struct{ subject string }

func (a tokenAuth) Authenticate(_ context.Context, token string) (auth.Identity, error) {
	if token != "valid" {
		return auth.Identity{}, errors.New("unauthenticated")
	}
	return auth.Identity{Issuer: "https://tests.invalid", Subject: a.subject}, nil
}

type fixture struct {
	t                                                                                testing.TB
	admin, runtime                                                                   *pgxpool.Pool
	store                                                                            *postgres.Store
	org, wh, owner, otherOwner, actor, item, source, target, foreign, document, line string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	adminDSN, runtimeDSN := os.Getenv("WMS_TEST_ADMIN_DSN"), os.Getenv("WMS_TEST_RUNTIME_DSN")
	if adminDSN == "" || runtimeDSN == "" {
		t.Skip("integration PostgreSQL DSNs not configured")
	}
	if err := validateTestDatabaseDSNs(adminDSN, runtimeDSN); err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.New(t.Context(), adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	runtime, err := pgxpool.New(t.Context(), runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Close)
	store, err := postgres.New(t.Context(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, admin: admin, runtime: runtime, store: store, org: id(), wh: id(), owner: id(), otherOwner: id(), actor: id(), item: id(), document: id(), line: id()}
	f.exec(`INSERT INTO platform.organizations(id,code,name) VALUES($1,$1::uuid::text,'Go API test')`, f.org)
	f.exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'https://tests.invalid',$1::uuid::text)`, f.actor)
	f.exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$3,$1::uuid::text),($2,$3,$2::uuid::text)`, f.owner, f.otherOwner, f.org)
	f.exec(`INSERT INTO catalog.units(code,dimension) VALUES('ea','count') ON CONFLICT DO NOTHING`)
	f.exec(`INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES($1,$2,$1::uuid::text,'ea',3,'none')`, f.item, f.org)
	f.exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, f.wh, f.org)
	positions := []string{id(), id(), id()}
	for _, p := range positions {
		l := id()
		f.exec(`INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES($1,$2,$3,$1::uuid::text,'storage')`, l, f.org, f.wh)
		f.exec(`INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES($1,$2,$3,$4)`, p, f.org, f.wh, l)
	}
	for i, ptr := range [](*string){&f.source, &f.target, &f.foreign} {
		owner := f.owner
		if i == 2 {
			owner = f.otherOwner
		}
		if err := admin.QueryRow(t.Context(), `SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available')`, f.org, f.wh, owner, f.item, positions[i]).Scan(ptr); err != nil {
			t.Fatal(err)
		}
	}
	rd, rl := f.doc("receipt", "10", f.owner)
	if _, err := admin.Exec(t.Context(), `SELECT inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,10)`, id(), f.actor, rl, f.source); err != nil {
		t.Fatal(err)
	}
	_ = rd
	f.exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES($1,$2,$3,$4,'shipment',$1::uuid::text,'active')`, f.document, f.org, f.wh, f.owner)
	f.exec(`INSERT INTO warehouse.document_lines(id,document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty) VALUES($1,$2,$3,$4,$5,$6,1,10)`, f.line, f.document, f.org, f.wh, f.owner, f.item)
	for _, p := range []string{"stock.read", "reservation.read", "reservation.create", "reservation.release", "movement.read", "movement.create", "operation.read"} {
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, f.actor, f.org, f.wh, f.owner, p)
	}
	return f
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.admin.Exec(f.t.Context(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) doc(kind, qty, owner string) (string, string) {
	f.t.Helper()
	d, l := id(), id()
	f.exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES($1,$2,$3,$4,$5,$1::uuid::text,'active')`, d, f.org, f.wh, owner, kind)
	f.exec(`INSERT INTO warehouse.document_lines(id,document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty) VALUES($1,$2,$3,$4,$5,$6,1,$7::numeric)`, l, d, f.org, f.wh, owner, f.item, qty)
	return d, l
}
func (f *fixture) scalar(sql string, args ...any) string {
	f.t.Helper()
	var v string
	if err := f.admin.QueryRow(f.t.Context(), sql, args...).Scan(&v); err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *fixture) handler(manifests []modules.Manifest, code []modules.Module) http.Handler {
	f.t.Helper()
	registry, err := modules.NewRegistry(manifests, code, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	h, err := api.New(api.Config{Backend: f.store, Auth: tokenAuth{subject: f.actor}, Modules: registry})
	if err != nil {
		f.t.Fatal(err)
	}
	return h
}
func (f *fixture) request(h http.Handler, method, path string, body map[string]any, command string) *httptest.ResponseRecorder {
	f.t.Helper()
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, strings.NewReader(string(data)))
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Content-Type", "application/json")
	if command != "" {
		r.Header.Set("Idempotency-Key", command)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func (f *fixture) scopePath(resource string) string {
	return "/api/v1/" + resource + "?organization_id=" + f.org + "&warehouse_id=" + f.wh
}
func (f *fixture) reserveBody(line, qty string, version int) map[string]any {
	return map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "document_line_id": line, "expected_document_version": version, "balance_id": f.source, "quantity": qty}
}
func (f *fixture) movementBody(line, kind, qty string, version int, reservation string) map[string]any {
	body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "document_line_id": line, "expected_document_version": version, "kind": kind, "source_balance_id": f.source, "quantity": qty}
	if kind == "transfer" {
		body["target_balance_id"] = f.target
	}
	if reservation != "" {
		body["reservation_id"] = reservation
	}
	return body
}
func data(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Data
}
func requireStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body)
	}
}

func TestStoreRejectsPrivilegedApplicationConnection(t *testing.T) {
	f := newFixture(t)
	if _, err := postgres.New(t.Context(), f.admin); err == nil {
		t.Fatal("privileged application pool accepted")
	}
}

func TestStockAPIIsScopedAndPaginated(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	w := f.request(h, "GET", f.scopePath("stock-balances")+"&limit=1", nil, "")
	requireStatus(t, w, 200)
	var page struct {
		Data       []map[string]any `json:"data"`
		Pagination struct {
			NextCursor string `json:"next_cursor"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 || page.Pagination.NextCursor == "" {
		t.Fatalf("page=%s", w.Body)
	}
	if _, ok := page.Data[0]["on_hand"].(string); !ok {
		t.Fatal("quantity was not returned as a decimal string")
	}
	w = f.request(h, "GET", f.scopePath("stock-balances")+"&limit=1&cursor="+page.Pagination.NextCursor, nil, "")
	requireStatus(t, w, 200)
	if strings.Contains(w.Body.String(), f.foreign) {
		t.Fatal("other owner's stock leaked")
	}
	w = f.request(h, "GET", f.scopePath("stock-balances")+"&owner_id="+f.otherOwner, nil, "")
	requireStatus(t, w, 403)
	w = f.request(h, "GET", f.scopePath("stock-balances")+"&item_id="+id()+"&cursor="+page.Pagination.NextCursor, nil, "")
	requireStatus(t, w, 400)
	w = f.request(h, "GET", f.scopePath("stock-balances/"+f.foreign), nil, "")
	requireStatus(t, w, 404)
}

func TestReservationAndReleaseAPI(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	c := id()
	body := f.reserveBody(f.line, "4", 1)
	a := f.request(h, "POST", "/api/v1/reservations", body, c)
	requireStatus(t, a, 201)
	r := data(t, a)["reservation_id"].(string)
	b := f.request(h, "POST", "/api/v1/reservations", body, c)
	requireStatus(t, b, 201)
	if a.Body.String() != b.Body.String() {
		t.Fatal("retry changed result")
	}
	w := f.request(h, "GET", f.scopePath("reservations/"+r), nil, "")
	requireStatus(t, w, 200)
	release := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 2, "quantity": "1", "reason": "cancelled remainder"}
	w = f.request(h, "POST", "/api/v1/reservations/"+r+"/releases", release, id())
	requireStatus(t, w, 201)
	if got := f.scalar(`SELECT reserved_qty::text FROM inventory.stock_balances WHERE id=$1`, f.source); got != "3.000000" {
		t.Fatalf("reserved=%s", got)
	}
	w = f.request(h, "GET", f.scopePath("reservations"), nil, "")
	requireStatus(t, w, 200)
	w = f.request(h, "GET", f.scopePath("operations/"+c), nil, "")
	requireStatus(t, w, 200)
}

func guarded(point string, g modules.Guard) ([]modules.Manifest, []modules.Module) {
	m := modules.Manifest{ManifestVersion: 1, ID: "test.required", Version: "1.0.0", Compatibility: modules.Compatibility{Core: ">=0.2.0 <1.0.0", SDK: ">=2.0.0 <3.0.0"}, Guards: []modules.GuardSpec{{Point: point, Handler: "Check", Required: true, FailurePolicy: "deny", TimeoutMS: 10}}}
	return []modules.Manifest{m}, []modules.Module{{ID: m.ID, Version: m.Version, Guards: map[string]modules.Guard{"Check": g}, GuardPoints: map[string][]string{"Check": {point}}}}
}

func TestMandatoryGuardsRollbackEveryMutation(t *testing.T) {
	for _, tc := range []struct {
		name, point, action string
		guard               modules.Guard
		status              int
	}{
		{"reserve denied", modules.ReservationPoint, "reserve", func(context.Context, modules.Check) error { return &modules.Denial{Code: "test.required.denied"} }, 409},
		{"release denied", modules.ReleasePoint, "release", func(context.Context, modules.Check) error { return &modules.Denial{Code: "test.required.denied"} }, 409},
		{"movement denied", modules.MovementPoint, "movement", func(context.Context, modules.Check) error { return &modules.Denial{Code: "test.required.denied"} }, 409},
		{"movement panic", modules.MovementPoint, "movement", func(context.Context, modules.Check) error { panic("private driver state") }, 503},
		{"movement timeout", modules.MovementPoint, "movement", func(ctx context.Context, _ modules.Check) error { <-ctx.Done(); return ctx.Err() }, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			manifests, code := guarded(tc.point, tc.guard)
			h := f.handler(manifests, code)
			c := id()
			path := "/api/v1/reservations"
			body := f.reserveBody(f.line, "2", 1)
			if tc.action == "movement" {
				_, line := f.doc("transfer", "2", f.owner)
				path = "/api/v1/movements"
				body = f.movementBody(line, "transfer", "2", 1, "")
			}
			if tc.action == "release" {
				seed := f.request(f.handler(nil, nil), "POST", "/api/v1/reservations", body, id())
				requireStatus(t, seed, 201)
				r := data(t, seed)["reservation_id"].(string)
				path = "/api/v1/reservations/" + r + "/releases"
				body = map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 2, "quantity": "1", "reason": "test"}
			}
			before := f.scalar(`SELECT json_build_array(on_hand,reserved_qty)::text FROM inventory.stock_balances WHERE id=$1`, f.source)
			w := f.request(h, "POST", path, body, c)
			requireStatus(t, w, tc.status)
			if strings.Contains(w.Body.String(), "private driver") {
				t.Fatal("panic leaked")
			}
			if after := f.scalar(`SELECT json_build_array(on_hand,reserved_qty)::text FROM inventory.stock_balances WHERE id=$1`, f.source); after != before {
				t.Fatalf("balance changed: %s -> %s", before, after)
			}
			if got := f.scalar(`SELECT count(*)::text FROM platform.operations WHERE id=$1`, c); got != "0" {
				t.Fatal("rejected operation committed")
			}
			if got := f.scalar(`SELECT count(*)::text FROM platform.outbox WHERE operation_id=$1`, c); got != "0" {
				t.Fatal("rejected event committed")
			}
		})
	}
}

func TestCompletedRetryDoesNotReevaluateGuard(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	manifests, code := guarded(modules.ReservationPoint, func(context.Context, modules.Check) error {
		if calls.Add(1) > 1 {
			return &modules.Denial{Code: "test.required.later_denied"}
		}
		return nil
	})
	h := f.handler(manifests, code)
	c := id()
	body := f.reserveBody(f.line, "3", 1)
	a := f.request(h, "POST", "/api/v1/reservations", body, c)
	requireStatus(t, a, 201)
	b := f.request(h, "POST", "/api/v1/reservations", body, c)
	requireStatus(t, b, 201)
	if calls.Load() != 1 || a.Body.String() != b.Body.String() {
		t.Fatalf("calls=%d results differ=%v", calls.Load(), a.Body.String() != b.Body.String())
	}
}

func TestConcurrentAPICommandsDoNotOversell(t *testing.T) {
	f := newFixture(t)
	_, line2 := f.doc("shipment", "7", f.owner)
	h := f.handler(nil, nil)
	responses := make(chan *httptest.ResponseRecorder, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, line := range []string{f.line, line2} {
		wg.Go(func() {
			<-start
			responses <- f.request(h, "POST", "/api/v1/reservations", f.reserveBody(line, "7", 1), id())
		})
	}
	close(start)
	wg.Wait()
	close(responses)
	counts := map[int]int{}
	for w := range responses {
		counts[w.Code]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		t.Fatalf("statuses=%v", counts)
	}
	if got := f.scalar(`SELECT reserved_qty::text FROM inventory.stock_balances WHERE id=$1`, f.source); got != "7.000000" {
		t.Fatalf("reserved=%s", got)
	}
}

func TestConcurrentIdenticalAPICommandRunsGuardOnce(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	manifests, code := guarded(modules.ReservationPoint, func(context.Context, modules.Check) error { calls.Add(1); return nil })
	h := f.handler(manifests, code)
	c := id()
	responses := make(chan *httptest.ResponseRecorder, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			responses <- f.request(h, "POST", "/api/v1/reservations", f.reserveBody(f.line, "4", 1), c)
		})
	}
	close(start)
	wg.Wait()
	close(responses)
	var bodies []string
	for w := range responses {
		requireStatus(t, w, 201)
		bodies = append(bodies, w.Body.String())
	}
	if calls.Load() != 1 || bodies[0] != bodies[1] {
		t.Fatalf("calls=%d bodies=%v", calls.Load(), bodies)
	}
}

func TestDispatchQualityModuleUsesTransactionalFacts(t *testing.T) {
	f := newFixture(t)
	base := f.handler(nil, nil)
	seed := f.request(base, "POST", "/api/v1/reservations", f.reserveBody(f.line, "4", 1), id())
	requireStatus(t, seed, 201)
	r := data(t, seed)["reservation_id"].(string)
	module := dispatchquality.New()
	h := f.handler([]modules.Manifest{dispatchquality.Manifest()}, []modules.Module{module})
	body := f.movementBody(f.line, "issue", "2", 2, r)
	w := f.request(h, "POST", "/api/v1/movements", body, id())
	requireStatus(t, w, 409)
	f.exec(`INSERT INTO mod_example_dispatch_quality.checks(organization_id,warehouse_id,document_id,checked_content_revision,policy_revision,decision,reviewer_id,evidence_reference,expires_at) VALUES($1,$2,$3,2,1,'approved',$4,'verified-test-evidence',clock_timestamp()+interval '1 hour')`, f.org, f.wh, f.document, f.actor)
	c := id()
	w = f.request(h, "POST", "/api/v1/movements", body, c)
	requireStatus(t, w, 201)
	movementID := data(t, w)["movement_id"].(string)
	read := f.request(h, "GET", f.scopePath("movements/"+movementID), nil, "")
	requireStatus(t, read, 200)
	list := f.request(h, "GET", f.scopePath("movements"), nil, "")
	requireStatus(t, list, 200)
	// Posting changes content_revision. A new command must not reuse the old approval.
	body["expected_document_version"] = 3
	denied := f.request(h, "POST", "/api/v1/movements", body, id())
	requireStatus(t, denied, 409)
	body["expected_document_version"] = 2
	replay := f.request(h, "POST", "/api/v1/movements", body, c)
	requireStatus(t, replay, 201)
	if replay.Body.String() != w.Body.String() {
		t.Fatal("completed movement retry changed")
	}
}

func TestRuntimeModuleInstallationLoading(t *testing.T) {
	f := newFixture(t)
	manifests, code := guarded(modules.MovementPoint, func(context.Context, modules.Check) error { return nil })
	m := manifests[0]
	m.Data = modules.ManifestData{Schema: "mod_test_required", TargetSchemaVersion: 1}
	code[0].SchemaVersion = 1
	raw, _ := json.Marshal(m)
	f.exec(`INSERT INTO platform.modules(id,version,schema_name,state,manifest) VALUES($1,$2,'mod_test_required','active',$3::jsonb)`, m.ID, m.Version, string(raw))
	t.Cleanup(func() { _, _ = f.admin.Exec(context.Background(), `DELETE FROM platform.modules WHERE id=$1`, m.ID) })
	loaded, err := f.store.InstalledModules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := modules.NewRegistry(loaded, code, []string{m.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := modules.NewRegistry(loaded, nil, []string{m.ID}); err == nil {
		t.Fatal("active module without compiled handlers accepted")
	}
}

func TestDatabaseBusinessErrorsUseSafeHTTPProblems(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	for _, tc := range []struct {
		name, qty       string
		version, status int
	}{
		{"insufficient stock", "11", 1, 409}, {"item precision", "0.0001", 1, 400}, {"stale version", "1", 99, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := f.request(h, "POST", "/api/v1/reservations", f.reserveBody(f.line, tc.qty, tc.version), id())
			requireStatus(t, w, tc.status)
			if strings.Contains(w.Body.String(), "PL/pgSQL") || strings.Contains(w.Body.String(), "SELECT ") {
				t.Fatal("SQL details leaked")
			}
		})
	}
}

func TestRequestCancellationLeavesNoPreparedCommand(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{})
	manifests, code := guarded(modules.ReservationPoint, func(ctx context.Context, _ modules.Check) error { close(started); <-ctx.Done(); return ctx.Err() })
	manifests[0].Guards[0].TimeoutMS = 1000
	h := f.handler(manifests, code)
	c := id()
	raw, _ := json.Marshal(f.reserveBody(f.line, "2", 1))
	ctx, cancel := context.WithCancel(t.Context())
	r := httptest.NewRequest("POST", "/api/v1/reservations", strings.NewReader(string(raw))).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", c)
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), r) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("guard did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled request did not finish")
	}
	if got := f.scalar(`SELECT count(*)::text FROM platform.operations WHERE id=$1`, c); got != "0" {
		t.Fatal("canceled operation committed")
	}
}

func TestChangedCommandPayloadConflicts(t *testing.T) {
	f := newFixture(t)
	h := f.handler(nil, nil)
	c := id()
	body := f.reserveBody(f.line, "2", 1)
	requireStatus(t, f.request(h, "POST", "/api/v1/reservations", body, c), 201)
	body["quantity"] = "1"
	requireStatus(t, f.request(h, "POST", "/api/v1/reservations", body, c), 409)
}

func TestAmbiguousModuleFactsFailClosed(t *testing.T) {
	f := newFixture(t)
	f.exec(`CREATE SCHEMA mod_test_ambiguous`)
	f.exec(`CREATE VIEW mod_test_ambiguous.document_facts_v1 AS
      SELECT d.organization_id,d.warehouse_id,d.id AS document_id,d.content_revision,
        jsonb_build_object('approved',true) AS payload
      FROM warehouse.documents d CROSS JOIN generate_series(1,2)`)
	f.exec(`GRANT USAGE ON SCHEMA mod_test_ambiguous TO wms_test_runtime; GRANT SELECT ON mod_test_ambiguous.document_facts_v1 TO wms_test_runtime`)
	t.Cleanup(func() {
		_, _ = f.admin.Exec(context.Background(), `DROP VIEW mod_test_ambiguous.document_facts_v1; DROP SCHEMA mod_test_ambiguous`)
	})
	m := modules.Manifest{ManifestVersion: 1, ID: "test.ambiguous", Version: "1.0.0", Compatibility: modules.Compatibility{Core: ">=0.2.0 <1.0.0", SDK: ">=2.0.0 <3.0.0"}, Guards: []modules.GuardSpec{{Point: modules.ReservationPoint, Handler: "Check", Required: true, FailurePolicy: "deny", TimeoutMS: 100}}}
	code := modules.Module{ID: m.ID, Version: m.Version, FactView: "mod_test_ambiguous.document_facts_v1", Guards: map[string]modules.Guard{"Check": func(context.Context, modules.Check) error { return nil }}, GuardPoints: map[string][]string{"Check": {modules.ReservationPoint}}}
	h := f.handler([]modules.Manifest{m}, []modules.Module{code})
	w := f.request(h, "POST", "/api/v1/reservations", f.reserveBody(f.line, "1", 1), id())
	requireStatus(t, w, 503)
	if value := f.scalar(`SELECT reserved_qty::text FROM inventory.stock_balances WHERE id=$1`, f.source); value != "0.000000" {
		t.Fatalf("reserved=%s", value)
	}
}
