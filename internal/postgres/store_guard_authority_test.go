//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"testing"
)

func installSQLTestGuard(t *testing.T, f *fixture, point, mode string, guard modules.Guard) ([]modules.Manifest, []modules.Module) {
	t.Helper()
	manifests, code := guarded(point, guard)
	manifests[0].Data = modules.ManifestData{Schema: "mod_test_required", TargetSchemaVersion: 1}
	code[0].SchemaVersion = 1
	f.exec(`CREATE SCHEMA mod_test_required;
 CREATE TABLE mod_test_required.control(mode text,calls integer);
 CREATE FUNCTION mod_test_required.enforce(uuid,uuid) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
 DECLARE m text; BEGIN
 UPDATE mod_test_required.control SET calls=calls+1 RETURNING mode INTO m;
 IF m='deny' THEN RAISE EXCEPTION 'module_guard_denied'; END IF;
 IF m='error' THEN RAISE EXCEPTION 'internal_test_error'; END IF;
 IF m='wait' THEN PERFORM pg_sleep(30); END IF;
 END $$;
 GRANT USAGE ON SCHEMA mod_test_required TO wms_test_owner;
 GRANT EXECUTE ON FUNCTION mod_test_required.enforce(uuid,uuid) TO wms_test_owner`)
	f.exec(`INSERT INTO mod_test_required.control VALUES($1,0)`, mode)
	raw, _ := json.Marshal(manifests[0])
	f.exec(`INSERT INTO platform.modules(id,version,schema_name,state,manifest) VALUES('test.required','1.0.0','mod_test_required','active',$1)`, raw)
	f.exec(`INSERT INTO platform.sql_guards VALUES('test.required',$1,'Check','mod_test_required.enforce(uuid,uuid)'::regprocedure)`, point)
	t.Cleanup(func() {
		_, _ = f.admin.Exec(context.Background(), `DELETE FROM platform.modules WHERE id='test.required'; DELETE FROM platform.sql_guards WHERE module_id='test.required'; DROP SCHEMA mod_test_required CASCADE`)
	})
	return manifests, code
}
func TestSQLGuardIsOnlyBusinessDecision(t *testing.T) {
	f := newFixture(t)
	manifests, code := installSQLTestGuard(t, f, modules.ReservationPoint, "allow", func(context.Context, modules.Check) error {
		t.Error("Go business guard executed alongside authoritative SQL")
		return &modules.Denial{Code: "divergent_go_rule"}
	})
	requireStatus(t, f.request(f.handler(manifests, code), "POST", "/api/v1/reservations", f.reserveBody(f.line, "1", 1), id()), 201)
	if got := f.scalar(`SELECT calls FROM mod_test_required.control`); got != "1" {
		t.Fatal(got)
	}
}

func TestUninstalledHostGuardsCannotAuthorizeCommands(t *testing.T) {
	f := newFixture(t)
	manifests, code := guarded(modules.ReservationPoint, func(context.Context, modules.Check) error { return nil })
	requireStatus(t, f.request(f.handler(manifests, code), "POST", "/api/v1/reservations", f.reserveBody(f.line, "1", 1), id()), 503)
	if f.scalar(`SELECT reserved_qty::text FROM inventory.stock_balances WHERE id=$1`, f.source) != "0.000000" {
		t.Fatal("uninstalled guard authorized stock change")
	}
}
