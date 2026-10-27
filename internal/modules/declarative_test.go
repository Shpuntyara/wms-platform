package modules

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDeclarativeModuleDoesNotNeedCompiledBuild(t *testing.T) {
	m := Manifest{ManifestVersion: 1, ID: "client.shipping", Version: "1.0.0", Runtime: "declarative_v1", Compatibility: Compatibility{Core: ">=0.3.0 <1.0.0", SDK: ">=2.0.0 <3.0.0"}, Data: ManifestData{Schema: "mod_client_shipping", TargetSchemaVersion: 1}, Guards: []GuardSpec{{Point: DispatchPoint, Handler: "Approved", Required: true, FailurePolicy: "deny", TimeoutMS: 100}}, Policies: []json.RawMessage{json.RawMessage(`{"handler":"Approved","conditions":[{"field":"decision","op":"eq","value":"approved"}]}`)}}
	r, err := NewRegistry([]Manifest{m}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		fact   string
		denied bool
	}{{`{"decision":"approved"}`, true}, {`{"decision":"rejected"}`, true}, {`{}`, true}} {
		err = r.Check(t.Context(), DispatchPoint, Check{}, func(_ctx context.Context, _ string, _ Document) (json.RawMessage, error) {
			return json.RawMessage(test.fact), nil
		})
		if (err != nil) != test.denied {
			t.Fatalf("facts=%s err=%v", test.fact, err)
		}
	}
	m.Policies = []json.RawMessage{json.RawMessage(`{"handler":"Approved","conditions":[{"field":"decision","op":"sql","value":"true"}]}`)}
	if _, err := NewRegistry([]Manifest{m}, nil, nil); err == nil {
		t.Fatal("unsupported policy operator accepted")
	}
}
