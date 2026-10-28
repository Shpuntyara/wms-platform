//go:build integration && hardened

package postgres_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/api"
	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/jobs"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type onPremCriticAuth struct{ subject string }

func TestHardenedOnPremCriticWorkerRejectsInstallationDrift(t *testing.T) {
	admin := onPremCriticPool(t, "WMS_HARDENED_ADMIN_DSN")
	workerPool := onPremCriticPool(t, "WMS_HARDENED_WORKER_DSN")
	worker, err := postgres.NewHardenedWorker(t.Context(), workerPool)
	if err != nil {
		t.Fatal("cannot construct worker from disposable fixture")
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), query, args...); err != nil {
			if databaseError, ok := errors.AsType[*pgconn.PgError](err); ok {
				t.Fatalf("independent worker fixture SQL failed: %s (%s)", databaseError.Message, databaseError.Code)
			}
			t.Fatal("independent worker fixture SQL failed")
		}
	}
	var org, wh, operation, readyID, leasedID, token string
	if err := admin.QueryRow(t.Context(), `SELECT gen_random_uuid()::text,gen_random_uuid()::text,
	 gen_random_uuid()::text,gen_random_uuid()::text,gen_random_uuid()::text,gen_random_uuid()::text`).Scan(&org, &wh, &operation, &readyID, &leasedID, &token); err != nil {
		t.Fatal("cannot allocate independent worker fixture IDs")
	}
	exec(`INSERT INTO platform.organizations(id,code,name) VALUES($1,$2,'worker identity critic')`, org, org)
	exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$3)`, wh, org, wh)
	exec(`INSERT INTO platform.operations(id,actor_id,organization_id,warehouse_id,operation_type,request,result,completed_at)
	 VALUES($1,'00000000-0000-4000-8000-000000000001',$2,$3,'critic_identity','{}','{}',clock_timestamp())`, operation, org, wh)
	exec(`INSERT INTO platform.outbox(id,operation_id,event_type,aggregate_id,payload)
	 VALUES($1,$2,'critic.ready',$1,'{}')`, readyID, operation)
	exec(`INSERT INTO platform.outbox(id,operation_id,event_type,aggregate_id,payload,lease_token,leased_until,attempts)
	 VALUES($1,$2,'critic.leased',$1,'{}',$3,clock_timestamp()+interval '5 minutes',1)`, leasedID, operation, token)
	t.Cleanup(func() {
		for _, query := range []string{
			`DELETE FROM platform.outbox WHERE operation_id=$1`,
			`DELETE FROM platform.operations WHERE id=$1`,
		} {
			if _, err := admin.Exec(context.Background(), query, operation); err != nil {
				t.Error("cannot clean worker outbox fixture")
			}
		}
		if _, err := admin.Exec(context.Background(), `DELETE FROM warehouse.warehouses WHERE organization_id=$1`, org); err != nil {
			t.Error("cannot clean worker warehouse fixture")
		}
		if _, err := admin.Exec(context.Background(), `DELETE FROM platform.organizations WHERE id=$1`, org); err != nil {
			t.Error("cannot clean worker organization fixture")
		}
	})
	snapshot := func() string {
		t.Helper()
		var state string
		if err := admin.QueryRow(t.Context(), `SELECT jsonb_build_array(
		 (SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY id),'[]') FROM platform.outbox o WHERE operation_id=$1),
		 (SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY id),'[]') FROM inventory.stock_balances s))::text`, operation).Scan(&state); err != nil {
			t.Fatal("cannot snapshot worker effects")
		}
		return state
	}
	if _, err := worker.QueueState(t.Context()); err != nil {
		t.Fatal("worker monitoring baseline must succeed")
	}
	before := snapshot()
	exec(`UPDATE hardened_private.installation SET id=gen_random_uuid() WHERE singleton`)
	restore := func() {
		exec(`UPDATE hardened_private.installation SET id=$1 WHERE singleton`, worker.InstallationID())
	}
	defer restore()
	if events, err := worker.ClaimEvents(t.Context(), 1); err == nil || len(events) != 0 {
		t.Error("worker claimed events after installation identity changed")
	}
	if count, err := worker.ExpireReservations(t.Context()); err == nil || count != 0 {
		t.Error("worker accepted expiry after installation identity changed")
	}
	if err := worker.CompleteEvent(t.Context(), jobs.Event{ID: leasedID, Token: token}, true); err == nil {
		t.Error("worker acknowledged a pre-existing lease after installation identity changed")
	}
	if _, err := worker.QueueState(t.Context()); err == nil {
		t.Error("worker monitoring reports healthy after installation identity changed")
	}
	if after := snapshot(); after != before {
		t.Error("worker changed owned outbox or stock rows after installation identity changed")
	}
	restore()
	if _, err := worker.QueueState(t.Context()); err != nil {
		t.Error("worker monitoring did not recover after identity restoration")
	}
}

func (a onPremCriticAuth) Authenticate(context.Context, string) (auth.Identity, error) {
	return auth.Identity{Issuer: "onprem-security-critic", Subject: a.subject}, nil
}

func TestHardenedOnPremCriticCrossOwnerAndStaleReadGrants(t *testing.T) {
	admin := onPremCriticPool(t, "WMS_HARDENED_ADMIN_DSN")
	runtime := onPremCriticPool(t, "WMS_HARDENED_API_DSN")
	key, err := base64.StdEncoding.DecodeString(os.Getenv("WMS_HARDENED_KEY"))
	if err != nil || len(key) != 32 {
		t.Fatal("a 32-byte synthetic signing key is required")
	}
	store, err := postgres.NewHardened(t.Context(), runtime, key)
	if err != nil {
		t.Fatal("cannot construct hardened store from disposable fixture")
	}
	id := func() string {
		t.Helper()
		var value string
		if err := admin.QueryRow(t.Context(), `SELECT gen_random_uuid()::text`).Scan(&value); err != nil {
			t.Fatal("cannot allocate independent critic fixture ID")
		}
		return value
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), query, args...); err != nil {
			t.Fatal("independent owner fixture SQL failed")
		}
	}
	org, wh, ownerA, ownerB, actor, ungrantedActor, documentA, documentB := id(), id(), id(), id(), id(), id(), id(), id()
	exec(`INSERT INTO platform.organizations(id,code,name) VALUES($1,$2,'independent owner critic')`, org, org)
	exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$3)`, wh, org, wh)
	for _, owner := range []string{ownerA, ownerB} {
		exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$3)`, owner, org, owner)
	}
	for _, who := range []string{actor, ungrantedActor} {
		exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'onprem-security-critic',$2)`, who, who)
	}
	for _, pair := range []struct{ document, owner string }{{documentA, ownerA}, {documentB, ownerB}} {
		exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state)
		 VALUES($1,$2,$3,$4,'shipment',$5,'draft')`, pair.document, org, wh, pair.owner, pair.document)
	}
	grantDocumentRead := func() {
		exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission)
		 VALUES($1,$2,$3,$4,'document.read')`, actor, org, wh, ownerA)
	}
	grantDocumentRead()
	// An unrelated grant survives document.read revocation. The stale-read test
	// must exercise the resource permission check, not just reject an empty actor.
	exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission)
	 VALUES($1,$2,$3,$4,'stock.read')`, actor, org, wh, ownerA)
	t.Cleanup(func() {
		for _, query := range []string{
			`DELETE FROM warehouse.documents WHERE organization_id=$1`,
			`DELETE FROM platform.actor_grants WHERE organization_id=$1`,
			`DELETE FROM catalog.stock_owners WHERE organization_id=$1`,
			`DELETE FROM warehouse.warehouses WHERE organization_id=$1`,
			`DELETE FROM platform.organizations WHERE id=$1`,
		} {
			if _, err := admin.Exec(context.Background(), query, org); err != nil {
				t.Error("cannot clean independent owner critic fixture")
			}
		}
		if _, err := admin.Exec(context.Background(), `DELETE FROM platform.actors WHERE id=$1 OR id=$2`, actor, ungrantedActor); err != nil {
			t.Error("cannot clean independent owner critic actors")
		}
	})
	principal, err := store.ResolvePrincipal(t.Context(), auth.Identity{Issuer: "onprem-security-critic", Subject: actor})
	if err != nil {
		t.Fatal("cannot resolve independent critic actor")
	}
	cachedScope, err := principal.Scope("document.read", org, wh)
	if err != nil {
		t.Fatal("cannot select independent critic read scope")
	}
	assertPage := func(t *testing.T, scope inventory.Scope, filters inventory.Filters, want ...string) {
		t.Helper()
		page, err := store.List(t.Context(), inventory.DocumentResource, scope, filters)
		if err != nil {
			t.Fatal("document listing unexpectedly failed")
		}
		if len(page.Data) != len(want) {
			t.Fatalf("document count = %d, want %d", len(page.Data), len(want))
		}
		for i, raw := range page.Data {
			var row struct{ ID string }
			if json.Unmarshal(raw, &row) != nil || row.ID != want[i] {
				t.Fatal("document listing returned a row outside current owner grants")
			}
		}
	}
	assertProblem := func(t *testing.T, err error, status int) {
		t.Helper()
		problem, ok := errors.AsType[*inventory.Problem](err)
		if !ok || problem.Status != status {
			t.Fatalf("expected inventory problem status %d", status)
		}
	}
	t.Run("current user cannot read another owner in the same warehouse", func(t *testing.T) {
		assertPage(t, cachedScope, inventory.Filters{}, documentA)
		_, err := store.Get(t.Context(), inventory.DocumentResource, cachedScope, documentB)
		assertProblem(t, err, http.StatusNotFound)
		registry, err := modules.NewRegistry(nil, nil, nil)
		if err != nil {
			t.Fatal("cannot create empty registry")
		}
		handler, err := api.New(api.Config{Backend: store, Auth: onPremCriticAuth{subject: actor}, Modules: registry})
		if err != nil {
			t.Fatal("cannot construct owner-scoped API")
		}
		for _, tc := range []struct {
			document string
			status   int
		}{{documentA, http.StatusOK}, {documentB, http.StatusNotFound}} {
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/documents/%s?organization_id=%s&warehouse_id=%s", tc.document, org, wh), nil)
			req.Header.Set("Authorization", "Bearer synthetic-verified-identity")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Errorf("same-warehouse document status = %d, want %d", rec.Code, tc.status)
			}
		}
	})
	t.Run("forged in-memory owner and actor scope cannot replace SQL grants", func(t *testing.T) {
		forgedScope := cachedScope
		forgedScope.AllOwners = true
		forgedScope.OwnerIDs = []string{ownerB}
		assertPage(t, forgedScope, inventory.Filters{}, documentA)
		assertPage(t, forgedScope, inventory.Filters{OwnerID: ownerB})
		_, err := store.Get(t.Context(), inventory.DocumentResource, forgedScope, documentB)
		assertProblem(t, err, http.StatusNotFound)
		forgedScope.ActorID = ungrantedActor
		_, err = store.List(t.Context(), inventory.DocumentResource, forgedScope, inventory.Filters{})
		assertProblem(t, err, http.StatusForbidden)
	})
	t.Run("revoked document grants reject a cached read scope while unrelated grants remain", func(t *testing.T) {
		exec(`DELETE FROM platform.actor_grants WHERE actor_id=$1 AND permission='document.read'`, actor)
		defer grantDocumentRead()
		assertPage(t, cachedScope, inventory.Filters{})
		_, err := store.Get(t.Context(), inventory.DocumentResource, cachedScope, documentA)
		assertProblem(t, err, http.StatusNotFound)
		current, err := store.ResolvePrincipal(t.Context(), auth.Identity{Issuer: "onprem-security-critic", Subject: actor})
		if err != nil {
			t.Fatal("unrelated grant should keep the actor resolvable")
		}
		_, err = current.Scope("document.read", org, wh)
		assertProblem(t, err, http.StatusForbidden)
	})
	assertPage(t, cachedScope, inventory.Filters{}, documentA)
}

func onPremCriticPool(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	if os.Getenv(name) == "" {
		t.Fatal("disposable hardened fixture required: " + name)
	}
	p, err := pgxpool.New(t.Context(), os.Getenv(name))
	if err != nil {
		t.Fatal("cannot create disposable fixture pool")
	}
	t.Cleanup(p.Close)
	return p
}

// The singleton and signing key are deliberately changed only in the disposable
// owner fixture. Do not parallelize this test: those two values are installation
// configuration shared by all API requests in this customer's database.
func TestHardenedOnPremCriticReadinessTracksInstallationAndSigningKey(t *testing.T) {
	admin := onPremCriticPool(t, "WMS_HARDENED_ADMIN_DSN")
	runtime := onPremCriticPool(t, "WMS_HARDENED_API_DSN")
	key, err := base64.StdEncoding.DecodeString(os.Getenv("WMS_HARDENED_KEY"))
	if err != nil || len(key) != 32 {
		t.Fatal("a 32-byte synthetic signing key is required")
	}
	store, err := postgres.NewHardened(t.Context(), runtime, key)
	if err != nil {
		t.Fatal("cannot construct hardened store from disposable fixture")
	}

	// Independent actor/document: no fixture actor, organization, owner or document
	// IDs are assumed, and a successful real request establishes the baseline.
	var org, wh, owner, actor, document string
	if err := admin.QueryRow(t.Context(), `SELECT gen_random_uuid()::text,gen_random_uuid()::text,
	 gen_random_uuid()::text,gen_random_uuid()::text,gen_random_uuid()::text`).Scan(&org, &wh, &owner, &actor, &document); err != nil {
		t.Fatal("cannot allocate independent critic fixture IDs")
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), query, args...); err != nil {
			if databaseError, ok := errors.AsType[*pgconn.PgError](err); ok {
				t.Fatalf("disposable critic fixture SQL failed: %s (%s)", databaseError.Message, databaseError.Code)
			}
			t.Fatal("disposable critic fixture SQL failed")
		}
	}
	exec(`INSERT INTO platform.organizations(id,code,name) VALUES($1,$2,'security critic')`, org, org)
	exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$3)`, wh, org, wh)
	exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$3)`, owner, org, owner)
	exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'onprem-security-critic',$2)`, actor, actor)
	exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission)
	 VALUES($1,$2,$3,$4,'document.read')`, actor, org, wh, owner)
	exec(`INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state)
	 VALUES($1,$2,$3,$4,'shipment',$5,'draft')`, document, org, wh, owner, document)
	t.Cleanup(func() {
		// Foreign keys make cleanup order explicit. No persistent data is retained.
		for _, query := range []string{
			`DELETE FROM warehouse.documents WHERE organization_id=$1`,
			`DELETE FROM platform.actor_grants WHERE organization_id=$1`,
			`DELETE FROM catalog.stock_owners WHERE organization_id=$1`,
			`DELETE FROM warehouse.warehouses WHERE organization_id=$1`,
			`DELETE FROM platform.organizations WHERE id=$1`,
		} {
			if _, err := admin.Exec(context.Background(), query, org); err != nil {
				t.Error("cannot clean independent critic fixture")
			}
		}
		if _, err := admin.Exec(context.Background(), `DELETE FROM platform.actors WHERE id=$1`, actor); err != nil {
			t.Error("cannot clean independent critic actor")
		}
	})
	registry, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal("cannot create empty module registry")
	}
	handler, err := api.New(api.Config{Backend: store, Auth: onPremCriticAuth{subject: actor}, Modules: registry})
	if err != nil {
		t.Fatal("cannot construct API")
	}
	request := func(path string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer synthetic-verified-identity")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	documentURL := fmt.Sprintf("/api/v1/documents/%s?organization_id=%s&warehouse_id=%s", document, org, wh)
	healthy := func(t *testing.T) {
		t.Helper()
		if err := store.Ping(t.Context()); err != nil {
			t.Error("Ping rejected restored installation configuration")
		}
		if got := request("/readyz"); got != http.StatusOK {
			t.Errorf("restored /readyz = %d, want 200", got)
		}
		if got := request(documentURL); got != http.StatusOK {
			t.Errorf("restored authenticated document request = %d, want 200", got)
		}
	}
	healthy(t)

	var replacement [32]byte
	if _, err := rand.Read(replacement[:]); err != nil {
		t.Fatal("cannot generate synthetic replacement key")
	}
	cases := []struct {
		name    string
		mutate  func()
		restore func()
	}{
		{"installation identifier changed after startup", func() {
			exec(`UPDATE hardened_private.installation SET id=gen_random_uuid() WHERE singleton`)
		}, func() {
			exec(`UPDATE hardened_private.installation SET id=$1 WHERE singleton`, store.InstallationID())
		}},
		{"signing key changed after startup", func() {
			exec(`UPDATE hardened_private.signing_keys SET key=$1 WHERE login=$2`, replacement[:], store.RuntimeLogin())
		}, func() {
			exec(`UPDATE hardened_private.signing_keys SET key=$1 WHERE login=$2`, key, store.RuntimeLogin())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.mutate()
			defer tc.restore()
			if got := request(documentURL); got == http.StatusOK {
				t.Error("authenticated request accepted changed installation configuration")
			}
			if err := store.Ping(t.Context()); err == nil {
				t.Error("Ping reports healthy while authenticated requests fail after configuration drift")
			}
			if got := request("/readyz"); got != http.StatusServiceUnavailable {
				t.Errorf("/readyz = %d after configuration drift, want 503", got)
			}
			tc.restore()
			healthy(t)
		})
	}
}
