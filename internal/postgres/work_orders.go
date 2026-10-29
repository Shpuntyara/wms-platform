package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func orderScope(p inventory.Principal, raw json.RawMessage) (inventory.Scope, error) {
	var input struct {
		OrganizationID string `json:"organization_id"`
		WarehouseID    string `json:"warehouse_id"`
		OwnerID        string `json:"owner_id"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return inventory.Scope{}, inventory.Invalid("invalid_work_order")
	}
	scope, err := p.Scope("document.create", input.OrganizationID, input.WarehouseID)
	if err != nil {
		return scope, err
	}
	if !scope.AllowsOwner(input.OwnerID) {
		return scope, inventory.Forbidden()
	}
	return scope, nil
}
func (s *Store) CommandWorkOrder(ctx context.Context, p inventory.Principal, command string, raw json.RawMessage, registry *modules.Registry) (json.RawMessage, error) {
	scope, err := orderScope(p, raw)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, translate(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err = tx.Exec(ctx, `SELECT set_config('lock_timeout','2s',true),set_config('statement_timeout','5s',true),platform.lock_module_configuration()`); err != nil {
		return nil, translate(err)
	}
	if err = validateRegistry(ctx, tx, registry); err != nil {
		return nil, err
	}
	var result []byte
	if err = tx.QueryRow(ctx, `SELECT warehouse.command_work_order($1::uuid,$2::uuid,$3::jsonb)`, command, scope.ActorID, raw).Scan(&result); err != nil {
		return nil, translate(err)
	}
	if err = tx.Commit(ctx); err != nil {
		if _, ok := errors.AsType[*pgconn.PgError](err); ok {
			return nil, translate(err)
		}
		return nil, &inventory.Problem{Status: 503, Code: "operation_interrupted", Detail: "The operation could not be confirmed. Retry with the same command identifier."}
	}
	return json.RawMessage(result), nil
}
func (s *HardenedStore) CommandWorkOrder(ctx context.Context, p inventory.Principal, command string, raw json.RawMessage, registry *modules.Registry) (json.RawMessage, error) {
	scope, err := orderScope(p, raw)
	if err != nil {
		return nil, err
	}
	if err = s.scope(scope); err != nil {
		return nil, err
	}
	var result []byte
	err = s.transaction(ctx, p.ActorID, scope.OrganizationID, scope.WarehouseID, "work_order_create|"+command+"|"+string(raw), func(tx pgx.Tx) error {
		if err := validateHardenedModules(ctx, tx, registry); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT hardened_api.work_order_command($1::uuid,$2)`, command, string(raw)).Scan(&result)
	})
	return json.RawMessage(result), err
}
func orderPage(ctx context.Context, scope inventory.Scope, f inventory.Filters, read func(context.Context, map[string]any) ([]json.RawMessage, error)) (inventory.Page, error) {
	page := inventory.Page{Data: []json.RawMessage{}}
	if f.Limit == 0 {
		f.Limit = 50
	}
	if f.Limit < 1 || f.Limit > 200 {
		return page, inventory.Invalid("invalid_limit")
	}
	if f.OwnerID != "" && !scope.AllowsOwner(f.OwnerID) {
		return page, inventory.Forbidden()
	}
	binding := cursorBinding(inventory.Resource("work-orders"), scope, f)
	input := map[string]any{"limit": f.Limit + 1}
	if f.OwnerID != "" {
		input["owner_id"] = f.OwnerID
	}
	if f.Cursor != "" {
		var c cursor
		raw, err := base64.RawURLEncoding.DecodeString(f.Cursor)
		if err != nil || len(raw) > 2048 || json.Unmarshal(raw, &c) != nil || c.Binding != binding {
			return page, inventory.Invalid("invalid_cursor")
		}
		if _, err = inventory.NormalizeID(c.ID); err != nil {
			return page, inventory.Invalid("invalid_cursor")
		}
		input["after"] = c.ID
	}
	var err error
	page.Data, err = read(ctx, input)
	if err != nil {
		return page, err
	}
	if len(page.Data) > f.Limit {
		page.Data = page.Data[:f.Limit]
		var last struct{ ID string }
		if err = json.Unmarshal(page.Data[len(page.Data)-1], &last); err != nil {
			return page, err
		}
		raw, _ := json.Marshal(cursor{ID: last.ID, Binding: binding})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}
func (s *Store) ListWorkOrders(ctx context.Context, scope inventory.Scope, f inventory.Filters) (inventory.Page, error) {
	return orderPage(ctx, scope, f, func(ctx context.Context, input map[string]any) ([]json.RawMessage, error) {
		raw, _ := json.Marshal(input)
		rows, err := s.pool.Query(ctx, `SELECT * FROM warehouse.read_work_orders($1::uuid,$2::uuid,$3::uuid,$4::jsonb)`, scope.ActorID, scope.OrganizationID, scope.WarehouseID, raw)
		if err != nil {
			return nil, translate(err)
		}
		defer rows.Close()
		result := []json.RawMessage{}
		for rows.Next() {
			var data []byte
			if err = rows.Scan(&data); err != nil {
				return nil, translate(err)
			}
			result = append(result, json.RawMessage(data))
		}
		return result, translate(rows.Err())
	})
}
func (s *HardenedStore) ListWorkOrders(ctx context.Context, scope inventory.Scope, f inventory.Filters) (inventory.Page, error) {
	if err := s.scope(scope); err != nil {
		return inventory.Page{}, err
	}
	return orderPage(ctx, scope, f, func(ctx context.Context, input map[string]any) ([]json.RawMessage, error) {
		raw, _ := json.Marshal(input)
		result := []json.RawMessage{}
		err := s.transaction(ctx, scope.ActorID, scope.OrganizationID, scope.WarehouseID, "work_orders|"+string(raw), func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT * FROM hardened_api.work_orders($1)`, string(raw))
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var data []byte
				if err = rows.Scan(&data); err != nil {
					return err
				}
				result = append(result, json.RawMessage(data))
			}
			return rows.Err()
		})
		return result, err
	})
}
