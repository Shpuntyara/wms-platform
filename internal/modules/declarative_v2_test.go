package modules

import (
	"encoding/json"
	"testing"
)

func TestDeclarativeV2ValidatesAlternativesAndTypedRules(t *testing.T) {
	m := Manifest{ManifestVersion: 1, ID: "client.rules", Version: "1.0.0", Runtime: "declarative_v2", Compatibility: Compatibility{Core: ">=0.4.0 <1.0.0", SDK: ">=2.0.0 <3.0.0"}, Data: ManifestData{Schema: "mod_client_rules", TargetSchemaVersion: 1}, Guards: []GuardSpec{{Point: ReservationPoint, Handler: "Allow", Required: true, FailurePolicy: "deny", TimeoutMS: 100}}}
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{"handler":"Allow","any":[{"conditions":[{"field":"weight","op":"decimal_lte","value":"999999999999999999.125"},{"field":"zone","op":"in","values":["cold","dry"]}]},{"conditions":[{"field":"override","op":"is_true"}]}]}`, true},
		{`{"handler":"Allow","any":[]}`, false},
		{`{"handler":"Allow","any":[{"conditions":[{"field":"zone","op":"nonempty","value":null}]}]}`, false},
		{`{"handler":"Allow","any":[{"conditions":[{"field":"zone","op":"in","values":["dry",null]}]}]}`, false},
		{`{"handler":"Allow","any":[{"conditions":[{"field":"weight","op":"decimal_gte","value":"NaN"}]}]}`, false},
		{`{"handler":"Allow","any":[{"conditions":[{"field":"zone","op":"in","values":[]}]}]}`, false},
		{`{"handler":"Allow","any":[{"conditions":[{"field":"zone","op":"nonempty","typo":true}]}]}`, false},
		{`{"handler":"Allow","conditions":[{"field":"zone","op":"nonempty"}]}`, false},
	} {
		m.Policies = []json.RawMessage{json.RawMessage(tc.raw)}
		_, err := NewRegistry([]Manifest{m}, nil, nil)
		if (err == nil) != tc.valid {
			t.Errorf("valid=%v err=%v raw=%s", tc.valid, err, tc.raw)
		}
	}
}
