package modules

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureManifest() Manifest {
	return Manifest{ManifestVersion: 1, ID: "client.quality", Version: "1.0.0",
		Compatibility: Compatibility{Core: ">=0.2.0 <1.0.0", SDK: ">=2.0.0 <3.0.0"},
		Guards:        []GuardSpec{{Point: DispatchPoint, Handler: "Approve", Required: true, FailurePolicy: "deny", TimeoutMS: 50}}}
}

func TestRegistryRejectsLegacySDK(t *testing.T) {
	m := fixtureManifest()
	m.Compatibility.SDK = ">=1.0.0 <2.0.0"
	code := Module{ID: m.ID, Version: m.Version, GuardPoints: map[string][]string{"Approve": {DispatchPoint}}, Guards: map[string]Guard{"Approve": func(context.Context, Check) error { return nil }}}
	if _, err := NewRegistry([]Manifest{m}, []Module{code}, nil); err == nil {
		t.Fatal("legacy SDK accepted after changing required compiled module metadata")
	}
}

func TestRegistryRejectsRelocatedGuard(t *testing.T) {
	m := fixtureManifest()
	code := Module{ID: m.ID, Version: m.Version, GuardPoints: map[string][]string{"Approve": {DispatchPoint}}, Guards: map[string]Guard{"Approve": func(context.Context, Check) error { return nil }}}
	m.Guards[0].Point = ReservationPoint
	if _, err := NewRegistry([]Manifest{m}, []Module{code}, nil); err == nil {
		t.Fatal("mandatory dispatch guard was relocated to a different supported point")
	}
}

func TestRegistryRejectsSchemaIdentityCollisions(t *testing.T) {
	for _, withFacts := range []bool{false, true} {
		t.Run(map[bool]string{false: "no facts", true: "with facts"}[withFacts], func(t *testing.T) {
			var installed []Manifest
			var compiled []Module
			for _, id := range []string{"a.b_c", "a_b.c"} {
				m := fixtureManifest()
				m.ID, m.Guards = id, nil
				code := Module{ID: id, Version: m.Version}
				if withFacts {
					code.FactView = "mod_a_b_c.document_facts_v1"
				}
				installed = append(installed, m)
				compiled = append(compiled, code)
			}
			if _, err := NewRegistry(installed, compiled, nil); err == nil {
				t.Fatal("distinct module identities share their storage namespace")
			}
		})
	}
}

func TestRegistryRejectsTruncatedPostgresIdentifiers(t *testing.T) {
	for _, field := range []string{"module id", "fact view"} {
		t.Run(field, func(t *testing.T) {
			m := fixtureManifest()
			m.Guards = nil
			code := Module{ID: m.ID, Version: m.Version}
			if field == "module id" {
				m.ID = "client." + strings.Repeat("x", 52)
				code.ID = m.ID
			} else {
				code.FactView = "mod_client_quality." + strings.Repeat("x", 64)
			}
			if _, err := NewRegistry([]Manifest{m}, []Module{code}, nil); err == nil {
				t.Fatal("identifier beyond the contract length accepted")
			}
		})
	}
}

func TestNewRegistryRejectsInvalidInstallations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		edit    func(*Manifest, *Module)
		missing bool
	}{
		{name: "missing required module", missing: true},
		{name: "missing handler", edit: func(_ *Manifest, m *Module) { m.Guards = nil }},
		{name: "compiled guard omitted from manifest", edit: func(m *Manifest, _ *Module) { m.Guards = nil }},
		{name: "wrong compiled version", edit: func(_ *Manifest, m *Module) { m.Version = "2.0.0" }},
		{name: "unsupported sdk", edit: func(m *Manifest, _ *Module) { m.Compatibility.SDK = ">=3.0.0 <4.0.0" }},
		{name: "unsupported core", edit: func(m *Manifest, _ *Module) { m.Compatibility.Core = ">=1.0.0 <2.0.0" }},
		{name: "fail open", edit: func(m *Manifest, _ *Module) { m.Guards[0].FailurePolicy = "allow" }},
		{name: "optional mandatory guard", edit: func(m *Manifest, _ *Module) { m.Guards[0].Required = false }},
		{name: "unknown point", edit: func(m *Manifest, _ *Module) { m.Guards[0].Point = "anything.unknown.v1" }},
		{name: "unknown capability", edit: func(m *Manifest, _ *Module) { m.RequiresCapabilities = []string{"unsupported.capability.v1"} }},
		{name: "missing dependency", edit: func(m *Manifest, _ *Module) {
			m.RequiresModules = []Dependency{{ID: "industry.other", Version: ">=1.0.0 <2.0.0"}}
		}},
		{name: "foreign facts schema", edit: func(_ *Manifest, m *Module) { m.FactView = "inventory.stock_balances" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureManifest()
			code := Module{ID: m.ID, Version: m.Version, GuardPoints: map[string][]string{"Approve": {DispatchPoint}}, Guards: map[string]Guard{"Approve": func(context.Context, Check) error { return nil }}}
			if tc.edit != nil {
				tc.edit(&m, &code)
			}
			installed := []Manifest{m}
			if tc.missing {
				installed = nil
			}
			if _, err := NewRegistry(installed, []Module{code}, []string{m.ID}); err == nil {
				t.Fatal("invalid installation accepted")
			}
		})
	}
}

func TestRegistryRunsEveryRequiredGuard(t *testing.T) {
	t.Parallel()
	m := fixtureManifest()
	m.Guards = append(m.Guards, GuardSpec{Point: DispatchPoint, Handler: "Second", Required: true, FailurePolicy: "deny", TimeoutMS: 50})
	var calls atomic.Int32
	code := Module{ID: m.ID, Version: m.Version, GuardPoints: map[string][]string{"Approve": {DispatchPoint}, "Second": {DispatchPoint}}, Guards: map[string]Guard{
		"Approve": func(context.Context, Check) error { calls.Add(1); return nil },
		"Second":  func(context.Context, Check) error { calls.Add(1); return &Denial{Code: "client.quality.rejected"} },
	}}
	r, err := NewRegistry([]Manifest{m}, []Module{code}, []string{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Changing caller-owned maps after construction must not remove a guard.
	delete(code.Guards, "Second")
	err = r.Check(context.Background(), DispatchPoint, Check{}, nil)
	var denial *Denial
	if !errors.As(err, &denial) || calls.Load() != 2 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}

func TestRegistryFailsClosedOnGuardFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		guard Guard
	}{
		{"panic", func(context.Context, Check) error { panic("sensitive implementation detail") }},
		{"error", func(context.Context, Check) error { return errors.New("connection secret") }},
		{"timeout", func(ctx context.Context, _ Check) error { <-ctx.Done(); return ctx.Err() }},
		{"late success", func(ctx context.Context, _ Check) error { <-ctx.Done(); return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureManifest()
			m.Guards[0].TimeoutMS = 5
			r, err := NewRegistry([]Manifest{m}, []Module{{ID: m.ID, Version: m.Version, GuardPoints: map[string][]string{"Approve": {DispatchPoint}}, Guards: map[string]Guard{"Approve": tc.guard}}}, []string{m.ID})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = r.Check(ctx, DispatchPoint, Check{}, nil)
			if _, ok := errors.AsType[*Unavailable](err); !ok {
				t.Fatalf("expected fail-closed unavailable, got %v", err)
			}
		})
	}
}

func TestRegistryLoadsOnlyRegisteredModuleFacts(t *testing.T) {
	t.Parallel()
	m := fixtureManifest()
	var received string
	code := Module{ID: m.ID, Version: m.Version, FactView: "mod_client_quality.document_facts_v1", GuardPoints: map[string][]string{"Approve": {DispatchPoint}}, Guards: map[string]Guard{
		"Approve": func(_ context.Context, c Check) error { received = string(c.Facts); return nil },
	}}
	r, err := NewRegistry([]Manifest{m}, []Module{code}, []string{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	loader := func(_ context.Context, view string, doc Document) (json.RawMessage, error) {
		if view != code.FactView || doc.ID != "document" {
			t.Fatal("unexpected facts access")
		}
		return json.RawMessage(`{"approved":true}`), nil
	}
	if err := r.Check(context.Background(), DispatchPoint, Check{Document: Document{ID: "document"}, Facts: json.RawMessage(`{"forged":true}`)}, loader); err != nil {
		t.Fatal(err)
	}
	if received != `{"approved":true}` {
		t.Fatalf("facts = %s", received)
	}
}

func TestRegistryRejectsDuplicateModules(t *testing.T) {
	t.Parallel()
	m := fixtureManifest()
	code := Module{ID: m.ID, Version: m.Version, GuardPoints: map[string][]string{"Approve": {DispatchPoint}}, Guards: map[string]Guard{"Approve": func(context.Context, Check) error { return nil }}}
	if _, err := NewRegistry([]Manifest{m, m}, []Module{code}, nil); err == nil {
		t.Fatal("duplicate installed module accepted")
	}
	if _, err := NewRegistry([]Manifest{m}, []Module{code, code}, nil); err == nil {
		t.Fatal("duplicate compiled module accepted")
	}
}

func TestRegistryRejectsUnsupportedExecutableExtensions(t *testing.T) {
	for _, field := range []string{"policies", "subscribers", "runtime"} {
		t.Run(field, func(t *testing.T) {
			m := fixtureManifest()
			raw, _ := json.Marshal(m)
			var document map[string]any
			_ = json.Unmarshal(raw, &document)
			if field == "runtime" {
				document[field] = "external_worker"
			} else {
				document[field] = []any{map[string]any{"handler": "Unimplemented"}}
			}
			raw, _ = json.Marshal(document)
			_ = json.Unmarshal(raw, &m)
			code := Module{ID: m.ID, Version: m.Version, GuardPoints: map[string][]string{"Approve": {DispatchPoint}}, Guards: map[string]Guard{"Approve": func(context.Context, Check) error { return nil }}}
			if _, err := NewRegistry([]Manifest{m}, []Module{code}, nil); err == nil {
				t.Fatal("unsupported executable extension was silently ignored")
			}
		})
	}
}

func TestRegistryRequiresExactGuardBindings(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Manifest, *Module)
	}{
		{"missing compiled binding", func(_ *Manifest, code *Module) { code.GuardPoints = nil }},
		{"unknown bound handler", func(_ *Manifest, code *Module) { code.GuardPoints["Missing"] = []string{DispatchPoint} }},
		{"empty bound points", func(_ *Manifest, code *Module) { code.GuardPoints["Approve"] = nil }},
		{"unknown bound point", func(_ *Manifest, code *Module) { code.GuardPoints["Approve"] = []string{"unknown.point.v1"} }},
		{"duplicate compiled point", func(_ *Manifest, code *Module) { code.GuardPoints["Approve"] = []string{DispatchPoint, DispatchPoint} }},
		{"deleted manifest binding", func(m *Manifest, _ *Module) { m.Guards = nil }},
		{"added manifest binding", func(m *Manifest, _ *Module) {
			spec := m.Guards[0]
			spec.Point = ReservationPoint
			m.Guards = append(m.Guards, spec)
		}},
		{"duplicate manifest binding", func(m *Manifest, _ *Module) { m.Guards = append(m.Guards, m.Guards[0]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureManifest()
			code := Module{ID: m.ID, Version: m.Version, GuardPoints: map[string][]string{"Approve": {DispatchPoint}}, Guards: map[string]Guard{"Approve": func(context.Context, Check) error { return nil }}}
			tc.edit(&m, &code)
			if _, err := NewRegistry([]Manifest{m}, []Module{code}, nil); err == nil {
				t.Fatal("nonmatching compiled and installed binding sets accepted")
			}
		})
	}
}

func TestRegistryFreezesMultiPointGuardBindings(t *testing.T) {
	m := fixtureManifest()
	second := m.Guards[0]
	second.Point = ReservationPoint
	m.Guards = append(m.Guards, second)
	var calls atomic.Int32
	code := Module{ID: m.ID, Version: m.Version, GuardPoints: map[string][]string{"Approve": {DispatchPoint, ReservationPoint}}, Guards: map[string]Guard{"Approve": func(context.Context, Check) error { calls.Add(1); return nil }}}
	r, err := NewRegistry([]Manifest{m}, []Module{code}, nil)
	if err != nil {
		t.Fatal(err)
	}
	code.GuardPoints["Approve"][0] = ReleasePoint
	delete(code.GuardPoints, "Approve")
	m.Guards[1].Point = ReleasePoint
	for _, point := range []string{DispatchPoint, ReservationPoint} {
		if err := r.Check(t.Context(), point, Check{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("registered bindings changed after construction: calls=%d", calls.Load())
	}
}

func migrationFixture() (Manifest, Module) {
	m := fixtureManifest()
	m.Guards = nil
	m.Data = ManifestData{Schema: "mod_client_quality", TargetSchemaVersion: 2}
	m.Migrations = []ManifestMigration{{ID: "001_create"}, {ID: "002_projection"}}
	code := Module{ID: m.ID, Version: m.Version, SchemaVersion: 2, MigrationChecksums: map[string]string{
		"001_create": strings.Repeat("a", 64), "002_projection": strings.Repeat("b", 64),
	}}
	return m, code
}

func TestRegistryRejectsInvalidInstallationMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Manifest, *Module)
	}{
		{"wrong schema", func(m *Manifest, _ *Module) { m.Data.Schema = "mod_other_client" }},
		{"wrong schema version", func(m *Manifest, _ *Module) { m.Data.TargetSchemaVersion = 3 }},
		{"missing data", func(m *Manifest, _ *Module) { m.Data = ManifestData{} }},
		{"missing migration", func(m *Manifest, _ *Module) { m.Migrations = m.Migrations[:1] }},
		{"extra migration", func(m *Manifest, _ *Module) {
			m.Migrations = append(m.Migrations, ManifestMigration{ID: "003_unknown"})
		}},
		{"duplicate migration", func(m *Manifest, _ *Module) { m.Migrations[1] = m.Migrations[0] }},
		{"invalid checksum", func(_ *Manifest, code *Module) { code.MigrationChecksums["001_create"] = "invalid" }},
		{"invalid migration id", func(m *Manifest, code *Module) {
			delete(code.MigrationChecksums, "001_create")
			m.Migrations[0].ID = "../001_create"
			code.MigrationChecksums[m.Migrations[0].ID] = strings.Repeat("a", 64)
		}},
		{"undeclared compiled schema version", func(_ *Manifest, code *Module) { code.SchemaVersion = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, code := migrationFixture()
			tc.edit(&m, &code)
			if _, err := NewRegistry([]Manifest{m}, []Module{code}, nil); err == nil {
				t.Fatal("invalid migration or schema metadata accepted")
			}
		})
	}
}

func TestRegistryInstallationsAreIndependentSnapshots(t *testing.T) {
	m, code := migrationFixture()
	r, err := NewRegistry([]Manifest{m}, []Module{code}, nil)
	if err != nil {
		t.Fatal(err)
	}
	code.MigrationChecksums["001_create"] = strings.Repeat("c", 64)
	m.Migrations[0].ID = "099_changed"
	first := r.Installations()
	if len(first) != 1 || first[0].ID != m.ID || first[0].Schema != "mod_client_quality" || first[0].SchemaVersion != 2 || len(first[0].Migrations) != 2 || first[0].Migrations["001_create"] != strings.Repeat("a", 64) {
		t.Fatalf("unexpected frozen installation: %+v", first)
	}
	first[0].ID = "changed.module"
	delete(first[0].Migrations, "001_create")
	second := r.Installations()
	if second[0].ID != m.ID || second[0].Migrations["001_create"] != strings.Repeat("a", 64) {
		t.Fatalf("installation snapshots share mutable data: %+v", second)
	}
}

func TestRegistryChecksConcurrentCommands(t *testing.T) {
	t.Parallel()
	m := fixtureManifest()
	var calls atomic.Int32
	code := Module{ID: m.ID, Version: m.Version, GuardPoints: map[string][]string{"Approve": {DispatchPoint}}, Guards: map[string]Guard{"Approve": func(context.Context, Check) error { calls.Add(1); return nil }}}
	r, err := NewRegistry([]Manifest{m}, []Module{code}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if err := r.Check(t.Context(), DispatchPoint, Check{}, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 32 {
		t.Fatalf("concurrent guards skipped: calls=%d", calls.Load())
	}
}
