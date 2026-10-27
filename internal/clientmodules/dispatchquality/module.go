// Package dispatchquality demonstrates a compiled, fail-closed client module.
package dispatchquality

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/modules"
)

const ID = "example.dispatch_quality"

func Manifest() modules.Manifest {
	return modules.Manifest{ManifestVersion: 1, ID: ID, Version: "1.0.0",
		Compatibility:        modules.Compatibility{Core: ">=0.2.0 <1.0.0", SDK: ">=1.0.0 <2.0.0"},
		RequiresCapabilities: []string{modules.DispatchPoint, "platform.aggregate_transaction.v1"},
		Guards:               []modules.GuardSpec{{Point: modules.DispatchPoint, Handler: "CheckApproval", Required: true, FailurePolicy: "deny", TimeoutMS: 100}},
	}
}

func New() modules.Module {
	return modules.Module{ID: ID, Version: "1.0.0", FactView: "mod_example_dispatch_quality.document_facts_v1",
		Guards: map[string]modules.Guard{"CheckApproval": checkApproval}}
}

func checkApproval(ctx context.Context, check modules.Check) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var fact struct {
		Decision  string    `json:"decision"`
		Evidence  string    `json:"evidence_reference"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if len(check.Facts) == 0 || json.Unmarshal(check.Facts, &fact) != nil || fact.Decision != "approved" || fact.Evidence == "" || !fact.ExpiresAt.After(time.Now()) {
		return &modules.Denial{Code: ID + ".approval_required"}
	}
	return nil
}
