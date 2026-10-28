package modules

import (
	"encoding/json"
	"strings"
	"testing"
)

func cloneManifestForTest(t *testing.T, m Manifest) Manifest {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var result Manifest
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// An installed package is caller owned. It must not remain a live configuration
// channel after the registry has captured its immutable executable contract.
func TestRegistryAdversarialManifestSnapshotIsImmutable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Manifest)
	}{
		{name: "guard policy", mutate: func(m *Manifest) { m.Guards[0].Required = false }},
		{name: "capability", mutate: func(m *Manifest) { m.RequiresCapabilities[0] = "unsupported.permission.v1" }},
		{name: "dependency", mutate: func(m *Manifest) { m.RequiresModules[0].Version = ">=2.0.0 <3.0.0" }},
		{name: "conflict", mutate: func(m *Manifest) { m.ConflictsWith[0] = m.ID }},
		{name: "migration metadata", mutate: func(m *Manifest) { m.Migrations[0].Checksum = strings.Repeat("b", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureManifest()
			m.RequiresCapabilities = []string{"platform.outbox.v1"}
			m.RequiresModules = []Dependency{{ID: "client.base", Version: ">=1.0.0 <2.0.0"}}
			m.ConflictsWith = []string{"client.absent"}
			m.Data = ManifestData{Schema: canonicalSchema(m.ID), TargetSchemaVersion: 1}
			m.Migrations = []ManifestMigration{{ID: "001_initial", Checksum: strings.Repeat("a", 64)}}
			code := Module{ID: m.ID, Version: m.Version, SchemaVersion: 1, Guards: map[string]Guard{"Approve": SQLOnlyGuard(m.ID)}, GuardPoints: map[string][]string{"Approve": {DispatchPoint}}, MigrationChecksums: map[string]string{"001_initial": strings.Repeat("a", 64)}}
			base := fixtureManifest()
			base.ID = "client.base"
			base.Guards = nil
			registry, err := NewRegistry([]Manifest{m, base}, []Module{code, {ID: base.ID, Version: base.Version}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			original := cloneManifestForTest(t, m)
			if !registry.AcceptsManifest(original) {
				t.Fatal("original executable projection was not accepted")
			}
			tc.mutate(&m)
			if !registry.AcceptsManifest(original) {
				t.Fatal("caller mutation changed the registry's original executable contract")
			}
			if registry.AcceptsManifest(m) {
				t.Fatal("caller-owned manifest mutation was accepted as the installed contract")
			}
		})
	}
}

func TestRegistryAdversarialRawPolicySnapshotIsImmutable(t *testing.T) {
	m := Manifest{ManifestVersion: 1, ID: "client.rules", Version: "1.0.0", Runtime: "declarative_v2", Compatibility: Compatibility{Core: ">=0.4.0 <1.0.0", SDK: ">=2.0.0 <3.0.0"}, Data: ManifestData{Schema: "mod_client_rules", TargetSchemaVersion: 1}, Guards: []GuardSpec{{Point: ReservationPoint, Handler: "Allow", Required: true, FailurePolicy: "deny", TimeoutMS: 100}}, Policies: []json.RawMessage{json.RawMessage(`{"handler":"Allow","any":[{"conditions":[{"field":"decision","op":"eq","value":"allow"}]}]}`)}}
	r, err := NewRegistry([]Manifest{m}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	original := cloneManifestForTest(t, m)
	index := strings.Index(string(m.Policies[0]), "allow")
	copy(m.Policies[0][index:], []byte("other"))
	if !r.AcceptsManifest(original) || r.AcceptsManifest(m) {
		t.Fatal("mutable RawMessage policy changed the immutable registry contract")
	}
}
