// Package modules hosts trusted compiled guards and bounded declarative policies.
// Compiled extensions share the host process and are not sandboxed.
package modules

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	CoreVersion      = "0.6.0"
	SDKVersion       = "2.0.0"
	ReservationPoint = "inventory.before_reservation.v1"
	ReleasePoint     = "inventory.before_release.v1"
	MovementPoint    = "inventory.before_movement.v1"
	DispatchPoint    = "outbound.shipment_dispatch_guard.v1"
)

type Compatibility struct {
	Core string `json:"core"`
	SDK  string `json:"sdk"`
}
type Dependency struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}
type GuardSpec struct {
	Point         string `json:"point"`
	Handler       string `json:"handler"`
	Required      bool   `json:"required"`
	FailurePolicy string `json:"failure_policy"`
	TimeoutMS     int    `json:"timeout_ms"`
}
type ManifestData struct {
	Schema              string `json:"schema"`
	TargetSchemaVersion int    `json:"target_schema_version"`
}
type ManifestMigration struct {
	ID       string `json:"id"`
	Checksum string `json:"checksum,omitempty"`
}

// Manifest is the executable projection of the full package manifest.
type Manifest struct {
	ManifestVersion      int                 `json:"manifest_version"`
	ID                   string              `json:"id"`
	Version              string              `json:"version"`
	Compatibility        Compatibility       `json:"compatibility"`
	RequiresCapabilities []string            `json:"requires_capabilities"`
	RequiresModules      []Dependency        `json:"requires_modules"`
	ConflictsWith        []string            `json:"conflicts_with"`
	Guards               []GuardSpec         `json:"guards"`
	Runtime              string              `json:"runtime"`
	Policies             []json.RawMessage   `json:"policies"`
	Subscribers          []json.RawMessage   `json:"subscribers"`
	Data                 ManifestData        `json:"data"`
	Migrations           []ManifestMigration `json:"migrations"`
}
type Document struct {
	ID              string `json:"id"`
	OrganizationID  string `json:"organization_id"`
	WarehouseID     string `json:"warehouse_id"`
	OwnerID         string `json:"owner_id"`
	Kind            string `json:"kind"`
	Version         int64  `json:"version"`
	ContentRevision int64  `json:"content_revision"`
}
type Check struct {
	Document                                         Document
	ActorID, CommandID, Kind, MovementKind, Quantity string
	Facts                                            json.RawMessage
}
type Guard func(context.Context, Check) error
type FactLoader func(context.Context, string, Document) (json.RawMessage, error)
type Module struct {
	ID, Version, FactView string
	Guards                map[string]Guard
	GuardPoints           map[string][]string
	SchemaVersion         int
	MigrationChecksums    map[string]string
}
type Installation struct {
	ID, Schema    string
	SchemaVersion int
	Migrations    map[string]string
}
type Denial struct{ ModuleID, Code string }

func (e *Denial) Error() string { return e.Code }

type Unavailable struct{ ModuleID string }

func (e *Unavailable) Error() string { return "required_guard_unavailable" }

type registeredGuard struct {
	module, view string
	timeout      time.Duration
	check        Guard
}
type Registry struct {
	guards        map[string][]registeredGuard
	factViews     []string
	installations []Installation
	manifests     map[string]Manifest
}

var moduleID = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var viewName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var migrationID = regexp.MustCompile(`^[0-9]{3}_[a-z0-9_]+$`)
var checksum = regexp.MustCompile(`^[0-9a-f]{64}$`)

func canonicalSchema(id string) string { return "mod_" + strings.ReplaceAll(id, ".", "_") }

func validModuleID(id string) bool { return len(id) <= 58 && moduleID.MatchString(id) }

func knownPoint(point string) bool {
	return point == ReservationPoint || point == ReleasePoint || point == MovementPoint || point == DispatchPoint
}

func guardBindings(code Module) (map[string]bool, error) {
	bindings := map[string]bool{}
	if len(code.Guards) != len(code.GuardPoints) {
		return nil, errors.New("compiled guards require explicit point bindings")
	}
	for handler, points := range code.GuardPoints {
		if code.Guards[handler] == nil || len(points) == 0 {
			return nil, errors.New("invalid compiled guard binding")
		}
		for _, point := range points {
			key := point + "/" + handler
			if !knownPoint(point) || bindings[key] {
				return nil, errors.New("unknown or duplicate compiled guard binding")
			}
			bindings[key] = true
		}
	}
	return bindings, nil
}

func validateStorage(m Manifest, code Module) error {
	declared := m.Data.Schema != "" || m.Data.TargetSchemaVersion != 0 || code.SchemaVersion != 0 || len(m.Migrations) > 0 || len(code.MigrationChecksums) > 0
	if declared && (code.SchemaVersion < 1 || m.Data.TargetSchemaVersion != code.SchemaVersion || m.Data.Schema != canonicalSchema(m.ID)) {
		return fmt.Errorf("module storage schema does not match compiled package: %q", m.ID)
	}
	if len(m.Migrations) != len(code.MigrationChecksums) {
		return fmt.Errorf("module migrations do not match compiled package: %q", m.ID)
	}
	seen := map[string]bool{}
	for _, migration := range m.Migrations {
		if seen[migration.ID] || !migrationID.MatchString(migration.ID) || !checksum.MatchString(code.MigrationChecksums[migration.ID]) {
			return fmt.Errorf("invalid module migration metadata: %q", m.ID)
		}
		seen[migration.ID] = true
	}
	return nil
}

func inRange(version, rule string) bool {
	parts := strings.Fields(rule)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], ">=") || !strings.HasPrefix(parts[1], "<") {
		return false
	}
	low, high := strings.TrimPrefix(parts[0], ">="), strings.TrimPrefix(parts[1], "<")
	if !stableVersion.MatchString(version) || !stableVersion.MatchString(low) || !stableVersion.MatchString(high) {
		return false
	}
	return semver.Compare("v"+low, "v"+high) < 0 && semver.Compare("v"+version, "v"+low) >= 0 && semver.Compare("v"+version, "v"+high) < 0
}

func NewRegistry(installed []Manifest, compiled []Module, required []string) (*Registry, error) {
	for _, m := range installed {
		if m.Runtime == "declarative_v1" || m.Runtime == "declarative_v2" {
			build := declarativeModule
			if m.Runtime == "declarative_v2" {
				build = declarativeV2
			}
			code, err := build(m)
			if err != nil {
				return nil, err
			}
			compiled = append(compiled, code)
		}
	}
	r := &Registry{guards: make(map[string][]registeredGuard), manifests: make(map[string]Manifest)}
	codes := make(map[string]Module)
	bindings := make(map[string]map[string]bool)
	schemas := map[string]string{}
	manifests := make(map[string]Manifest)
	for _, code := range compiled {
		if _, exists := codes[code.ID]; exists {
			return nil, fmt.Errorf("duplicate compiled module %q", code.ID)
		}
		if !validModuleID(code.ID) || !stableVersion.MatchString(code.Version) || code.SchemaVersion < 0 {
			return nil, errors.New("invalid compiled module identity")
		}
		schema := canonicalSchema(code.ID)
		if previous, exists := schemas[schema]; exists {
			return nil, fmt.Errorf("module schema collision between %q and %q", previous, code.ID)
		}
		schemas[schema] = code.ID
		if code.FactView != "" {
			parts := strings.Split(code.FactView, ".")
			if len(parts) != 2 || parts[0] != schema || len(parts[1]) > 63 || !viewName.MatchString(parts[1]) {
				return nil, errors.New("module fact view must belong to its own schema")
			}
		}
		bound, err := guardBindings(code)
		if err != nil {
			return nil, fmt.Errorf("compiled module %q: %w", code.ID, err)
		}
		bindings[code.ID] = bound
		for id, digest := range code.MigrationChecksums {
			if code.SchemaVersion < 1 || !migrationID.MatchString(id) || !checksum.MatchString(digest) {
				return nil, fmt.Errorf("invalid compiled migration metadata: %q", code.ID)
			}
		}
		codes[code.ID] = code
	}
	capabilities := map[string]bool{ReservationPoint: true, ReleasePoint: true, MovementPoint: true, DispatchPoint: true, "platform.aggregate_transaction.v1": true, "platform.outbox.v1": true}
	for _, m := range installed {
		if (m.Runtime != "declarative_v1" && m.Runtime != "declarative_v2" && ((m.Runtime != "" && m.Runtime != "in_process_compiled") || len(m.Policies) > 0)) || len(m.Subscribers) > 0 {
			return nil, fmt.Errorf("unsupported executable extension in module %q", m.ID)
		}
		if _, exists := manifests[m.ID]; exists {
			return nil, fmt.Errorf("duplicate installed module %q", m.ID)
		}
		if m.ManifestVersion != 1 || !validModuleID(m.ID) || !stableVersion.MatchString(m.Version) || !inRange(CoreVersion, m.Compatibility.Core) || !inRange(SDKVersion, m.Compatibility.SDK) {
			return nil, fmt.Errorf("incompatible module %q", m.ID)
		}
		code, exists := codes[m.ID]
		if !exists || code.Version != m.Version {
			return nil, fmt.Errorf("compiled module missing or version mismatch: %q", m.ID)
		}
		if err := validateStorage(m, code); err != nil {
			return nil, err
		}
		for _, capability := range m.RequiresCapabilities {
			if !capabilities[capability] {
				return nil, fmt.Errorf("unsupported module capability %q", capability)
			}
		}
		manifests[m.ID] = m
		r.manifests[m.ID] = m
	}
	for _, id := range required {
		if _, exists := manifests[id]; !exists {
			return nil, fmt.Errorf("required module is not active: %q", id)
		}
	}
	// Dependency traversal rejects cycles and produces a deterministic order.
	state := map[string]int{}
	var order []string
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return errors.New("module dependency cycle")
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		m := manifests[id]
		for _, conflict := range m.ConflictsWith {
			if _, exists := manifests[conflict]; exists {
				return fmt.Errorf("conflicting module %q", conflict)
			}
		}
		for _, dep := range m.RequiresModules {
			other, exists := manifests[dep.ID]
			if !exists || !inRange(other.Version, dep.Version) {
				return fmt.Errorf("unsatisfied module dependency %q", dep.ID)
			}
			if err := visit(dep.ID); err != nil {
				return err
			}
		}
		state[id] = 2
		order = append(order, id)
		return nil
	}
	ids := make([]string, 0, len(manifests))
	for id := range manifests {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	for _, id := range order {
		m, code := manifests[id], codes[id]
		seen := map[string]bool{}
		for _, spec := range m.Guards {
			if !knownPoint(spec.Point) {
				return nil, fmt.Errorf("unknown guard point %q", spec.Point)
			}
			fn := code.Guards[spec.Handler]
			key := spec.Point + "/" + spec.Handler
			if fn == nil || !bindings[id][key] || !spec.Required || spec.FailurePolicy != "deny" || spec.TimeoutMS < 1 || spec.TimeoutMS > 1000 || seen[key] {
				return nil, fmt.Errorf("invalid required guard in %q", id)
			}
			seen[key] = true
			r.guards[spec.Point] = append(r.guards[spec.Point], registeredGuard{module: id, view: code.FactView, timeout: time.Duration(spec.TimeoutMS) * time.Millisecond, check: fn})
		}
		if len(seen) != len(bindings[id]) {
			return nil, fmt.Errorf("compiled guard binding omitted from active manifest: %q", id)
		}
		if code.FactView != "" {
			r.factViews = append(r.factViews, code.FactView)
		}
		r.installations = append(r.installations, Installation{ID: id, Schema: canonicalSchema(id), SchemaVersion: code.SchemaVersion, Migrations: maps.Clone(code.MigrationChecksums)})
	}
	return r, nil
}

func (r *Registry) FactViews() []string { return slices.Clone(r.factViews) }

func (r *Registry) Installations() []Installation {
	result := slices.Clone(r.installations)
	for i := range result {
		result[i].Migrations = maps.Clone(result[i].Migrations)
	}
	return result
}

// Check runs legacy local SDK hooks. It is not used to authorize kernel writes.
// Shipped SQL-backed modules fail closed here; only PostgreSQL decides them.
func (r *Registry) Check(ctx context.Context, point string, input Check, load FactLoader) error {
	for _, g := range r.guards[point] {
		guardCtx, cancel := context.WithTimeout(ctx, g.timeout)
		check := input
		check.Facts = nil
		var err error
		if g.view != "" {
			if load == nil {
				err = errors.New("module facts unavailable")
			} else {
				check.Facts, err = load(guardCtx, g.view, input.Document)
			}
		}
		if err == nil {
			err = invoke(guardCtx, g.check, check)
		}
		if guardCtx.Err() != nil {
			err = guardCtx.Err()
		}
		cancel()
		if err == nil {
			continue
		}
		if denied, ok := errors.AsType[*Denial](err); ok {
			code := denied.Code
			if !moduleID.MatchString(code) || !strings.HasPrefix(code, g.module+".") {
				code = g.module + ".denied"
			}
			return &Denial{ModuleID: g.module, Code: code}
		}
		return &Unavailable{ModuleID: g.module}
	}
	return ctx.Err()
}

func invoke(ctx context.Context, guard Guard, check Check) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("guard panic")
		}
	}()
	return guard(ctx, check)
}

// AcceptsManifest checks the complete executable projection, including guard policy.
func (r *Registry) AcceptsManifest(m Manifest) bool {
	if r == nil {
		return false
	}
	expected, ok := r.manifests[m.ID]
	if !ok {
		return false
	}
	canonical := func(value Manifest) string {
		raw, _ := json.Marshal(value)
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var object any
		if decoder.Decode(&object) != nil {
			return ""
		}
		normalized, _ := json.Marshal(object)
		return string(normalized)
	}
	return canonical(expected) == canonical(m)
}

// SQLOnlyGuard records the handler binding without duplicating its business
// predicate. Calling it outside the transactional kernel fails closed.
func SQLOnlyGuard(moduleID string) Guard {
	return func(ctx context.Context, _ Check) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return &Unavailable{ModuleID: moduleID}
	}
}
