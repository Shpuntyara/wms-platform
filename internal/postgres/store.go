// Package postgres adapts published views and transactional kernel commands.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func New(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, errors.New("database pool required")
	}
	var privileged bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (
      SELECT 1 FROM pg_roles r
      WHERE (r.rolname=current_user OR pg_has_role(r.oid,'SET')) AND (
        r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls
        OR EXISTS (
          SELECT 1 FROM pg_namespace n
          WHERE n.nspname IN ('platform','catalog','warehouse','inventory','contracts_v1')
            AND has_schema_privilege(r.oid,n.oid,'CREATE')
        )
        OR EXISTS (
          SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
          WHERE n.nspname IN ('platform','catalog','warehouse','inventory','contracts_v1')
            AND c.relkind IN ('r','p','v','m','f')
            AND (c.relowner=r.oid
              OR has_table_privilege(r.oid,c.oid,'INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER')
              OR has_any_column_privilege(r.oid,c.oid,'INSERT,UPDATE'))
        )
      )
    )`).Scan(&privileged)
	if err != nil {
		return nil, fmt.Errorf("check application database role: %w", err)
	}
	if privileged {
		return nil, errors.New("unsafe database role: application must not own, write or administer core objects")
	}
	for _, view := range []string{"stock_balances", "documents", "document_lines", "reservations", "movements", "operations", "actor_grants", "active_modules", "module_migrations"} {
		if _, err := pool.Exec(ctx, "SELECT 1 FROM contracts_v1."+view+" LIMIT 0"); err != nil {
			return nil, fmt.Errorf("backend schema unavailable: %w", err)
		}
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) ResolvePrincipal(ctx context.Context, identity auth.Identity) (inventory.Principal, error) {
	rows, err := s.pool.Query(ctx, `SELECT actor_id::text, organization_id::text, warehouse_id::text,
      coalesce(owner_id::text,''),permission FROM contracts_v1.actor_grants WHERE issuer=$1 AND subject=$2
      ORDER BY organization_id,warehouse_id,permission,owner_id`, identity.Issuer, identity.Subject)
	if err != nil {
		return inventory.Principal{}, translate(err)
	}
	defer rows.Close()
	var p inventory.Principal
	for rows.Next() {
		var g inventory.Grant
		if err := rows.Scan(&p.ActorID, &g.OrganizationID, &g.WarehouseID, &g.OwnerID, &g.Permission); err != nil {
			return p, translate(err)
		}
		p.Grants = append(p.Grants, g)
	}
	if err := rows.Err(); err != nil {
		return p, translate(err)
	}
	if p.ActorID == "" {
		return p, inventory.Forbidden()
	}
	return p, nil
}

func (s *Store) InstalledModules(ctx context.Context) ([]modules.Manifest, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,version,schema_name::text,manifest FROM contracts_v1.active_modules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var installed []modules.Manifest
	for rows.Next() {
		var id, version, schema string
		var raw []byte
		var m modules.Manifest
		if err := rows.Scan(&id, &version, &schema, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, errors.New("invalid active module manifest")
		}
		if id != m.ID || version != m.Version || schema != "mod_"+strings.ReplaceAll(id, ".", "_") || schema != m.Data.Schema {
			return nil, errors.New("module registry metadata does not match manifest")
		}
		installed = append(installed, m)
	}
	return installed, rows.Err()
}

func (s *Store) ValidateModules(ctx context.Context, r *modules.Registry) error {
	for _, installation := range r.Installations() {
		if installation.SchemaVersion == 0 && len(installation.Migrations) == 0 {
			continue
		}
		if err := s.validateInstallation(ctx, installation); err != nil {
			return err
		}
	}
	for _, view := range r.FactViews() {
		parts := strings.Split(view, ".")
		if len(parts) != 2 {
			return errors.New("invalid module fact view")
		}
		if _, err := s.pool.Exec(ctx, "SELECT organization_id,warehouse_id,document_id,content_revision,payload FROM "+pgx.Identifier(parts).Sanitize()+" LIMIT 0"); err != nil {
			return fmt.Errorf("module fact view unavailable: %w", err)
		}
	}
	return nil
}

func (s *Store) validateInstallation(ctx context.Context, installation modules.Installation) error {
	rows, err := s.pool.Query(ctx, `SELECT migration_id,checksum FROM contracts_v1.module_migrations WHERE module_id=$1`, installation.ID)
	if err != nil {
		return fmt.Errorf("module migration history unavailable: %w", err)
	}
	defer rows.Close()
	seen := make(map[string]bool, len(installation.Migrations))
	for rows.Next() {
		var id, checksum string
		if err := rows.Scan(&id, &checksum); err != nil {
			return fmt.Errorf("module migration history unavailable: %w", err)
		}
		if expected, exists := installation.Migrations[id]; !exists || expected != checksum || seen[id] {
			return fmt.Errorf("module migration history mismatch: %q", installation.ID)
		}
		seen[id] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("module migration history unavailable: %w", err)
	}
	if len(seen) != len(installation.Migrations) {
		return fmt.Errorf("module migrations are incomplete: %q", installation.ID)
	}
	return nil
}

type readSpec struct{ view, projection string }

func spec(resource inventory.Resource) (readSpec, error) {
	switch resource {
	case inventory.StockResource:
		return readSpec{"stock_balances", `jsonb_build_object('id',id,'organization_id',organization_id,'warehouse_id',warehouse_id,
      'owner_id',owner_id,'item_id',item_id,'position_id',position_id,'stock_state',stock_state,'lot_id',lot_id,'serial_id',serial_id,
      'on_hand',on_hand::text,'reserved_qty',reserved_qty::text,'available_qty',available_qty::text,'version',version)`}, nil
	case inventory.ReservationResource:
		return readSpec{"reservations", `jsonb_build_object('id',id,'organization_id',organization_id,'warehouse_id',warehouse_id,
      'owner_id',owner_id,'item_id',item_id,'document_line_id',document_line_id,'balance_id',balance_id,'quantity',quantity::text,
      'consumed_qty',consumed_qty::text,'released_qty',released_qty::text,'active_qty',active_qty::text,'expires_at',expires_at,'created_at',created_at)`}, nil
	case inventory.MovementResource:
		return readSpec{"movements", `jsonb_build_object('id',id,'organization_id',organization_id,'warehouse_id',warehouse_id,
      'owner_id',owner_id,'item_id',item_id,'operation_id',operation_id,'document_line_id',document_line_id,'kind',kind,'reason',reason,
      'correction_of',correction_of,'created_at',created_at,'lines',lines)`}, nil
	case inventory.OperationResource:
		return readSpec{"operations", `jsonb_build_object('id',id,'status','completed','result',result,'completed_at',completed_at)`}, nil
	default:
		return readSpec{}, inventory.Missing()
	}
}

const scopeWhere = `organization_id=$1::text::uuid AND warehouse_id=$2::text::uuid AND ($3::boolean OR owner_id=ANY($4::text[]::uuid[]))`

func scopeArgs(s inventory.Scope) []any {
	return []any{s.OrganizationID, s.WarehouseID, s.AllOwners, s.OwnerIDs}
}
func optional(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type cursor struct {
	ID      string `json:"id"`
	Binding string `json:"binding"`
}

func cursorBinding(resource inventory.Resource, s inventory.Scope, f inventory.Filters) string {
	value, _ := json.Marshal([]any{resource, s.ActorID, s.OrganizationID, s.WarehouseID, s.AllOwners, s.OwnerIDs, f.OwnerID, f.ItemID, f.PositionID, f.DocumentLineID})
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func (s *Store) List(ctx context.Context, resource inventory.Resource, scope inventory.Scope, f inventory.Filters) (inventory.Page, error) {
	page := inventory.Page{Data: []json.RawMessage{}}
	if resource == inventory.OperationResource {
		return page, inventory.Missing()
	}
	definition, err := spec(resource)
	if err != nil {
		return page, err
	}
	if f.Limit == 0 {
		f.Limit = 50
	}
	if f.Limit < 1 || f.Limit > 200 {
		return page, inventory.Invalid("invalid_limit")
	}
	if f.OwnerID != "" && !scope.AllowsOwner(f.OwnerID) {
		return page, inventory.Forbidden()
	}
	if f.PositionID != "" && resource != inventory.StockResource || f.DocumentLineID != "" && resource == inventory.StockResource {
		return page, inventory.Invalid("unsupported_filter")
	}
	binding := cursorBinding(resource, scope, f)
	last := ""
	if f.Cursor != "" {
		var c cursor
		raw, err := base64.RawURLEncoding.DecodeString(f.Cursor)
		if err != nil || len(raw) > 2048 || json.Unmarshal(raw, &c) != nil || c.Binding != binding {
			return page, inventory.Invalid("invalid_cursor")
		}
		last, err = inventory.NormalizeID(c.ID)
		if err != nil {
			return page, inventory.Invalid("invalid_cursor")
		}
	}
	args := []any{scope.OrganizationID, scope.WarehouseID}
	var where strings.Builder
	where.WriteString("organization_id=$1::text::uuid AND warehouse_id=$2::text::uuid")
	// Separate query shapes keep owner and cursor bounds visible even when
	// PostgreSQL reuses a generic prepared plan. All values remain parameters.
	if !scope.AllOwners && f.OwnerID == "" {
		switch len(scope.OwnerIDs) {
		case 0:
			where.WriteString(" AND false")
		case 1:
			args = append(args, scope.OwnerIDs[0])
			where.WriteString(" AND owner_id=$3::text::uuid")
		default:
			args = append(args, scope.OwnerIDs)
			where.WriteString(" AND owner_id=ANY($3::text[]::uuid[])")
		}
	}
	for _, filter := range []struct{ column, value string }{{"owner_id", f.OwnerID}, {"item_id", f.ItemID}, {"position_id", f.PositionID}, {"document_line_id", f.DocumentLineID}} {
		if filter.value != "" {
			args = append(args, filter.value)
			where.WriteString(fmt.Sprintf(" AND %s=$%d::text::uuid", filter.column, len(args)))
		}
	}
	if last != "" {
		args = append(args, last)
		where.WriteString(fmt.Sprintf(" AND id>$%d::text::uuid", len(args)))
	}
	args = append(args, f.Limit+1)
	rows, err := s.pool.Query(ctx, "SELECT "+definition.projection+" FROM contracts_v1."+definition.view+" WHERE "+where.String()+fmt.Sprintf(" ORDER BY id LIMIT $%d", len(args)), args...)
	if err != nil {
		return page, translate(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return page, translate(err)
		}
		page.Data = append(page.Data, json.RawMessage(raw))
	}
	if err := rows.Err(); err != nil {
		return page, translate(err)
	}
	if len(page.Data) > f.Limit {
		page.Data = page.Data[:f.Limit]
		var tail struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(page.Data[len(page.Data)-1], &tail); err != nil {
			return page, err
		}
		raw, _ := json.Marshal(cursor{ID: tail.ID, Binding: binding})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

func (s *Store) Get(ctx context.Context, resource inventory.Resource, scope inventory.Scope, id string) (json.RawMessage, error) {
	definition, err := spec(resource)
	if err != nil {
		return nil, err
	}
	args := append(scopeArgs(scope), id)
	where := scopeWhere + " AND id=$5::text::uuid"
	if resource == inventory.OperationResource {
		args = append(args, scope.ActorID)
		where += " AND actor_id=$6::text::uuid"
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, "SELECT "+definition.projection+" FROM contracts_v1."+definition.view+" WHERE "+where, args...).Scan(&raw)
	if err != nil {
		return nil, translate(err)
	}
	return json.RawMessage(raw), nil
}

func (s *Store) Execute(ctx context.Context, p inventory.Principal, c inventory.Command, registry *modules.Registry) (json.RawMessage, error) {
	if registry == nil {
		return nil, &modules.Unavailable{}
	}
	scope, err := p.Scope(c.Permission(), c.OrganizationID, c.WarehouseID)
	if err != nil {
		return nil, err
	}
	if err := inventory.ValidateQuantity(c.Quantity); err != nil {
		return nil, err
	}
	for attempt := range 3 {
		result, err := s.execute(ctx, p, scope, c, registry)
		if err == nil {
			return result, nil
		}
		var pgerr *pgconn.PgError
		if attempt == 2 || !errors.As(err, &pgerr) || (pgerr.Code != "40P01" && pgerr.Code != "40001") {
			return nil, translate(err)
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, translate(ctx.Err())
		case <-timer.C:
		}
	}
	return nil, errors.New("transaction retry exhausted")
}

func (s *Store) execute(ctx context.Context, p inventory.Principal, scope inventory.Scope, c inventory.Command, registry *modules.Registry) (json.RawMessage, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout','2s',true),set_config('statement_timeout','5s',true)`); err != nil {
		return nil, err
	}
	// Resolve authorization before touching any operation or taking core locks.
	if c.Kind == "release" {
		var line string
		err := tx.QueryRow(ctx, "SELECT document_line_id::text FROM contracts_v1.reservations WHERE "+scopeWhere+" AND id=$5::text::uuid", append(scopeArgs(scope), c.ReservationID)...).Scan(&line)
		if err != nil {
			return nil, err
		}
		c.DocumentLineID = line
	}
	var documentID string
	err = tx.QueryRow(ctx, "SELECT document_id::text FROM contracts_v1.document_lines WHERE "+scopeWhere+" AND id=$5::text::uuid", append(scopeArgs(scope), c.DocumentLineID)...).Scan(&documentID)
	if err != nil {
		return nil, err
	}
	for _, balance := range []string{c.BalanceID, c.SourceBalanceID, c.TargetBalanceID} {
		if balance == "" {
			continue
		}
		var found string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM contracts_v1.stock_balances WHERE "+scopeWhere+" AND id=$5::text::uuid", append(scopeArgs(scope), balance)...).Scan(&found); err != nil {
			return nil, err
		}
	}
	if c.ReservationID != "" && c.Kind != "release" {
		var found string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM contracts_v1.reservations WHERE "+scopeWhere+" AND id=$5::text::uuid", append(scopeArgs(scope), c.ReservationID)...).Scan(&found); err != nil {
			return nil, err
		}
	}
	if c.CorrectionOf != "" {
		var found string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM contracts_v1.movements WHERE "+scopeWhere+" AND id=$5::text::uuid", append(scopeArgs(scope), c.CorrectionOf)...).Scan(&found); err != nil {
			return nil, err
		}
	}
	prepare, post, args, point, err := commandSQL(p, c)
	if err != nil {
		return nil, err
	}
	var preparedBytes []byte
	if err := tx.QueryRow(ctx, prepare, args...).Scan(&preparedBytes); err != nil {
		return nil, err
	}
	var prepared struct {
		Replayed bool             `json:"replayed"`
		Result   json.RawMessage  `json:"result"`
		Document modules.Document `json:"document"`
	}
	if err := json.Unmarshal(preparedBytes, &prepared); err != nil {
		return nil, err
	}
	var result json.RawMessage
	if prepared.Replayed {
		result = prepared.Result
	} else {
		check := modules.Check{Document: prepared.Document, ActorID: p.ActorID, CommandID: c.ID, Kind: c.Kind, MovementKind: c.MovementKind, Quantity: c.Quantity}
		load := func(ctx context.Context, view string, doc modules.Document) (json.RawMessage, error) {
			return readModuleFact(ctx, tx, view, doc)
		}
		if err := registry.Check(ctx, point, check, load); err != nil {
			return nil, err
		}
		if c.Kind == "movement" && c.MovementKind == "issue" {
			if err := registry.Check(ctx, modules.DispatchPoint, check, load); err != nil {
				return nil, err
			}
		}
		var raw []byte
		if err := tx.QueryRow(ctx, post, args...).Scan(&raw); err != nil {
			return nil, err
		}
		result = json.RawMessage(raw)
	}
	if err := tx.Commit(ctx); err != nil {
		if _, ok := errors.AsType[*pgconn.PgError](err); ok {
			return nil, err
		}
		// A transport/client failure cannot tell us whether COMMIT took effect.
		return nil, &inventory.Problem{Status: 503, Code: "operation_interrupted", Detail: "The operation could not be confirmed. Retry with the same command identifier."}
	}
	return result, nil
}

func readModuleFact(ctx context.Context, tx pgx.Tx, view string, doc modules.Document) (json.RawMessage, error) {
	parts := strings.Split(view, ".")
	if len(parts) != 2 {
		return nil, errors.New("invalid module fact view")
	}
	rows, err := tx.Query(ctx, "SELECT payload FROM "+pgx.Identifier(parts).Sanitize()+" WHERE organization_id=$1::text::uuid AND warehouse_id=$2::text::uuid AND document_id=$3::text::uuid AND content_revision=$4 LIMIT 2", doc.OrganizationID, doc.WarehouseID, doc.ID, doc.ContentRevision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var raw []byte
	if err := rows.Scan(&raw); err != nil {
		return nil, err
	}
	if rows.Next() {
		return nil, errors.New("ambiguous module facts")
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

func commandSQL(p inventory.Principal, c inventory.Command) (string, string, []any, string, error) {
	switch c.Kind {
	case "reserve":
		args := []any{c.ID, p.ActorID, c.DocumentLineID, c.ExpectedDocumentVersion, c.BalanceID, c.Quantity, c.ExpiresAt}
		suffix := `($1::text::uuid,$2::text::uuid,$3::text::uuid,$4::bigint,$5::text::uuid,$6::text::numeric,$7::timestamptz)`
		return "SELECT inventory.prepare_reservation" + suffix, "SELECT inventory.reserve_stock" + suffix, args, modules.ReservationPoint, nil
	case "release":
		args := []any{c.ID, p.ActorID, c.ReservationID, c.ExpectedDocumentVersion, c.Quantity, c.Reason}
		suffix := `($1::text::uuid,$2::text::uuid,$3::text::uuid,$4::bigint,$5::text::numeric,$6::text)`
		return "SELECT inventory.prepare_release" + suffix, "SELECT inventory.release_reservation" + suffix, args, modules.ReleasePoint, nil
	case "movement":
		args := []any{c.ID, p.ActorID, c.DocumentLineID, c.ExpectedDocumentVersion, c.MovementKind, optional(c.SourceBalanceID), optional(c.TargetBalanceID), c.Quantity, optional(c.ReservationID), optional(c.Reason), optional(c.CorrectionOf)}
		suffix := `($1::text::uuid,$2::text::uuid,$3::text::uuid,$4::bigint,$5::text,$6::text::uuid,$7::text::uuid,$8::text::numeric,$9::text::uuid,$10::text,$11::text::uuid)`
		return "SELECT inventory.prepare_movement" + suffix, "SELECT inventory.post_movement" + suffix, args, modules.MovementPoint, nil
	default:
		return "", "", nil, "", inventory.Invalid("invalid_command")
	}
}

func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return inventory.Missing()
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &inventory.Problem{Status: 503, Code: "operation_interrupted", Detail: "The operation could not be confirmed. Retry with the same command identifier."}
	}
	var networkError net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) || errors.Is(err, pgconn.ErrConnClosed) || errors.Is(err, pgx.ErrTxCommitRollback) || errors.As(err, &networkError) {
		return &inventory.Problem{Status: 503, Code: "operation_interrupted", Detail: "The operation could not be confirmed. Retry with the same command identifier."}
	}
	if _, ok := errors.AsType[*pgconn.ConnectError](err); ok {
		return &inventory.Problem{Status: 503, Code: "database_unavailable", Detail: "The database is temporarily unavailable. Retry with the same command identifier."}
	}
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) {
		return err
	}
	if strings.HasPrefix(pgerr.Code, "08") {
		return &inventory.Problem{Status: 503, Code: "operation_interrupted", Detail: "The operation could not be confirmed. Retry with the same command identifier."}
	}
	switch pgerr.Code {
	case "P0002":
		return inventory.Missing()
	case "22023":
		if pgerr.Message == "idempotency_key_reused" {
			return &inventory.Problem{Status: 409, Code: "idempotency_key_reused", Detail: "The command identifier is already bound to different input."}
		}
		if pgerr.Message == "invalid_quantity" {
			return inventory.Invalid("invalid_quantity")
		}
		return inventory.Invalid("invalid_value")
	case "22P02", "22003", "22007", "22008":
		return inventory.Invalid("invalid_value")
	case "P0001":
		allowed := map[string]bool{"document_version_conflict": true, "document_not_active": true, "insufficient_available_stock": true, "insufficient_active_reservation": true, "reservation_mismatch": true, "reservation_expired": true, "reservation_expiry_in_past": true, "stock_scope_mismatch": true, "stock_state_not_allocatable": true, "target_state_not_allocatable": true, "stock_state_not_shippable": true, "document_kind_mismatch": true, "invalid_movement_shape": true, "invalid_movement_kind": true, "adjustment_reason_required": true, "release_reason_required": true, "reservation_requires_shipment": true, "correction_requires_adjustment": true, "correction_scope_mismatch": true, "transfer_tracking_mismatch": true}
		if allowed[pgerr.Message] {
			return &inventory.Problem{Status: 409, Code: pgerr.Message, Detail: "The operation conflicts with the current inventory or document state."}
		}
	case "23514", "23505", "23503":
		return &inventory.Problem{Status: 409, Code: "inventory_constraint_conflict", Detail: "The operation conflicts with inventory constraints."}
	case "40P01", "40001", "55P03", "57014", "57P01", "53300", "42501":
		return &inventory.Problem{Status: 503, Code: "database_busy", Detail: "The operation could not be confirmed. Retry with the same command identifier."}
	}
	return errors.New("unexpected database error")
}
