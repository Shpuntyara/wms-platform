// Package modules hosts trusted, compiled client extensions. It is not a sandbox.
package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	CoreVersion      = "0.2.0"
	SDKVersion       = "1.0.0"
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

// Manifest is the executable projection of the full package manifest.
type Manifest struct {
	ManifestVersion      int               `json:"manifest_version"`
	ID                   string            `json:"id"`
	Version              string            `json:"version"`
	Compatibility        Compatibility     `json:"compatibility"`
	RequiresCapabilities []string          `json:"requires_capabilities"`
	RequiresModules      []Dependency      `json:"requires_modules"`
	ConflictsWith        []string          `json:"conflicts_with"`
	Guards               []GuardSpec       `json:"guards"`
	Runtime              string            `json:"runtime"`
	Policies             []json.RawMessage `json:"policies"`
	Subscribers          []json.RawMessage `json:"subscribers"`
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
	guards    map[string][]registeredGuard
	factViews []string
}

var moduleID = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var viewName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

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
	r := &Registry{guards: make(map[string][]registeredGuard)}
	codes := make(map[string]Module)
	manifests := make(map[string]Manifest)
	for _, code := range compiled {
		if _, exists := codes[code.ID]; exists {
			return nil, fmt.Errorf("duplicate compiled module %q", code.ID)
		}
		if !moduleID.MatchString(code.ID) || !stableVersion.MatchString(code.Version) {
			return nil, errors.New("invalid compiled module identity")
		}
		if code.FactView != "" {
			parts := strings.Split(code.FactView, ".")
			if len(parts) != 2 || parts[0] != "mod_"+strings.ReplaceAll(code.ID, ".", "_") || !viewName.MatchString(parts[1]) {
				return nil, errors.New("module fact view must belong to its own schema")
			}
		}
		codes[code.ID] = code
	}
	capabilities := map[string]bool{ReservationPoint: true, ReleasePoint: true, MovementPoint: true, DispatchPoint: true, "platform.aggregate_transaction.v1": true, "platform.outbox.v1": true}
	for _, m := range installed {
		if (m.Runtime != "" && m.Runtime != "in_process_compiled") || len(m.Policies) > 0 || len(m.Subscribers) > 0 {
			return nil, fmt.Errorf("unsupported executable extension in module %q", m.ID)
		}
		if _, exists := manifests[m.ID]; exists {
			return nil, fmt.Errorf("duplicate installed module %q", m.ID)
		}
		if m.ManifestVersion != 1 || !moduleID.MatchString(m.ID) || !stableVersion.MatchString(m.Version) || !inRange(CoreVersion, m.Compatibility.Core) || !inRange(SDKVersion, m.Compatibility.SDK) {
			return nil, fmt.Errorf("incompatible module %q", m.ID)
		}
		code, exists := codes[m.ID]
		if !exists || code.Version != m.Version {
			return nil, fmt.Errorf("compiled module missing or version mismatch: %q", m.ID)
		}
		for _, capability := range m.RequiresCapabilities {
			if !capabilities[capability] {
				return nil, fmt.Errorf("unsupported module capability %q", capability)
			}
		}
		manifests[m.ID] = m
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
		usedHandlers := map[string]bool{}
		for _, spec := range m.Guards {
			if spec.Point != ReservationPoint && spec.Point != ReleasePoint && spec.Point != MovementPoint && spec.Point != DispatchPoint {
				return nil, fmt.Errorf("unknown guard point %q", spec.Point)
			}
			fn := code.Guards[spec.Handler]
			key := spec.Point + "/" + spec.Handler
			if fn == nil || !spec.Required || spec.FailurePolicy != "deny" || spec.TimeoutMS < 1 || spec.TimeoutMS > 1000 || seen[key] {
				return nil, fmt.Errorf("invalid required guard in %q", id)
			}
			seen[key] = true
			usedHandlers[spec.Handler] = true
			r.guards[spec.Point] = append(r.guards[spec.Point], registeredGuard{module: id, view: code.FactView, timeout: time.Duration(spec.TimeoutMS) * time.Millisecond, check: fn})
		}
		for handler := range code.Guards {
			if !usedHandlers[handler] {
				return nil, fmt.Errorf("compiled guard omitted from active manifest: %q", id)
			}
		}
		if code.FactView != "" {
			r.factViews = append(r.factViews, code.FactView)
		}
	}
	return r, nil
}

func (r *Registry) FactViews() []string { return slices.Clone(r.factViews) }

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
