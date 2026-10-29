//go:build integration && hardened

package postgres_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgconn"
)

// This uses the actual onprem-v1 Store and signed RPC, not the legacy runtime.
// It owns all business IDs so the profile gate may shuffle it with other tests.
func TestHardenedDocumentAutomaticNumberingSignedReplayAndPermissions(t *testing.T) {
	admin := onPremCriticPool(t, "WMS_HARDENED_ADMIN_DSN")
	runtime := onPremCriticPool(t, "WMS_HARDENED_API_DSN")
	key, err := base64.StdEncoding.DecodeString(os.Getenv("WMS_HARDENED_KEY"))
	if err != nil || len(key) != 32 {
		t.Fatal("a synthetic 32-byte signing key is required")
	}
	store, err := postgres.NewHardened(t.Context(), runtime, key)
	if err != nil {
		t.Fatal("cannot construct the disposable onprem-v1 store")
	}
	registry, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	org, wh, owner, item, actor, deniedActor := id(), id(), id(), id(), id(), id()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), query, args...); err != nil {
			t.Fatal("cannot seed independent numbering fixture")
		}
	}
	exec(`INSERT INTO platform.organizations(id,code,name) VALUES($1,$1::uuid::text,'numbering signed RPC critic')`, org)
	exec(`INSERT INTO warehouse.warehouses(id,organization_id,code,timezone) VALUES($1,$2,$1::uuid::text,'Europe/Warsaw')`, wh, org)
	exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, owner, org)
	exec(`INSERT INTO catalog.units(code,dimension) VALUES('ea','count') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES($1,$2,$1::uuid::text,'ea',0,'none')`, item, org)
	exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'numbering-signed-critic',$1::uuid::text),($2,'numbering-signed-critic',$2::uuid::text)`, actor, deniedActor)
	for _, permission := range []string{"document.create", "document.read"} {
		exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, actor, org, wh, owner, permission)
	}
	principal, err := store.ResolvePrincipal(t.Context(), auth.Identity{Issuer: "numbering-signed-critic", Subject: actor})
	if err != nil {
		t.Fatal("cannot resolve independent numbered-document actor")
	}
	var year int
	if err := admin.QueryRow(t.Context(), `SELECT extract(year FROM transaction_timestamp() AT TIME ZONE timezone)::int FROM warehouse.warehouses WHERE id=$1`, wh).Scan(&year); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"receipt", "transfer"} {
		t.Run(kind, func(t *testing.T) {
			input := inventory.DocumentInput{OrganizationID: org, WarehouseID: wh, OwnerID: owner, Kind: kind, Lines: []inventory.DocumentLineInput{{ItemID: item, Quantity: "1"}}}
			// Forged in-memory grants must not bypass the kernel's actor check.
			forged := inventory.Principal{ActorID: deniedActor, Grants: principal.Grants}
			deniedKey := id()
			if _, err := store.CommandDocument(t.Context(), forged, deniedKey, "", input, registry); err == nil {
				t.Fatal("signed RPC accepted forged actor grants")
			}
			var count int
			if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM platform.operations WHERE id=$1`, deniedKey).Scan(&count); err != nil || count != 0 {
				t.Fatalf("denied signed command retained a receipt: count=%d err=%v", count, err)
			}
			prefix := map[string]string{"receipt": "PZ", "transfer": "MM"}[kind]
			for sequence := 1; sequence <= 2; sequence++ {
				command := id()
				first, err := store.CommandDocument(t.Context(), principal, command, "", input, registry)
				if err != nil {
					t.Fatalf("numbered document through signed RPC failed: %v", err)
				}
				var result struct {
					Document string `json:"document_id"`
				}
				if err := json.Unmarshal(first, &result); err != nil || result.Document == "" {
					t.Fatal("signed create returned no document")
				}
				for range 2 {
					replay, err := store.CommandDocument(t.Context(), principal, command, "", input, registry)
					if err != nil || string(replay) != string(first) {
						t.Fatalf("signed replay changed original receipt: %v", err)
					}
				}
				scope, err := principal.Scope("document.read", org, wh)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := store.Get(t.Context(), inventory.DocumentResource, scope, result.Document)
				if err != nil {
					t.Fatalf("signed document read failed: %v", err)
				}
				var document struct {
					Number string `json:"number"`
				}
				if err := json.Unmarshal(raw, &document); err != nil {
					t.Fatal(err)
				}
				if want := fmt.Sprintf("%s/%04d/%06d", prefix, year, sequence); document.Number != want {
					t.Fatalf("signed numbering=%q want=%q", document.Number, want)
				}
			}
		})
	}
	// Stolen SQL credentials have neither counter writes nor allocator EXECUTE.
	for _, query := range []string{`SELECT warehouse.allocate_document_number($1::uuid,$2::uuid,'receipt',transaction_timestamp())`, `UPDATE warehouse.document_number_counters SET last_value=0 WHERE organization_id=$1 AND warehouse_id=$2`} {
		_, err := runtime.Exec(t.Context(), query, org, wh)
		problem, ok := errors.AsType[*pgconn.PgError](err)
		if !ok || problem.Code != "42501" {
			t.Fatalf("numbering SQL access was not denied with 42501: %v", err)
		}
	}
}
