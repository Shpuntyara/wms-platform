package dispatchquality

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/modules"
)

func TestPackagedManifestMatchesCompiledModule(t *testing.T) {
	raw, err := os.ReadFile("../../../modules/dispatch-quality/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest modules.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := modules.NewRegistry([]modules.Manifest{manifest}, []modules.Module{New()}, []string{ID}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckApproval(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, decision, evidence string
		expired, valid           bool
	}{
		{name: "approved", decision: "approved", evidence: "verified-evidence", valid: true},
		{name: "rejected", decision: "rejected", evidence: "verified-evidence"},
		{name: "missing evidence", decision: "approved"},
		{name: "expired", decision: "approved", evidence: "verified-evidence", expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expiry := time.Now().Add(time.Hour)
			if tc.expired {
				expiry = time.Now().Add(-time.Hour)
			}
			fact, _ := json.Marshal(map[string]any{"decision": tc.decision, "evidence_reference": tc.evidence, "expires_at": expiry})
			err := New().Guards["CheckApproval"](t.Context(), modules.Check{Facts: fact})
			if (err == nil) != tc.valid {
				t.Fatalf("error=%v", err)
			}
		})
	}
	for _, raw := range []string{"", `{"expires_at":"malformed"}`} {
		if err := New().Guards["CheckApproval"](t.Context(), modules.Check{Facts: json.RawMessage(raw)}); err == nil {
			t.Fatal("missing or malformed fact accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := New().Guards["CheckApproval"](ctx, modules.Check{}); err == nil {
		t.Fatal("canceled guard context accepted")
	}
}
