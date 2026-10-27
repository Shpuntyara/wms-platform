package dispatchquality

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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

func TestCompiledMigrationChecksumsMatchPackage(t *testing.T) {
	code := New()
	manifest := Manifest()
	if code.SchemaVersion != 3 || manifest.Data.TargetSchemaVersion != 3 || manifest.Data.Schema != "mod_example_dispatch_quality" || len(manifest.Migrations) != 3 || len(code.MigrationChecksums) != 3 {
		t.Fatal("compiled storage metadata does not describe the installed package")
	}
	for _, migration := range manifest.Migrations {
		content, err := os.ReadFile(filepath.Join("../../../modules/dispatch-quality/migrations", migration.ID+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(content, []byte("\r\n")) {
			t.Fatalf("migration %s must use LF before checksumming", migration.ID)
		}
		sum := sha256.Sum256(content)
		if expected := hex.EncodeToString(sum[:]); code.MigrationChecksums[migration.ID] != expected {
			t.Fatalf("compiled checksum for %s does not match SQL package", migration.ID)
		}
	}
}

func TestApprovalCannotBeDecidedOutsideSQL(t *testing.T) {
	for _, raw := range []string{"", `{"decision":"approved","evidence_reference":"verified-evidence","expires_at":"2099-01-01T00:00:00Z"}`} {
		if err := New().Guards["CheckApproval"](t.Context(), modules.Check{Facts: json.RawMessage(raw)}); err == nil {
			t.Fatal("approval decided without SQL transaction")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := New().Guards["CheckApproval"](ctx, modules.Check{}); err == nil {
		t.Fatal("cancelled context accepted")
	}
}
