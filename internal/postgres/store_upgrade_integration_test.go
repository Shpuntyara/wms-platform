//go:build integration

package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/clientmodules/dispatchquality"
	"github.com/Shpuntyara/wms-platform/internal/modules"
)

func upgradeDispatchSQL(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../db/examples/upgrade_dispatch_quality_1_1.psql")
	if err != nil {
		t.Fatal(err)
	}
	var sql []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, `\`) || trimmed == "BEGIN;" || trimmed == "COMMIT;" {
			continue
		}
		sql = append(sql, line)
	}
	return strings.Join(sql, "\n")
}

func runDispatchUpgrade(t *testing.T, f *fixture, sql string) error {
	t.Helper()
	tx, err := f.admin.Begin(t.Context())
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err := tx.Exec(t.Context(), sql); err != nil {
		return err
	}
	return tx.Commit(t.Context())
}

func seedDispatchUpgrade(t *testing.T, f *fixture) []byte {
	t.Helper()
	f.exec(`DELETE FROM platform.module_migrations WHERE module_id='example.dispatch_quality'`)
	f.exec(`DELETE FROM platform.modules WHERE id='example.dispatch_quality'`)
	t.Cleanup(func() {
		f.cleanupExec(`DELETE FROM platform.module_migrations WHERE module_id='example.dispatch_quality'`)
		f.cleanupExec(`DELETE FROM platform.modules WHERE id='example.dispatch_quality'`)
	})
	newManifest, err := os.ReadFile("../../tests/fixtures/dispatch-quality-1.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var oldManifest map[string]any
	if err := json.Unmarshal(newManifest, &oldManifest); err != nil {
		t.Fatal(err)
	}
	oldManifest["version"] = "1.0.0"
	oldManifest["compatibility"] = map[string]string{"core": ">=0.2.0 <1.0.0", "sdk": ">=1.0.0 <2.0.0"}
	oldJSON, err := json.Marshal(oldManifest)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO platform.modules(id,version,schema_name,state,manifest) VALUES('example.dispatch_quality','1.0.0','mod_example_dispatch_quality','active',$1::jsonb)`, string(oldJSON))
	for _, migration := range []string{"001_create_checks", "002_document_facts"} {
		raw, err := os.ReadFile("../../modules/dispatch-quality/migrations/" + migration + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(raw)
		f.exec(`INSERT INTO platform.module_migrations(module_id,migration_id,checksum) VALUES('example.dispatch_quality',$1,$2)`, migration, hex.EncodeToString(digest[:]))
	}
	f.exec(`INSERT INTO mod_example_dispatch_quality.checks(organization_id,warehouse_id,document_id,checked_content_revision,policy_revision,decision,reviewer_id,evidence_reference,expires_at)
      SELECT organization_id,warehouse_id,id,content_revision,1,'approved',$2,'upgrade-preserved-evidence',clock_timestamp()+interval '1 hour' FROM warehouse.documents WHERE id=$1`, f.document, f.actor)
	return newManifest
}

func upgradeHistory(f *fixture) string {
	return f.scalar(`SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY migration_id),'[]'::jsonb)::text FROM platform.module_migrations m WHERE module_id='example.dispatch_quality'`)
}

func upgradeChecks(f *fixture) string {
	return f.scalar(`SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY id),'[]'::jsonb)::text FROM mod_example_dispatch_quality.checks c WHERE document_id=$1`, f.document)
}

func TestDispatchQualityUpgradePreservesDataAndHistory(t *testing.T) {
	sql := upgradeDispatchSQL(t)
	f := newFixture(t)
	newManifest := seedDispatchUpgrade(t, f)
	historyBefore, checksBefore := upgradeHistory(f), upgradeChecks(f)
	for _, name := range []string{"upgrade from 1.0.0", "repeat exact 1.1.0"} {
		t.Run(name, func(t *testing.T) {
			if err := runDispatchUpgrade(t, f, sql); err != nil {
				t.Fatal(err)
			}
			if version := f.scalar(`SELECT version FROM platform.modules WHERE id='example.dispatch_quality'`); version != "1.1.0" {
				t.Fatalf("version=%s", version)
			}
			if equal := f.scalar(`SELECT (manifest=$1::jsonb)::text FROM platform.modules WHERE id='example.dispatch_quality'`, string(newManifest)); equal != "true" {
				t.Fatal("upgrade did not install the exact bundled manifest")
			}
			if upgradeHistory(f) != historyBefore || upgradeChecks(f) != checksBefore {
				t.Fatal("upgrade changed migration history or existing approval data")
			}
		})
	}
	installed, err := f.store.InstalledModules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := modules.NewRegistry(installed, []modules.Module{legacyDispatchModule()}, []string{dispatchquality.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ValidateModules(t.Context(), registry); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchQualityUpgradeRejectsUnexpectedInstallation(t *testing.T) {
	sql := upgradeDispatchSQL(t)
	for _, tc := range []struct{ name, mutate string }{
		{name: "checksum mismatch", mutate: `UPDATE platform.module_migrations SET checksum=repeat('0',64) WHERE module_id='example.dispatch_quality' AND migration_id='001_create_checks'`},
		{name: "missing migration", mutate: `DELETE FROM platform.module_migrations WHERE module_id='example.dispatch_quality' AND migration_id='002_document_facts'`},
		{name: "extra migration", mutate: `INSERT INTO platform.module_migrations(module_id,migration_id,checksum) VALUES('example.dispatch_quality','999_unknown',repeat('0',64))`},
		{name: "future version", mutate: `UPDATE platform.modules SET version='9.0.0',manifest=jsonb_set(manifest,'{version}','"9.0.0"') WHERE id='example.dispatch_quality'`},
		{name: "foreign schema", mutate: `UPDATE platform.modules SET schema_name='mod_foreign' WHERE id='example.dispatch_quality'`},
		{name: "inactive module", mutate: `UPDATE platform.modules SET state='disabled' WHERE id='example.dispatch_quality'`},
		{name: "manifest identity mismatch", mutate: `UPDATE platform.modules SET manifest=jsonb_set(manifest,'{id}','"example.foreign"') WHERE id='example.dispatch_quality'`},
		{name: "manifest version mismatch", mutate: `UPDATE platform.modules SET manifest=jsonb_set(manifest,'{version}','"0.9.0"') WHERE id='example.dispatch_quality'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			seedDispatchUpgrade(t, f)
			f.exec(tc.mutate)
			moduleBefore := f.scalar(`SELECT to_jsonb(m)::text FROM platform.modules m WHERE id='example.dispatch_quality'`)
			historyBefore, checksBefore := upgradeHistory(f), upgradeChecks(f)
			if err := runDispatchUpgrade(t, f, sql); err == nil {
				t.Fatal("unexpected installation was upgraded")
			}
			if after := f.scalar(`SELECT to_jsonb(m)::text FROM platform.modules m WHERE id='example.dispatch_quality'`); after != moduleBefore || upgradeHistory(f) != historyBefore || upgradeChecks(f) != checksBefore {
				t.Fatal("failed upgrade did not roll back completely")
			}
		})
	}
	t.Run("altered target manifest", func(t *testing.T) {
		f := newFixture(t)
		newManifest := seedDispatchUpgrade(t, f)
		f.exec(`UPDATE platform.modules SET version='1.1.0',manifest=$1::jsonb || '{"description":"altered"}'::jsonb WHERE id='example.dispatch_quality'`, string(newManifest))
		before := f.scalar(`SELECT manifest::text FROM platform.modules WHERE id='example.dispatch_quality'`)
		if err := runDispatchUpgrade(t, f, sql); err == nil {
			t.Fatal("altered target manifest was silently overwritten")
		}
		if after := f.scalar(`SELECT manifest::text FROM platform.modules WHERE id='example.dispatch_quality'`); before != after {
			t.Fatal("failed repeat changed the target manifest")
		}
	})
}

// Preserve upgrade coverage for the previously shipped package and its exact history.
func legacyDispatchModule() modules.Module {
	m := dispatchquality.New()
	m.Version = "1.1.0"
	m.SchemaVersion = 2
	delete(m.MigrationChecksums, "003_authoritative_guard")
	return m
}
