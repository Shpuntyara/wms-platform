package modules

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

type condition struct {
	Field string  `json:"field"`
	Op    string  `json:"op"`
	Value *string `json:"value,omitempty"`
}
type policy struct {
	Handler    string      `json:"handler"`
	Conditions []condition `json:"conditions"`
}

// declarativeModule permits a bounded vocabulary; it never evaluates code or SQL.
func declarativeModule(m Manifest) (Module, error) {
	code := Module{ID: m.ID, Version: m.Version, SchemaVersion: m.Data.TargetSchemaVersion, FactView: canonicalSchema(m.ID) + ".document_facts_v1", Guards: map[string]Guard{}, GuardPoints: map[string][]string{}, MigrationChecksums: map[string]string{}}
	if len(m.Policies) == 0 || len(m.Policies) > 32 {
		return code, errors.New("invalid declarative policy count")
	}
	for _, migration := range m.Migrations {
		code.MigrationChecksums[migration.ID] = migration.Checksum
	}
	for _, raw := range m.Policies {
		var p policy
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if len(raw) > 16384 || decoder.Decode(&p) != nil || decoder.Decode(new(any)) != io.EOF || p.Handler == "" || code.Guards[p.Handler] != nil || len(p.Conditions) == 0 || len(p.Conditions) > 32 {
			return code, errors.New("invalid declarative policy")
		}
		for _, c := range p.Conditions {
			if !viewName.MatchString(c.Field) || len(c.Field) > 63 {
				return code, errors.New("invalid policy field")
			}
			switch c.Op {
			case "eq", "nonempty", "future", "not_actor":
				if c.Op == "eq" && c.Value == nil {
					return code, errors.New("eq policy requires a value")
				}
			default:
				return code, errors.New("unsupported policy operator")
			}
		}
		code.Guards[p.Handler] = SQLOnlyGuard(m.ID)
	}
	for _, g := range m.Guards {
		code.GuardPoints[g.Handler] = append(code.GuardPoints[g.Handler], g.Point)
	}
	if len(code.Guards) != len(code.GuardPoints) {
		return code, errors.New("policy has no guard binding")
	}
	return code, nil
}
