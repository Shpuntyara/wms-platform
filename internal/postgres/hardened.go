package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const contextProbeActor = "00000000-0000-0000-0000-000000000000"

type hardenedProfile struct {
	Profile        string `json:"profile"`
	Login          string `json:"login"`
	InstallationID string `json:"installation_id"`
}

// HardenedStore implements api.Backend for one dedicated onprem-v1 installation.
// key is a separate 32-byte request-context secret, never the SQL password or
// outbox relay key. Caller identities must come from verified OIDC middleware.
type HardenedStore struct {
	pool    *pgxpool.Pool
	profile hardenedProfile
	key     [32]byte
}

func loadHardenedProfile(ctx context.Context, pool *pgxpool.Pool, kind string) (hardenedProfile, error) {
	var p hardenedProfile
	if pool == nil {
		return p, errors.New("database pool required")
	}
	if err := requireDurablePostgres(ctx, pool); err != nil {
		return p, err
	}
	var raw []byte
	if err := pool.QueryRow(ctx, "SELECT hardened_"+kind+".profile()").Scan(&raw); err != nil {
		return p, fmt.Errorf("hardened profile preflight: %w", err)
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Profile != "onprem-v1" || p.Login == "" || strings.Contains(p.Login, "|") {
		return p, errors.New("onprem-v1 profile required; apply the forward installation profile")
	}
	if _, err := inventory.NormalizeID(p.InstallationID); err != nil {
		return p, errors.New("invalid on-premise installation identifier")
	}
	return p, nil
}

func NewHardened(ctx context.Context, pool *pgxpool.Pool, key []byte) (*HardenedStore, error) {
	if len(key) != 32 {
		return nil, errors.New("hardened request key must contain exactly 32 bytes")
	}
	p, err := loadHardenedProfile(ctx, pool, "api")
	if err != nil {
		return nil, err
	}
	s := &HardenedStore{pool: pool, profile: p}
	copy(s.key[:], key)
	// A signed empty lookup proves the app key matches the owner-provisioned key.
	if err := s.verifyContext(ctx); err != nil {
		return nil, fmt.Errorf("hardened signing key preflight: %w", err)
	}
	return s, nil
}

func (s *HardenedStore) verifyContext(ctx context.Context) error {
	return s.transaction(ctx, contextProbeActor, contextProbeActor, contextProbeActor, `resolve|["wms:startup","wms:startup"]`, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT * FROM hardened_api.resolve($1)`, `["wms:startup","wms:startup"]`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
		}
		return rows.Err()
	})
}

func (s *HardenedStore) RuntimeLogin() string   { return s.profile.Login }
func (s *HardenedStore) InstallationID() string { return s.profile.InstallationID }
func (s *HardenedStore) Ping(ctx context.Context) error {
	current, err := loadHardenedProfile(ctx, s.pool, "api")
	if err != nil {
		return err
	}
	if current.InstallationID != s.profile.InstallationID || current.Login != s.profile.Login {
		return errors.New("on-premise installation identity changed")
	}
	return s.verifyContext(ctx)
}

func (s *HardenedStore) transaction(ctx context.Context, actor, organization, warehouse, action string, fn func(pgx.Tx) error) error {
	tx, err := beginDurableTransaction(ctx, s.pool)
	if err != nil {
		return translate(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var pid int
	var xid string
	err = tx.QueryRow(ctx, `SELECT pg_backend_pid(),pg_current_xact_id()::text`).Scan(&pid, &xid)
	if err != nil {
		return translate(err)
	}
	digest := sha256.Sum256([]byte(action))
	payload := fmt.Sprintf("2|%s|%s|%d|%s|%s|%s|%s|%d|%x", s.profile.InstallationID, s.profile.Login, pid, xid, actor,
		organization, warehouse, time.Now().Unix()+20, digest)
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write([]byte(payload))
	_, err = tx.Exec(ctx, `SELECT set_config('wms.context',$1,true),set_config('wms.context_mac',$2,true),
	 set_config('lock_timeout','2s',true),set_config('statement_timeout','5s',true)`, payload, hex.EncodeToString(mac.Sum(nil)))
	if err != nil {
		return translate(err)
	}
	if err = fn(tx); err != nil {
		return translate(err)
	}
	if err = tx.Commit(ctx); err != nil {
		if _, known := errors.AsType[*pgconn.PgError](err); known {
			return translate(err)
		}
		return &inventory.Problem{Status: 503, Code: "operation_interrupted", Detail: "The operation could not be confirmed. Retry with the same command identifier."}
	}
	return nil
}

func (s *HardenedStore) ResolvePrincipal(ctx context.Context, identity auth.Identity) (inventory.Principal, error) {
	raw, _ := json.Marshal([]string{identity.Issuer, identity.Subject})
	var p inventory.Principal
	err := s.transaction(ctx, contextProbeActor, contextProbeActor, contextProbeActor, "resolve|"+string(raw), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT * FROM hardened_api.resolve($1)`, string(raw))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var data []byte
			if err := rows.Scan(&data); err != nil {
				return err
			}
			var g struct {
				ActorID        string `json:"actor_id"`
				OrganizationID string `json:"organization_id"`
				WarehouseID    string `json:"warehouse_id"`
				OwnerID        string `json:"owner_id"`
				Permission     string `json:"permission"`
			}
			if err := json.Unmarshal(data, &g); err != nil {
				return err
			}
			p.ActorID = g.ActorID
			p.Grants = append(p.Grants, inventory.Grant{OrganizationID: g.OrganizationID, WarehouseID: g.WarehouseID, OwnerID: g.OwnerID, Permission: g.Permission})
		}
		return rows.Err()
	})
	if err == nil && p.ActorID == "" {
		err = inventory.Forbidden()
	}
	return p, err
}

type hardenedModule struct {
	Manifest   modules.Manifest  `json:"manifest"`
	State      string            `json:"state"`
	Schema     string            `json:"schema"`
	Migrations map[string]string `json:"migrations"`
}

func hardenedModules(ctx context.Context, q moduleQuerier) ([]hardenedModule, error) {
	rows, err := q.Query(ctx, `SELECT * FROM hardened_api.module_metadata()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []hardenedModule
	for rows.Next() {
		var raw []byte
		var m hardenedModule
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		if m.State != "active" || m.Schema != "mod_"+strings.ReplaceAll(m.Manifest.ID, ".", "_") {
			return nil, &modules.Unavailable{}
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

func (s *HardenedStore) InstalledModules(ctx context.Context) ([]modules.Manifest, error) {
	metadata, err := hardenedModules(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	result := make([]modules.Manifest, 0, len(metadata))
	for _, m := range metadata {
		result = append(result, m.Manifest)
	}
	return result, nil
}

func validateHardenedModules(ctx context.Context, q moduleQuerier, registry *modules.Registry) error {
	if registry == nil {
		return &modules.Unavailable{}
	}
	metadata, err := hardenedModules(ctx, q)
	if err != nil {
		return err
	}
	if len(metadata) != len(registry.Installations()) {
		return &modules.Unavailable{}
	}
	for _, m := range metadata {
		if !registry.AcceptsManifest(m.Manifest) {
			return &modules.Unavailable{}
		}
		for _, installation := range registry.Installations() {
			if installation.ID == m.Manifest.ID && !maps.Equal(installation.Migrations, m.Migrations) {
				return &modules.Unavailable{}
			}
		}
	}
	return nil
}

func (s *HardenedStore) ValidateModules(ctx context.Context, registry *modules.Registry) error {
	return validateHardenedModules(ctx, s.pool, registry)
}

func (s *HardenedStore) scope(scope inventory.Scope) error {
	for _, value := range []string{scope.ActorID, scope.OrganizationID, scope.WarehouseID} {
		if _, err := inventory.NormalizeID(value); err != nil {
			return inventory.Forbidden()
		}
	}
	return nil
}

func (s *HardenedStore) read(ctx context.Context, resource inventory.Resource, scope inventory.Scope, input map[string]any) ([]json.RawMessage, error) {
	if err := s.scope(scope); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(input)
	result := []json.RawMessage{}
	err := s.transaction(ctx, scope.ActorID, scope.OrganizationID, scope.WarehouseID, "read|"+string(resource)+"|"+string(raw), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT * FROM hardened_api.read($1,$2)`, string(resource), string(raw))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var data []byte
			if err := rows.Scan(&data); err != nil {
				return err
			}
			result = append(result, json.RawMessage(data))
		}
		return rows.Err()
	})
	return result, err
}

func (s *HardenedStore) List(ctx context.Context, resource inventory.Resource, scope inventory.Scope, f inventory.Filters) (inventory.Page, error) {
	page := inventory.Page{Data: []json.RawMessage{}}
	if resource == inventory.OperationResource {
		return page, inventory.Missing()
	}
	if _, err := spec(resource); err != nil {
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
	binding := cursorBinding(resource, scope, f)
	input := map[string]any{"limit": f.Limit + 1}
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
	for _, filter := range []struct{ key, value string }{{"owner_id", f.OwnerID}, {"item_id", f.ItemID}, {"position_id", f.PositionID}, {"document_line_id", f.DocumentLineID}} {
		if filter.value != "" {
			input[filter.key] = filter.value
		}
	}
	var err error
	page.Data, err = s.read(ctx, resource, scope, input)
	if err != nil {
		return page, err
	}
	if len(page.Data) > f.Limit {
		page.Data = page.Data[:f.Limit]
		var tail struct{ ID string }
		if err := json.Unmarshal(page.Data[len(page.Data)-1], &tail); err != nil {
			return page, err
		}
		raw, _ := json.Marshal(cursor{ID: tail.ID, Binding: binding})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

func (s *HardenedStore) Get(ctx context.Context, resource inventory.Resource, scope inventory.Scope, id string) (json.RawMessage, error) {
	rows, err := s.read(ctx, resource, scope, map[string]any{"id": id, "limit": 1})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, inventory.Missing()
	}
	return rows[0], nil
}

func (s *HardenedStore) command(ctx context.Context, actor, organization, warehouse, command, kind string, input any, registry *modules.Registry) (json.RawMessage, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var result []byte
	err = s.transaction(ctx, actor, organization, warehouse, "command|"+command+"|"+kind+"|"+string(raw), func(tx pgx.Tx) error {
		if err := validateHardenedModules(ctx, tx, registry); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT hardened_api.command($1::uuid,$2,$3)`, command, kind, string(raw)).Scan(&result)
	})
	return json.RawMessage(result), err
}

func (s *HardenedStore) Execute(ctx context.Context, p inventory.Principal, c inventory.Command, registry *modules.Registry) (json.RawMessage, error) {
	scope, err := p.Scope(c.Permission(), c.OrganizationID, c.WarehouseID)
	if err != nil {
		return nil, err
	}
	if err := s.scope(scope); err != nil {
		return nil, err
	}
	if err := inventory.ValidateQuantity(c.Quantity); err != nil {
		return nil, err
	}
	input := map[string]any{"line": optional(c.DocumentLineID), "balance": optional(c.BalanceID), "expected_version": c.ExpectedDocumentVersion,
		"quantity": c.Quantity, "expires_at": c.ExpiresAt, "reservation": optional(c.ReservationID), "reason": optional(c.Reason),
		"kind": optional(c.MovementKind), "source": optional(c.SourceBalanceID), "target": optional(c.TargetBalanceID), "correction_of": optional(c.CorrectionOf)}
	return s.command(ctx, p.ActorID, c.OrganizationID, c.WarehouseID, c.ID, c.Kind, input, registry)
}

func (s *HardenedStore) CommandDocument(ctx context.Context, p inventory.Principal, command, document string, input inventory.DocumentInput, registry *modules.Registry) (json.RawMessage, error) {
	permission := "document.create"
	if document != "" {
		permission = "document.transition"
	}
	scope, err := p.Scope(permission, input.OrganizationID, input.WarehouseID)
	if err != nil {
		return nil, err
	}
	if err := s.scope(scope); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(input)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	body["document_id"], _ = json.Marshal(optional(document))
	return s.command(ctx, p.ActorID, input.OrganizationID, input.WarehouseID, command, "document", body, registry)
}
