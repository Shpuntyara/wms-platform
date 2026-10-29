//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDocumentBusinessUpgradePreservesHistoricalShipmentReplayAndSeedsWZ(t *testing.T) {
	f := newFixture(t)
	name := "wms_kernel_test_business_upgrade_" + strings.ReplaceAll(id(), "-", "")
	if _, err := f.admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER wms_test_owner"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := f.admin.Exec(cleanup, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error("owned historical business fixture cleanup failed: ", err)
		}
	})
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	python := "python3"
	if runtime.GOOS == "windows" {
		python = "python"
	}
	psql, err := exec.LookPath("psql")
	if err != nil && runtime.GOOS == "windows" {
		psql = `C:\Program Files\PostgreSQL\17\bin\psql.exe`
		_, err = os.Stat(psql)
	}
	if err != nil {
		t.Fatal("historical business migration verification requires psql: ", err)
	}
	env := append(os.Environ(), "PGHOST="+f.admin.Config().ConnConfig.Host,
		fmt.Sprint("PGPORT=", f.admin.Config().ConnConfig.Port), "PGUSER="+f.admin.Config().ConnConfig.User,
		"PGPASSWORD="+f.admin.Config().ConnConfig.Password, "PGDATABASE="+name, "PGCLIENTENCODING=UTF8")
	migrate := func(through string) {
		t.Helper()
		build := exec.CommandContext(t.Context(), python, "-c", `from pathlib import Path; import sys; sys.path.insert(0,'scripts'); from migrate_core import build_script; print('SET ROLE wms_test_owner;'); print(build_script(Path('.'),through=sys.argv[1]))`, through)
		build.Dir, build.Env = root, append(os.Environ(), "PYTHONIOENCODING=utf-8")
		source, err := build.Output()
		if err != nil {
			t.Fatal("cannot construct historical business migration fixture: ", err)
		}
		command := exec.CommandContext(t.Context(), psql, "-X", "-q", "-v", "ON_ERROR_STOP=1")
		command.Dir, command.Env, command.Stdin = root, env, bytes.NewReader(source)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("official business upgrade fixture failed: %v\n%s", err, output)
		}
	}
	migrate("0011")
	config := f.admin.Config().ConnConfig.Copy()
	config.Database = name
	connection, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	org, wh, owner, actor, item, command := id(), id(), id(), id(), id(), id()
	execSQL := func(query string, args ...any) {
		t.Helper()
		if _, err := connection.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	execSQL(`INSERT INTO platform.organizations(id,code,name) VALUES($1,$1::uuid::text,'Historical WZ fixture')`, org)
	execSQL(`INSERT INTO warehouse.warehouses(id,organization_id,code,timezone) VALUES($1,$2,$1::uuid::text,'Europe/Warsaw')`, wh, org)
	execSQL(`INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, owner, org)
	execSQL(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'historical-business',$1::uuid::text)`, actor)
	execSQL(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.create')`, actor, org, wh, owner)
	execSQL(`INSERT INTO catalog.units(code,dimension) VALUES('szt','count')`)
	execSQL(`INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES($1,$2,'SRUBA-M8','szt',0,'none')`, item, org)
	var year int
	if err := connection.QueryRow(t.Context(), `SELECT extract(year FROM transaction_timestamp() AT TIME ZONE 'Europe/Warsaw')::integer`).Scan(&year); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"organization_id": org, "warehouse_id": wh, "owner_id": owner, "kind": "shipment", "number": fmt.Sprintf("WZ/%d/000041", year), "lines": []any{map[string]any{"item_id": item, "quantity": "1"}}}
	raw, _ := json.Marshal(body)
	var original json.RawMessage
	if err := connection.QueryRow(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, command, actor, raw).Scan(&original); err != nil {
		t.Fatal(err)
	}
	var before, after string
	snapshot := `SELECT jsonb_build_object('document',to_jsonb(d)%s,'operation',to_jsonb(o))::text FROM warehouse.documents d JOIN platform.operations o ON d.id=(o.result->>'document_id')::uuid WHERE o.id=$1`
	if err := connection.QueryRow(t.Context(), fmt.Sprintf(snapshot, ""), command).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var oidBefore, oidAfter uint32
	if err := connection.QueryRow(t.Context(), `SELECT 'warehouse.command_document(uuid,uuid,uuid,jsonb)'::regprocedure::oid`).Scan(&oidBefore); err != nil {
		t.Fatal(err)
	}
	execSQL(`GRANT EXECUTE ON FUNCTION warehouse.command_document(uuid,uuid,uuid,jsonb) TO wms_go_runtime`)
	migrate("0012")
	if err := connection.QueryRow(t.Context(), fmt.Sprintf(snapshot, "-'recipient'-'order_reference'"), command).Scan(&after); err != nil {
		t.Fatal(err)
	}
	var hasACL bool
	if err := connection.QueryRow(t.Context(), `SELECT 'warehouse.command_document(uuid,uuid,uuid,jsonb)'::regprocedure::oid,has_function_privilege('wms_go_runtime','warehouse.command_document(uuid,uuid,uuid,jsonb)','EXECUTE')`).Scan(&oidAfter, &hasACL); err != nil {
		t.Fatal(err)
	}
	if before != after || oidBefore != oidAfter || !hasACL {
		t.Fatal("business upgrade changed historical shipment/operation/command identity or privileges")
	}
	var replay json.RawMessage
	if err := connection.QueryRow(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, command, actor, raw).Scan(&replay); err != nil || string(replay) != string(original) {
		t.Fatalf("historical missing-recipient command cannot replay its original receipt: %v", err)
	}
	if _, err := connection.Exec(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, id(), actor, raw); err == nil {
		t.Fatal("new submission reused the historical recipient exemption")
	}
	delete(body, "number")
	body["recipient"] = "Firma Alfa — Piotr Nowak"
	raw, _ = json.Marshal(body)
	var created json.RawMessage
	if err := connection.QueryRow(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, id(), actor, raw).Scan(&created); err != nil {
		t.Fatal(err)
	}
	var result struct {
		ID string `json:"document_id"`
	}
	if json.Unmarshal(created, &result) != nil {
		t.Fatal("invalid new WZ result")
	}
	var number string
	if err := connection.QueryRow(t.Context(), `SELECT number FROM warehouse.documents WHERE id=$1`, result.ID).Scan(&number); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("WZ/%d/000042", year); number != want {
		t.Fatalf("historical WZ series seed=%q want=%q", number, want)
	}
}
