package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
)

func businessDocumentHandler(t *testing.T, permission, owner string) (http.Handler, *numberingBackend) {
	t.Helper()
	b := &numberingBackend{fakeBackend: &fakeBackend{principal: inventory.Principal{ActorID: testActor,
		Grants: []inventory.Grant{{OrganizationID: testOrg, WarehouseID: testWarehouse, OwnerID: owner, Permission: permission}}}}}
	registry, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Backend: b, Auth: fakeAuth{}, Modules: registry})
	if err != nil {
		t.Fatal(err)
	}
	return h, b
}

func businessDocumentBody(kind string, fields map[string]any) string {
	body := map[string]any{"organization_id": testOrg, "warehouse_id": testWarehouse, "owner_id": testActor,
		"kind": kind, "lines": []any{map[string]any{"item_id": testLine, "quantity": "2"}}}
	maps.Copy(body, fields)
	raw, _ := json.Marshal(body)
	return string(raw)
}

func TestDocumentBusinessHTTPRecipientAndOrderReference(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		fields     map[string]any
		want       int
	}{
		{"automatic WZ preserves recipient and reference", "shipment", map[string]any{"recipient": "Fabryka Mebli Alfa — Jan Kowalski", "order_reference": "ZAM/2026/001"}, 201},
		{"automatic WZ requires recipient", "shipment", nil, 400},
		{"automatic WZ rejects blank recipient", "shipment", map[string]any{"recipient": " \t\u00a0"}, 400},
		{"automatic WZ rejects recipient control characters", "shipment", map[string]any{"recipient": "Firma\nodbiorca"}, 400},
		{"automatic WZ recipient maximum uses Unicode characters", "shipment", map[string]any{"recipient": strings.Repeat("ą", 200)}, 201},
		{"automatic WZ rejects oversized recipient", "shipment", map[string]any{"recipient": strings.Repeat("ą", 201)}, 400},
		{"automatic WZ rejects oversized reference", "shipment", map[string]any{"recipient": "Firma Alfa", "order_reference": strings.Repeat("Ź", 101)}, 400},
		{"MM may link an order", "transfer", map[string]any{"order_reference": "ZAM/2026/001"}, 201},
		{"PZ may name a supplier", "receipt", map[string]any{"recipient": "Dostawca Stalmet", "order_reference": "DOST/17"}, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, backend := businessDocumentHandler(t, "document.create", "")
			response := request(h, "POST", "/api/v1/documents", businessDocumentBody(tc.kind, tc.fields), "valid", testCommand, "application/json")
			if response.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.want, response.Body)
			}
			if tc.want != 201 {
				if backend.calls != 0 {
					t.Fatal("invalid business context reached the backend")
				}
				return
			}
			raw, _ := json.Marshal(backend.input)
			var forwarded map[string]any
			_ = json.Unmarshal(raw, &forwarded)
			for _, field := range []string{"recipient", "order_reference"} {
				if want, exists := tc.fields[field]; exists && forwarded[field] != want {
					t.Fatalf("business identity changed before durable replay: %s=%v want=%v", field, forwarded[field], want)
				}
			}
		})
	}
}

func TestDocumentBusinessHTTPWorkerKindSpecificPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, permission, owner, kind string
		want                          int
	}{
		{"worker may issue", "document.issue", testActor, "shipment", 201},
		{"worker may prepare own movement", "document.move", testActor, "transfer", 201},
		{"issue does not grant receipt creation", "document.issue", testActor, "receipt", 403},
		{"move does not grant receipt creation", "document.move", testActor, "receipt", 403},
		{"move does not grant shipment creation", "document.move", testActor, "shipment", 403},
		{"issue does not grant another owner", "document.issue", testLine, "shipment", 403},
		{"move does not grant another owner", "document.move", testLine, "transfer", 403},
		{"bureau keeps shipment creation", "document.create", testActor, "shipment", 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, backend := businessDocumentHandler(t, tc.permission, tc.owner)
			fields := map[string]any{}
			if tc.kind == "shipment" {
				fields["recipient"] = "Firma Alfa — Piotr Nowak"
			}
			response := request(h, "POST", "/api/v1/documents", businessDocumentBody(tc.kind, fields), "valid", testCommand, "application/json")
			if response.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.want, response.Body)
			}
			if tc.want == 403 && backend.calls != 0 {
				t.Fatal("forbidden document reached backend")
			}
		})
	}
}
