package api

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
)

type RecipeComponent struct {
	ItemID   string `json:"item_id"`
	Quantity string `json:"quantity"`
}
type ProductionRecipe struct {
	ID             string            `json:"id"`
	OrganizationID string            `json:"organization_id"`
	WarehouseID    string            `json:"warehouse_id"`
	ProductItemID  string            `json:"product_item_id"`
	Name           string            `json:"name"`
	Components     []RecipeComponent `json:"components"`
	Steps          []string          `json:"steps"`
}
type workOrderInput struct {
	OrganizationID string                        `json:"organization_id"`
	WarehouseID    string                        `json:"warehouse_id"`
	OwnerID        string                        `json:"owner_id"`
	Customer       string                        `json:"customer"`
	Reference      string                        `json:"reference,omitempty"`
	RecipeID       string                        `json:"recipe_id,omitempty"`
	Lines          []inventory.DocumentLineInput `json:"lines"`
}
type planningBackend interface {
	ListWorkOrders(context.Context, inventory.Scope, inventory.Filters) (inventory.Page, error)
	CommandWorkOrder(context.Context, inventory.Principal, string, json.RawMessage, *modules.Registry) (json.RawMessage, error)
}

func quantityMicros(value string) *big.Int {
	parts := strings.SplitN(value, ".", 2)
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	fraction += strings.Repeat("0", 6-len(fraction))
	n, _ := new(big.Int).SetString(parts[0]+fraction, 10)
	return n
}
func quantityFitsItem(value string, scale int) bool {
	if inventory.ValidateQuantity(value) != nil || scale < 0 || scale > 6 {
		return false
	}
	if parts := strings.SplitN(value, ".", 2); len(parts) == 2 && len(parts[1]) > scale {
		return strings.Trim(parts[1][scale:], "0") == ""
	}
	return true
}
func microsQuantity(n *big.Int) string {
	digits := n.String()
	if len(digits) <= 6 {
		digits = strings.Repeat("0", 7-len(digits)) + digits
	}
	whole := digits[:len(digits)-6]
	fraction := strings.TrimRight(digits[len(digits)-6:], "0")
	if fraction == "" {
		return whole
	}
	return whole + "." + fraction
}

func workOrderLabel(value string, limit int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= limit && !strings.ContainsFunc(value, unicode.IsControl)
}

func (c WorkspaceConfig) orderSnapshot(input workOrderInput) (json.RawMessage, error) {
	invalid := inventory.Invalid("invalid_work_order")
	if !workOrderLabel(input.Customer, 200) || (input.Reference != "" && !workOrderLabel(input.Reference, 100)) || len(input.Lines) < 1 || len(input.Lines) > 200 {
		return nil, invalid
	}
	items := map[string]NamedItem{}
	for _, item := range c.Items {
		if item.OrganizationID == input.OrganizationID {
			items[item.ID] = item
		}
	}
	for i := range input.Lines {
		line := &input.Lines[i]
		normalized, err := inventory.NormalizeID(line.ItemID)
		if err != nil {
			return nil, invalid
		}
		line.ItemID = normalized
		item, ok := items[line.ItemID]
		if !ok || !quantityFitsItem(line.Quantity, item.QuantityScale) {
			return nil, invalid
		}
	}
	demands := map[string]*big.Int{}
	steps := []string{}
	add := func(item, quantity string) error {
		value := quantityMicros(quantity)
		if demands[item] == nil {
			demands[item] = new(big.Int)
		}
		demands[item].Add(demands[item], value)
		if !quantityFitsItem(microsQuantity(demands[item]), items[item].QuantityScale) {
			return invalid
		}
		return nil
	}
	if input.RecipeID != "" {
		var found *ProductionRecipe
		for i := range c.Recipes {
			recipe := &c.Recipes[i]
			if recipe.ID == input.RecipeID && recipe.OrganizationID == input.OrganizationID && recipe.WarehouseID == input.WarehouseID {
				found = recipe
				break
			}
		}
		if found == nil || len(input.Lines) != 1 || input.Lines[0].ItemID != found.ProductItemID {
			return nil, invalid
		}
		for _, component := range found.Components {
			product := new(big.Int).Mul(quantityMicros(component.Quantity), quantityMicros(input.Lines[0].Quantity))
			value, remainder := new(big.Int), new(big.Int)
			value.QuoRem(product, big.NewInt(1_000_000), remainder)
			if remainder.Sign() != 0 || add(component.ItemID, microsQuantity(value)) != nil {
				return nil, invalid
			}
		}
		steps = append(steps, found.Steps...)
	} else {
		for _, line := range input.Lines {
			if err := add(line.ItemID, line.Quantity); err != nil {
				return nil, err
			}
		}
	}
	ids := make([]string, 0, len(demands))
	for id := range demands {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	requirements := make([]RecipeComponent, 0, len(ids))
	for _, id := range ids {
		requirements = append(requirements, RecipeComponent{ItemID: id, Quantity: microsQuantity(demands[id])})
	}
	return json.Marshal(map[string]any{"organization_id": input.OrganizationID, "warehouse_id": input.WarehouseID, "owner_id": input.OwnerID, "customer": input.Customer, "reference": input.Reference, "recipe_id": input.RecipeID, "lines": input.Lines, "requirements": requirements, "steps": steps, "request": input})
}

func (s *Server) workOrders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		s.method(w, r, "GET, POST")
		return
	}
	backend, ok := s.config.Backend.(planningBackend)
	if !ok {
		s.problem(w, r, &inventory.Problem{Status: 503, Code: "planning_unavailable", Detail: "Planning storage is unavailable."})
		return
	}
	p := principal(r)
	if r.Method == http.MethodGet {
		org, wh, err := queryScope(r)
		if err != nil {
			s.problem(w, r, err)
			return
		}
		scope, err := p.Scope("document.read", org, wh)
		if err != nil {
			s.problem(w, r, err)
			return
		}
		filters := inventory.Filters{Limit: 50, Cursor: r.URL.Query().Get("cursor")}
		if len(filters.Cursor) > 2048 {
			s.problem(w, r, inventory.Invalid("invalid_cursor"))
			return
		}
		if value := r.URL.Query().Get("limit"); value != "" {
			filters.Limit, err = strconv.Atoi(value)
			if err != nil || filters.Limit < 1 || filters.Limit > 200 {
				s.problem(w, r, inventory.Invalid("invalid_limit"))
				return
			}
		}
		if value := r.URL.Query().Get("owner_id"); value != "" {
			filters.OwnerID, err = inventory.NormalizeID(value)
			if err != nil {
				s.problem(w, r, err)
				return
			}
		}
		page, err := backend.ListWorkOrders(r.Context(), scope, filters)
		if err != nil {
			s.problem(w, r, err)
			return
		}
		if page.Data == nil {
			page.Data = []json.RawMessage{}
		}
		var next any
		if page.NextCursor != "" {
			next = page.NextCursor
		}
		writeJSON(w, 200, map[string]any{"data": page.Data, "pagination": map[string]any{"next_cursor": next}})
		return
	}
	command, err := inventory.NormalizeID(r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.problem(w, r, inventory.Invalid("invalid_idempotency_key"))
		return
	}
	var input workOrderInput
	if err := decode(w, r, &input); err != nil {
		s.problem(w, r, err)
		return
	}
	for _, field := range []*string{&input.OrganizationID, &input.WarehouseID, &input.OwnerID} {
		value, err := inventory.NormalizeID(*field)
		if err != nil {
			s.problem(w, r, err)
			return
		}
		*field = value
	}
	if input.RecipeID != "" {
		input.RecipeID, err = inventory.NormalizeID(input.RecipeID)
		if err != nil {
			s.problem(w, r, err)
			return
		}
	}
	scope, err := p.Scope("document.create", input.OrganizationID, input.WarehouseID)
	if err != nil {
		s.problem(w, r, err)
		return
	}
	if !scope.AllowsOwner(input.OwnerID) {
		s.problem(w, r, inventory.Forbidden())
		return
	}
	raw, err := s.config.Workspace.orderSnapshot(input)
	if err != nil {
		s.problem(w, r, err)
		return
	}
	result, err := backend.CommandWorkOrder(r.Context(), p, command, raw, s.config.Modules)
	if err != nil {
		s.problem(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"data": result})
}
