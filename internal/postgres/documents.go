package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s *Store) CommandDocument(ctx context.Context, p inventory.Principal, command, document string, input inventory.DocumentInput, registry *modules.Registry) (json.RawMessage, error) {
	var scope inventory.Scope
	var err error
	if document != "" {
		scope, err = p.Scope("document.transition", input.OrganizationID, input.WarehouseID)
	} else {
		scope, err = input.CreationScope(p)
	}
	if err != nil {
		return nil, err
	}
	if document == "" && !scope.AllowsOwner(input.OwnerID) {
		return nil, inventory.Forbidden()
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
	if _, err = tx.Exec(ctx, `SELECT set_config('lock_timeout','2s',true),set_config('statement_timeout','5s',true)`); err != nil {
		return nil, translate(err)
	}
	if document != "" {
		var found string
		if err = tx.QueryRow(ctx, `SELECT id::text FROM contracts_v1.documents WHERE `+scopeWhere+` AND id=$5::text::uuid`, append(scopeArgs(scope), document)...).Scan(&found); err != nil {
			return nil, translate(err)
		}
	}
	if _, err = tx.Exec(ctx, `SELECT platform.lock_module_configuration()`); err != nil {
		return nil, translate(err)
	}
	if err = validateRegistry(ctx, tx, registry); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var result []byte
	err = tx.QueryRow(ctx, `SELECT warehouse.command_document($1::text::uuid,$2::text::uuid,$3::text::uuid,$4::jsonb)`, command, p.ActorID, optional(document), raw).Scan(&result)
	if err != nil {
		return nil, translate(err)
	}
	if err = tx.Commit(ctx); err != nil {
		if _, ok := errors.AsType[*pgconn.PgError](err); ok {
			return nil, translate(err)
		}
		// A client/transport error cannot establish whether COMMIT took effect.
		return nil, &inventory.Problem{Status: 503, Code: "operation_interrupted", Detail: "The operation could not be confirmed. Retry with the same command identifier."}
	}
	return json.RawMessage(result), nil
}
