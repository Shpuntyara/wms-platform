package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
)

type numberingBackend struct {
	*fakeBackend
	input   inventory.DocumentInput
	command string
}

func (b *numberingBackend) CommandDocument(_ context.Context, _ inventory.Principal, command, _ string, input inventory.DocumentInput, _ *modules.Registry) (json.RawMessage, error) {
	b.calls++
	b.input, b.command = input, command
	return json.RawMessage(`{"document_id":"` + testLine + `"}`), nil
}

func numberingHandler(t *testing.T, granted bool) (http.Handler, *numberingBackend) {
	t.Helper()
	b := &numberingBackend{fakeBackend: &fakeBackend{principal: inventory.Principal{ActorID: testActor}}}
	if granted {
		b.principal.Grants = []inventory.Grant{{OrganizationID: testOrg, WarehouseID: testWarehouse, Permission: "document.create"}}
	}
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

func TestDocumentNumberingHTTPContract(t *testing.T) {
	for _, kind := range []string{"receipt", "transfer", "shipment", "adjustment"} {
		for _, number := range []struct {
			name, raw, value string
		}{
			{"omitted", "", ""}, {"empty", `,"number":""`, ""}, {"null", `,"number":null`, ""},
			{"legacy manual", `,"number":"EXT-42"`, "EXT-42"}, {"manual spaces preserved", `,"number":" EXT-42 "`, " EXT-42 "},
			{"whitespace invalid", `,"number":"  "`, "  "},
		} {
			t.Run(kind+"/"+number.name, func(t *testing.T) {
				h, backend := numberingHandler(t, true)
				body := `{"organization_id":"` + testOrg + `","warehouse_id":"` + testWarehouse + `","owner_id":"` + testActor + `","kind":"` + kind + `"` + number.raw + `,"lines":[{"item_id":"` + testLine + `","quantity":"1"}]}`
				if kind == "shipment" {
					body = strings.TrimSuffix(body, "}") + `,"recipient":"Firma Alfa"}`
				}
				valid := strings.TrimSpace(number.value) != "" || (number.value == "" && (kind == "receipt" || kind == "transfer" || kind == "shipment"))
				want, calls := 400, 0
				if valid {
					want, calls = 201, 1
				}
				response := request(h, "POST", "/api/v1/documents", body, "valid", testCommand, "application/json")
				if response.Code != want || backend.calls != calls {
					t.Fatalf("status=%d calls=%d, want status=%d calls=%d; body=%s", response.Code, backend.calls, want, calls, response.Body)
				}
				if valid && (backend.input.Number != number.value || backend.command != testCommand) {
					t.Fatalf("HTTP layer changed number/command before database idempotency: input=%+v command=%q", backend.input, backend.command)
				}
			})
		}
	}
}

func TestAutomaticDocumentNumberingStillRequiresEmployeePermission(t *testing.T) {
	h, backend := numberingHandler(t, false)
	body := `{"organization_id":"` + testOrg + `","warehouse_id":"` + testWarehouse + `","owner_id":"` + testActor + `","kind":"receipt","lines":[{"item_id":"` + testLine + `","quantity":"1"}]}`
	response := request(h, "POST", "/api/v1/documents", body, "valid", testCommand, "application/json")
	if response.Code != 403 || backend.calls != 0 {
		t.Fatalf("status=%d calls=%d; want permission denial before numbering: %s", response.Code, backend.calls, response.Body)
	}
}
