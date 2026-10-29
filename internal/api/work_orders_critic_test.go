package api

import (
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
)

func criticOrderHandler(t *testing.T, config WorkspaceConfig) (http.Handler, *ordersBackend) {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := DecodeWorkspaceConfig(raw)
	if err != nil {
		t.Fatalf("critic fixture configuration is invalid: %v", err)
	}
	b := &ordersBackend{principal: inventory.Principal{ActorID: testActor, Grants: []inventory.Grant{
		{OrganizationID: testOrg, WarehouseID: testWarehouse, OwnerID: planOwner, Permission: "document.create"},
		{OrganizationID: testOrg, WarehouseID: testWarehouse, OwnerID: planOwner, Permission: "document.read"},
	}}}
	registry, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Backend: b, Auth: fakeAuth{}, Modules: registry, Workspace: validated})
	if err != nil {
		t.Fatal(err)
	}
	return h, b
}

func criticBaseWorkspace(t *testing.T) WorkspaceConfig {
	t.Helper()
	config, err := DecodeWorkspaceConfig([]byte(orderWorkspaceJSON("1")))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestWorkOrdersCriticRationalRecipeProperties(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(0x20261029, 0x574d53))
	for history := range 64 {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			config := criticBaseWorkspace(t)
			config.Items[0].QuantityScale = 2
			config.Recipes[0].Components = nil
			quantity := fmt.Sprintf("%d.%02d", 1+rng.IntN(10000), rng.IntN(100))
			want := map[string]string{}
			for component := range 1 + rng.IntN(6) {
				itemID := fmt.Sprintf("00000000-0000-4000-8000-%012x", 100+component)
				config.Items = append(config.Items, NamedItem{ID: itemID, OrganizationID: testOrg, SKU: fmt.Sprint(component), Name: "Materiał dokładny", Unit: "m", QuantityScale: 6})
				coefficient := fmt.Sprintf("%d.%04d", 1+rng.IntN(100), rng.IntN(10000))
				config.Recipes[0].Components = append(config.Recipes[0].Components, RecipeComponent{ItemID: itemID, Quantity: coefficient})
				// An independent rational oracle never calls the implementation's micros helpers.
				factor, _ := new(big.Rat).SetString(coefficient)
				ordered, _ := new(big.Rat).SetString(quantity)
				exact := new(big.Rat).Mul(factor, ordered).FloatString(6)
				want[itemID] = strings.TrimRight(strings.TrimRight(exact, "0"), ".")
			}
			h, backend := criticOrderHandler(t, config)
			response := request(h, "POST", "/api/v1/work-orders", planBody(quantity), "valid", testCommand, "application/json")
			if response.Code != 201 {
				t.Fatalf("valid exact recipe rejected: %d %s", response.Code, response.Body.String())
			}
			var snapshot struct {
				Requirements []RecipeComponent `json:"requirements"`
			}
			if err := json.Unmarshal(backend.submitted, &snapshot); err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Requirements) != len(want) {
				t.Fatalf("component cardinality changed: got=%d want=%d", len(snapshot.Requirements), len(want))
			}
			for _, requirement := range snapshot.Requirements {
				if requirement.Quantity != want[requirement.ItemID] {
					t.Fatalf("exact BOM differs for %s: got=%s want=%s", requirement.ItemID, requirement.Quantity, want[requirement.ItemID])
				}
			}
		})
	}
}

func TestWorkOrdersCriticDispatchAggregationProperties(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(81, 20261029))
	for history := range 48 {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			config := criticBaseWorkspace(t)
			config.Items[0].QuantityScale = 6
			config.Items[1].QuantityScale = 6
			input := workOrderInput{OrganizationID: testOrg, WarehouseID: testWarehouse, OwnerID: planOwner, Customer: "Łódzkie Biuro Projektowe", Lines: []inventory.DocumentLineInput{}}
			want := map[string]*big.Rat{}
			for range 1 + rng.IntN(30) {
				item := config.Items[rng.IntN(2)].ID
				quantity := fmt.Sprintf("%d.%06d", rng.IntN(1000), 1+rng.IntN(999999))
				input.Lines = append(input.Lines, inventory.DocumentLineInput{ItemID: item, Quantity: quantity})
				if want[item] == nil {
					want[item] = new(big.Rat)
				}
				n, _ := new(big.Rat).SetString(quantity)
				want[item].Add(want[item], n)
			}
			h, backend := criticOrderHandler(t, config)
			body, _ := json.Marshal(input)
			response := request(h, "POST", "/api/v1/work-orders", string(body), "valid", testCommand, "application/json")
			if response.Code != 201 {
				t.Fatalf("valid dispatch rejected: %d %s", response.Code, response.Body.String())
			}
			var snapshot struct{ Requirements []RecipeComponent }
			if err := json.Unmarshal(backend.submitted, &snapshot); err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Requirements) != len(want) {
				t.Fatalf("duplicate product aggregation changed cardinality: %d/%d", len(snapshot.Requirements), len(want))
			}
			ids := []string{}
			for _, row := range snapshot.Requirements {
				ids = append(ids, row.ItemID)
				got, ok := new(big.Rat).SetString(row.Quantity)
				if !ok || got.Cmp(want[row.ItemID]) != 0 {
					t.Fatalf("aggregate changed for %s: %s != %s", row.ItemID, row.Quantity, want[row.ItemID])
				}
			}
			if !slices.IsSorted(ids) {
				t.Fatal("requirements order is unstable")
			}
		})
	}
}

func TestWorkOrdersCriticRecipeScopeAndPrecision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		change   func(*WorkspaceConfig)
		quantity string
	}{
		{"foreign warehouse recipe", func(c *WorkspaceConfig) {
			c.Warehouses = append(c.Warehouses, NamedWarehouse{ID: testActor, OrganizationID: testOrg, Name: "Obcy magazyn"})
			c.Recipes[0].WarehouseID = testActor
		}, "1"},
		{"fraction below accounting precision", func(c *WorkspaceConfig) {
			c.Items[0].QuantityScale = 6
			c.Recipes[0].Components[0].Quantity = "0.000001"
		}, "0.000001"},
		{"component scale cannot hold fractional result", func(c *WorkspaceConfig) {
			c.Items[0].QuantityScale = 6
			c.Items[1].QuantityScale = 0
		}, "0.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := criticBaseWorkspace(t)
			tc.change(&config)
			h, backend := criticOrderHandler(t, config)
			response := request(h, "POST", "/api/v1/work-orders", planBody(tc.quantity), "valid", testCommand, "application/json")
			if response.Code != 400 || backend.calls != 0 {
				t.Fatalf("invalid scoped/precision plan reached storage: %d writes=%d", response.Code, backend.calls)
			}
		})
	}
}

func TestWorkOrdersCriticUnicodeCustomerAndReference(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, customer, reference string
		status                    int
	}{
		{"Polish letters", "Łódzka Spółka Śrub i Nitów", "Zamówienie Żółć #1", 201},
		{"Unicode-only customer", "\u00a0\u2003", "", 400},
		{"control character", "Fabryka\nAlfa", "", 400},
		{"control reference", "Fabryka Alfa", "ZAM\t1", 400},
		{"200 Polish customer characters", strings.Repeat("ą", 200), "", 201},
		{"100 Polish reference characters", "Fabryka Alfa", strings.Repeat("ż", 100), 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, backend := criticOrderHandler(t, criticBaseWorkspace(t))
			input := workOrderInput{OrganizationID: testOrg, WarehouseID: testWarehouse, OwnerID: planOwner, Customer: tc.customer, Reference: tc.reference, Lines: []inventory.DocumentLineInput{{ItemID: testLine, Quantity: "1"}}}
			raw, _ := json.Marshal(input)
			response := request(h, "POST", "/api/v1/work-orders", string(raw), "valid", testCommand, "application/json")
			if response.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.status, response.Body.String())
			}
			if tc.status != 201 && backend.calls != 0 {
				t.Fatal("invalid labels reached storage")
			}
		})
	}
}
