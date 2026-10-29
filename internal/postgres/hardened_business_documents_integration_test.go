//go:build integration && hardened

package postgres_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
)

func TestHardenedDocumentBusinessWZAndMMSignedOwnerPermissions(t *testing.T) {
	admin := onPremCriticPool(t, "WMS_HARDENED_ADMIN_DSN")
	runtime := onPremCriticPool(t, "WMS_HARDENED_API_DSN")
	key, err := base64.StdEncoding.DecodeString(os.Getenv("WMS_HARDENED_KEY"))
	if err != nil || len(key) != 32 {
		t.Fatal("a synthetic 32-byte signing key is required")
	}
	store, err := postgres.NewHardened(t.Context(), runtime, key)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	org, wh, owner, otherOwner, item, worker, denied := id(), id(), id(), id(), id(), id(), id()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO platform.organizations(id,code,name) VALUES($1,$1::uuid::text,'business document signed RPC')`, org)
	exec(`INSERT INTO warehouse.warehouses(id,organization_id,code,timezone) VALUES($1,$2,$1::uuid::text,'Europe/Warsaw')`, wh, org)
	exec(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$3,$1::uuid::text),($2,$3,$2::uuid::text)`, owner, otherOwner, org)
	exec(`INSERT INTO catalog.units(code,dimension) VALUES('ea','count') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES($1,$2,$1::uuid::text,'ea',0,'none')`, item, org)
	exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'business-signed-critic',$1::uuid::text),($2,'business-signed-critic',$2::uuid::text)`, worker, denied)
	for _, permission := range []string{"document.read", "document.issue", "document.move"} {
		exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, worker, org, wh, owner, permission)
	}
	principal, err := store.ResolvePrincipal(t.Context(), auth.Identity{Issuer: "business-signed-critic", Subject: worker})
	if err != nil {
		t.Fatal(err)
	}
	var year int
	if err := admin.QueryRow(t.Context(), `SELECT extract(year FROM transaction_timestamp() AT TIME ZONE 'Europe/Warsaw')::int`).Scan(&year); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, kind, prefix string }{{"worker WZ", "shipment", "WZ"}, {"worker MM", "transfer", "MM"}} {
		t.Run(tc.name, func(t *testing.T) {
			input := inventory.DocumentInput{OrganizationID: org, WarehouseID: wh, OwnerID: owner, Kind: tc.kind,
				Recipient: "Firma Alfa — odbiór Piotr Nowak", OrderReference: "ZAM/2026/001", Lines: []inventory.DocumentLineInput{{ItemID: item, Quantity: "2"}}}
			command := id()
			first, err := store.CommandDocument(t.Context(), principal, command, "", input, registry)
			if err != nil {
				t.Fatal("signed worker issue/move was rejected: ", err)
			}
			var result struct {
				ID string `json:"document_id"`
			}
			if json.Unmarshal(first, &result) != nil || result.ID == "" {
				t.Fatal("missing signed document identity")
			}
			replay, err := store.CommandDocument(t.Context(), principal, command, "", input, registry)
			if err != nil || string(first) != string(replay) {
				t.Fatal("signed replay changed the issue/move command result")
			}
			scope, err := principal.Scope("document.read", org, wh)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := store.Get(t.Context(), inventory.DocumentResource, scope, result.ID)
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Number, Recipient string
				Reference         string `json:"order_reference"`
			}
			if json.Unmarshal(raw, &doc) != nil || doc.Number != fmt.Sprintf("%s/%d/000001", tc.prefix, year) || doc.Recipient != input.Recipient || doc.Reference != input.OrderReference {
				t.Fatalf("signed document read lost business fields: %s", raw)
			}
			for _, attack := range []struct {
				principal inventory.Principal
				owner     string
			}{
				{inventory.Principal{ActorID: denied, Grants: principal.Grants}, owner},
				{inventory.Principal{ActorID: worker, Grants: []inventory.Grant{{OrganizationID: org, WarehouseID: wh, Permission: "document.create"}}}, otherOwner},
			} {
				input.OwnerID = attack.owner
				if _, err := store.CommandDocument(t.Context(), attack.principal, id(), "", input, registry); err == nil {
					t.Fatal("forged in-memory grants escalated signed worker owner scope")
				}
			}
		})
	}
	receipt := inventory.DocumentInput{OrganizationID: org, WarehouseID: wh, OwnerID: owner, Kind: "receipt", Lines: []inventory.DocumentLineInput{{ItemID: item, Quantity: "1"}}}
	if _, err := store.CommandDocument(t.Context(), principal, id(), "", receipt, registry); err == nil {
		t.Fatal("WZ/MM permissions admitted PZ creation")
	}
	for _, query := range []string{
		`SELECT warehouse.allocate_document_number($1::uuid,$2::uuid,'shipment',transaction_timestamp())`,
		`UPDATE warehouse.document_number_counters SET last_value=0 WHERE organization_id=$1 AND warehouse_id=$2`,
	} {
		if _, err := runtime.Exec(t.Context(), query, org, wh); err == nil {
			t.Fatal("runtime SQL credentials can directly tamper with WZ counter")
		}
	}
}
