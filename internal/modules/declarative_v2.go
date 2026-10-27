package modules

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
)

type ruleV2 struct {
	valuePresent, valuesPresent bool
	Field                       string   `json:"field"`
	Op                          string   `json:"op"`
	Value                       *string  `json:"value,omitempty"`
	Values                      []string `json:"values,omitempty"`
}
type groupV2 struct {
	Conditions []ruleV2 `json:"conditions"`
}
type policyV2 struct {
	Handler string    `json:"handler"`
	Any     []groupV2 `json:"any"`
}

var decimalLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)([.][0-9]+)?$`)

// V2 validates the package shape here. Business evaluation belongs exclusively
// to PostgreSQL; Registry.Check cannot be used as a V2 authorization shortcut.
func declarativeV2(m Manifest) (Module, error) {
	code := Module{ID: m.ID, Version: m.Version, SchemaVersion: m.Data.TargetSchemaVersion, FactView: canonicalSchema(m.ID) + ".document_facts_v1", Guards: map[string]Guard{}, GuardPoints: map[string][]string{}, MigrationChecksums: map[string]string{}}
	invalid := errors.New("invalid declarative_v2 policy")
	if len(m.Policies) < 1 || len(m.Policies) > 32 {
		return code, invalid
	}
	for _, migration := range m.Migrations {
		code.MigrationChecksums[migration.ID] = migration.Checksum
	}
	for _, raw := range m.Policies {
		var p policyV2
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if len(raw) > 16384 || d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF || p.Handler == "" || code.Guards[p.Handler] != nil || len(p.Any) < 1 || len(p.Any) > 16 {
			return code, invalid
		}
		total := 0
		for _, group := range p.Any {
			if len(group.Conditions) < 1 || len(group.Conditions) > 32 {
				return code, invalid
			}
			total += len(group.Conditions)
			if total > 128 {
				return code, invalid
			}
			for _, r := range group.Conditions {
				if !viewName.MatchString(r.Field) || len(r.Field) > 63 {
					return code, invalid
				}
				needsValue := false
				needsValues := false
				switch r.Op {
				case "eq", "neq":
					needsValue = true
				case "decimal_gte", "decimal_lte":
					needsValue = true
					if r.Value == nil || len(*r.Value) > 128 || !decimalLiteral.MatchString(*r.Value) {
						return code, invalid
					}
				case "in":
					needsValues = true
					if len(r.Values) < 1 || len(r.Values) > 64 {
						return code, invalid
					}
					for _, v := range r.Values {
						if len(v) > 1024 {
							return code, invalid
						}
					}
				case "nonempty", "future", "not_actor", "is_true", "exists":
				default:
					return code, invalid
				}
				if needsValue != r.valuePresent || needsValues != r.valuesPresent || needsValue != (r.Value != nil) || needsValues != (len(r.Values) > 0) || r.Value != nil && len(*r.Value) > 1024 {
					return code, invalid
				}
			}
		}
		code.Guards[p.Handler] = SQLOnlyGuard(m.ID)
	}
	for _, g := range m.Guards {
		code.GuardPoints[g.Handler] = append(code.GuardPoints[g.Handler], g.Point)
	}
	if len(code.Guards) != len(code.GuardPoints) {
		return code, invalid
	}
	return code, nil
}

func (r *ruleV2) UnmarshalJSON(raw []byte) error {
	type plain ruleV2
	var decoded plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*r = ruleV2(decoded)
	_, r.valuePresent = fields["value"]
	_, r.valuesPresent = fields["values"]
	if r.valuesPresent {
		var entries []json.RawMessage
		if err := json.Unmarshal(fields["values"], &entries); err != nil {
			return err
		}
		for _, entry := range entries {
			if bytes.Equal(bytes.TrimSpace(entry), []byte("null")) {
				return errors.New("null is not a policy string")
			}
		}
	}
	return nil
}
