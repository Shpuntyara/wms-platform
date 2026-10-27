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
	return modules.Manifest{ManifestVersion: 1, ID: ID, Version: "1.1.0",
		Compatibility:        modules.Compatibility{Core: ">=0.3.0 <1.0.0", SDK: ">=2.0.0 <3.0.0"},
		RequiresCapabilities: []string{modules.DispatchPoint, "platform.aggregate_transaction.v1"},
		Guards:               []modules.GuardSpec{{Point: modules.DispatchPoint, Handler: "CheckApproval", Required: true, FailurePolicy: "deny", TimeoutMS: 100}},
		Data:                 modules.ManifestData{Schema: "mod_example_dispatch_quality", TargetSchemaVersion: 2},
		Migrations:           []modules.ManifestMigration{{ID: "001_create_checks"}, {ID: "002_document_facts"}},
	}
}

func New() modules.Module {
	return modules.Module{ID: ID, Version: "1.1.0", FactView: "mod_example_dispatch_quality.document_facts_v1",
		Guards:        map[string]modules.Guard{"CheckApproval": checkApproval},
		GuardPoints:   map[string][]string{"CheckApproval": {modules.DispatchPoint}},
		SchemaVersion: 2,
		MigrationChecksums: map[string]string{
			"001_create_checks":  "8a6e79c10f2178a5b84c22f8e481bf71f8e1de0ca51be56c9731ee02f6a7b8e4",
			"002_document_facts": "0564a3ff4b1dc6ce3feece08a601b269a90d24312ac31eac0e5af4c9fa3c0c6d",
		}}
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
