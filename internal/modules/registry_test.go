package modules

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureManifest() Manifest {
	return Manifest{ManifestVersion: 1, ID: "client.quality", Version: "1.0.0",
		Compatibility: Compatibility{Core: ">=0.2.0 <1.0.0", SDK: ">=1.0.0 <2.0.0"},
		Guards:        []GuardSpec{{Point: DispatchPoint, Handler: "Approve", Required: true, FailurePolicy: "deny", TimeoutMS: 50}}}
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
		{name: "unsupported sdk", edit: func(m *Manifest, _ *Module) { m.Compatibility.SDK = ">=2.0.0 <3.0.0" }},
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
			code := Module{ID: m.ID, Version: m.Version, Guards: map[string]Guard{"Approve": func(context.Context, Check) error { return nil }}}
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
	code := Module{ID: m.ID, Version: m.Version, Guards: map[string]Guard{
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
			r, err := NewRegistry([]Manifest{m}, []Module{{ID: m.ID, Version: m.Version, Guards: map[string]Guard{"Approve": tc.guard}}}, []string{m.ID})
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
	code := Module{ID: m.ID, Version: m.Version, FactView: "mod_client_quality.document_facts_v1", Guards: map[string]Guard{
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
	code := Module{ID: m.ID, Version: m.Version, Guards: map[string]Guard{"Approve": func(context.Context, Check) error { return nil }}}
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
			code := Module{ID: m.ID, Version: m.Version, Guards: map[string]Guard{"Approve": func(context.Context, Check) error { return nil }}}
			if _, err := NewRegistry([]Manifest{m}, []Module{code}, nil); err == nil {
				t.Fatal("unsupported executable extension was silently ignored")
			}
		})
	}
}
