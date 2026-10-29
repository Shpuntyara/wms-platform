package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
)

const planRecipe = "00000000-0000-4000-8000-000000000011"
const planOwner = "00000000-0000-4000-8000-000000000012"

func orderWorkspaceJSON(componentQuantity string) string {
	return fmt.Sprintf(`{"organizations":[{"id":%q,"name":"METALFORM"}],"warehouses":[{"id":%q,"organization_id":%q,"name":"Magazyn metalu"}],"owners":[{"id":%q,"organization_id":%q,"name":"METALFORM"}],"items":[{"id":%q,"organization_id":%q,"sku":"SZAFA-10","name":"Szafa metalowa","unit":"szt.","quantity_scale":0},{"id":%q,"organization_id":%q,"sku":"SR-M8","name":"Śruba M8","unit":"szt.","quantity_scale":6}],"positions":[{"id":%q,"organization_id":%q,"warehouse_id":%q,"code":"A1","kind":"storage","name":"Regał A · poziom 1"}],"recipes":[{"id":%q,"organization_id":%q,"warehouse_id":%q,"product_item_id":%q,"name":"Szafa metalowa","components":[{"item_id":%q,"quantity":%q}],"steps":["Spawanie","Szlifowanie","Lakierowanie","Montaż"]}]}`, testOrg, testWarehouse, testOrg, planOwner, testOrg, testLine, testOrg, testBalance, testOrg, testCommand, testOrg, testWarehouse, planRecipe, testOrg, testWarehouse, testLine, testBalance, componentQuantity)
}

type ordersBackend struct {
	fakeBackend
	submitted json.RawMessage
	listed    inventory.Scope
}

func (f *ordersBackend) CommandWorkOrder(_ context.Context, _ inventory.Principal, _ string, raw json.RawMessage, _ *modules.Registry) (json.RawMessage, error) {
	f.submitted = append(json.RawMessage{}, raw...)
	f.calls++
	return json.RawMessage(`{"id":"` + testCommand + `","state":"planned"}`), nil
}
func (f *ordersBackend) ListWorkOrders(_ context.Context, s inventory.Scope, _ inventory.Filters) (inventory.Page, error) {
	f.listed = s
	f.calls++
	return inventory.Page{Data: []json.RawMessage{}}, nil
}

func ordersHandler(t *testing.T, quantity string, permissions ...string) (http.Handler, *ordersBackend) {
	t.Helper()
	b := &ordersBackend{principal: inventory.Principal{ActorID: testActor}}
	for _, p := range permissions {
		b.principal.Grants = append(b.principal.Grants, inventory.Grant{OrganizationID: testOrg, WarehouseID: testWarehouse, OwnerID: planOwner, Permission: p})
	}
	var workspace WorkspaceConfig
	if err := json.Unmarshal([]byte(orderWorkspaceJSON(quantity)), &workspace); err != nil {
		t.Fatal(err)
	}
	registry, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Backend: b, Auth: fakeAuth{}, Modules: registry, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	return h, b
}

func planBody(quantity string) string {
	return fmt.Sprintf(`{"organization_id":%q,"warehouse_id":%q,"owner_id":%q,"customer":"Fabryka Alfa Sp. z o.o.","reference":"Zamówienie #1","recipe_id":%q,"lines":[{"item_id":%q,"quantity":%q}]}`, testOrg, testWarehouse, planOwner, planRecipe, testLine, quantity)
}

func TestWorkOrdersIndustryConfig(t *testing.T) {
	raw := orderWorkspaceJSON("16")
	c, err := DecodeWorkspaceConfig([]byte(raw))
	if err != nil {
		t.Fatalf("valid industry recipe and human location label rejected: %v", err)
	}
	data, _ := json.Marshal(c)
	if !strings.Contains(string(data), `"recipes"`) || !strings.Contains(string(data), "Regał A") {
		t.Fatal("industry catalog was lost")
	}
	for _, tc := range []struct{ name, raw string }{
		{"zero component", strings.Replace(raw, `"quantity":"16"`, `"quantity":"0"`, 1)},
		{"unknown component", strings.Replace(raw, `"item_id":"`+testBalance+`"`, `"item_id":"`+testActor+`"`, 1)},
		{"self component", strings.Replace(raw, `"item_id":"`+testBalance+`"`, `"item_id":"`+testLine+`"`, 1)},
		{"wrong warehouse", strings.Replace(raw, `"warehouse_id":"`+testWarehouse+`","product_item_id"`, `"warehouse_id":"`+testActor+`","product_item_id"`, 1)},
		{"empty step", strings.Replace(raw, `"Spawanie"`, `" "`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeWorkspaceConfig([]byte(tc.raw)); err == nil {
				t.Fatal("invalid recipe accepted")
			}
		})
	}
}

func TestWorkOrdersCalculateRecipeBeforePersisting(t *testing.T) {
	for _, tc := range []struct{ name, unit, quantity, want string }{
		{"ten cabinets", "16", "10", "160"},
		{"exact beyond JS safe integer", "0.000001", "9007199254740993", "9007199254.740993"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, b := ordersHandler(t, tc.unit, "document.create", "document.read")
			w := request(h, "POST", "/api/v1/work-orders", planBody(tc.quantity), "valid", testCommand, "application/json")
			if w.Code != 201 {
				t.Fatalf("order create status=%d body=%s", w.Code, w.Body.String())
			}
			var snapshot struct {
				Requirements []struct {
					ItemID, Quantity string `json:"-"`
				}
				Steps []string
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(b.submitted, &raw); err != nil {
				t.Fatal(err)
			}
			var requirements []map[string]string
			if err := json.Unmarshal(raw["requirements"], &requirements); err != nil {
				t.Fatal(err)
			}
			if len(requirements) != 1 || requirements[0]["item_id"] != testBalance || requirements[0]["quantity"] != tc.want {
				t.Fatalf("wrong exact BOM requirements: %s", raw["requirements"])
			}
			_ = json.Unmarshal(raw["steps"], &snapshot.Steps)
			if len(snapshot.Steps) != 4 {
				t.Fatal("production tasks lost")
			}
			if b.calls != 1 {
				t.Fatal("order submitted more than once")
			}
		})
	}
}

func TestWorkOrdersRejectBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		permissions []string
		status      int
	}{
		{"worker cannot plan", planBody("10"), []string{"document.read", "stock.read"}, 403},
		{"missing recipient", strings.Replace(planBody("10"), `"Fabryka Alfa Sp. z o.o."`, `" "`, 1), []string{"document.create"}, 400},
		{"fractional cabinet", planBody("1.5"), []string{"document.create"}, 400},
		{"overflow BOM", planBody("999999999999999999"), []string{"document.create"}, 400},
		{"negative", planBody("-10"), []string{"document.create"}, 400},
		{"forged requirements", strings.TrimSuffix(planBody("10"), "}") + `,"requirements":[{"item_id":"` + testBalance + `","quantity":"1"}]}`, []string{"document.create"}, 400},
		{"foreign owner", strings.Replace(planBody("10"), planOwner, testActor, 1), []string{"document.create"}, 403},
		{"foreign product", strings.Replace(planBody("10"), testLine, testActor, 1), []string{"document.create"}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, b := ordersHandler(t, "16", tc.permissions...)
			w := request(h, "POST", "/api/v1/work-orders", tc.body, "valid", testCommand, "application/json")
			if w.Code != tc.status || b.calls != 0 {
				t.Fatalf("status=%d want=%d writes=%d body=%s", w.Code, tc.status, b.calls, w.Body.String())
			}
		})
	}
}

func TestWorkOrdersReadPreservesOwnerScope(t *testing.T) {
	h, b := ordersHandler(t, "16", "document.read")
	w := request(h, "GET", "/api/v1/work-orders?organization_id="+testOrg+"&warehouse_id="+testWarehouse, "", "valid", "", "")
	if w.Code != 200 || b.listed.AllOwners || len(b.listed.OwnerIDs) != 1 || b.listed.OwnerIDs[0] != planOwner {
		t.Fatalf("scoped orders read failed: status=%d scope=%+v", w.Code, b.listed)
	}
}
