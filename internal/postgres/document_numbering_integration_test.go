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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/jackc/pgx/v5"
)

func numberingFixture(t *testing.T) (*fixture, inventory.Principal, *modules.Registry) {
	t.Helper()
	f := newFixture(t)
	f.exec(`UPDATE warehouse.warehouses SET timezone='Europe/Warsaw' WHERE id=$1`, f.wh)
	f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.create')`, f.actor, f.org, f.wh, f.owner)
	p := inventory.Principal{ActorID: f.actor, Grants: []inventory.Grant{{OrganizationID: f.org, WarehouseID: f.wh, OwnerID: f.owner, Permission: "document.create"}}}
	registry, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f, p, registry
}

func numberingInput(f *fixture, kind, number string) inventory.DocumentInput {
	return inventory.DocumentInput{OrganizationID: f.org, WarehouseID: f.wh, OwnerID: f.owner, Kind: kind,
		Number: number, Lines: []inventory.DocumentLineInput{{ItemID: f.item, Quantity: "1"}}}
}

func numberedCreate(t *testing.T, f *fixture, p inventory.Principal, registry *modules.Registry, command string, input inventory.DocumentInput) (string, json.RawMessage) {
	t.Helper()
	raw, err := f.store.CommandDocument(t.Context(), p, command, "", input, registry)
	if err != nil {
		t.Fatalf("warehouse document create failed: %v", err)
	}
	var result struct {
		DocumentID string `json:"document_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return f.scalar(`SELECT number FROM warehouse.documents WHERE id=$1`, result.DocumentID), raw
}

func TestDocumentAutomaticNumberingSeparateSeriesAndReplay(t *testing.T) {
	f, principal, registry := numberingFixture(t)
	year := f.scalar(`SELECT extract(year FROM transaction_timestamp() AT TIME ZONE timezone)::integer::text FROM warehouse.warehouses WHERE id=$1`, f.wh)
	key := id()
	first, saved := numberedCreate(t, f, principal, registry, key, numberingInput(f, "receipt", ""))
	if want := "PZ/" + year + "/000001"; first != want {
		t.Fatalf("first receipt=%q want=%q", first, want)
	}
	again, replay := numberedCreate(t, f, principal, registry, key, numberingInput(f, "receipt", ""))
	if again != first || string(replay) != string(saved) {
		t.Fatal("same command allocated or returned a different document number")
	}
	second, _ := numberedCreate(t, f, principal, registry, id(), numberingInput(f, "receipt", ""))
	transfer, _ := numberedCreate(t, f, principal, registry, id(), numberingInput(f, "transfer", ""))
	if second != "PZ/"+year+"/000002" || transfer != "MM/"+year+"/000001" {
		t.Fatalf("receipt and putaway series are not independent: receipt=%q transfer=%q", second, transfer)
	}
	other := id()
	f.exec(`INSERT INTO warehouse.warehouses(id,organization_id,code,timezone) VALUES($1,$2,$1::uuid::text,'Europe/Warsaw')`, other, f.org)
	f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.create')`, f.actor, f.org, other, f.owner)
	principal.Grants = append(principal.Grants, inventory.Grant{OrganizationID: f.org, WarehouseID: other, OwnerID: f.owner, Permission: "document.create"})
	input := numberingInput(f, "receipt", "")
	input.WarehouseID = other
	newWarehouse, _ := numberedCreate(t, f, principal, registry, id(), input)
	if newWarehouse != "PZ/"+year+"/000001" {
		t.Fatalf("another warehouse inherited the first warehouse counter: %q", newWarehouse)
	}
}

func TestDocumentAutomaticNumberingManualReservationAndRollback(t *testing.T) {
	f, principal, registry := numberingFixture(t)
	year := f.scalar(`SELECT extract(year FROM transaction_timestamp() AT TIME ZONE 'Europe/Warsaw')::integer::text`)
	manual := "PZ/" + year + "/000041"
	got, _ := numberedCreate(t, f, principal, registry, id(), numberingInput(f, "receipt", manual))
	if got != manual {
		t.Fatal("manual legacy document number was rewritten")
	}
	automatic, _ := numberedCreate(t, f, principal, registry, id(), numberingInput(f, "receipt", ""))
	if automatic != "PZ/"+year+"/000042" {
		t.Fatalf("automatic series did not reserve existing manual suffix: %q", automatic)
	}
	bad := numberingInput(f, "receipt", "")
	bad.Lines[0].Quantity = "0"
	badID := id()
	if _, err := f.store.CommandDocument(t.Context(), principal, badID, "", bad, registry); err == nil {
		t.Fatal("invalid generated-number document was accepted")
	}
	if count := f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, badID); count != "0" {
		t.Fatal("rolled-back document left its operation")
	}
	tx, err := f.admin.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(numberingInput(f, "receipt", ""))
	if _, err := tx.Exec(t.Context(), `SELECT warehouse.command_document($1,$2,NULL,$3::jsonb)`, id(), f.actor, raw); err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := numberedCreate(t, f, principal, registry, id(), numberingInput(f, "receipt", ""))
	if after != "PZ/"+year+"/000043" {
		t.Fatalf("a rejected or rolled-back transaction consumed a number: %q", after)
	}
	high := "MM/" + year + "/1000000"
	numberedCreate(t, f, principal, registry, id(), numberingInput(f, "transfer", high))
	next, _ := numberedCreate(t, f, principal, registry, id(), numberingInput(f, "transfer", ""))
	if next != "MM/"+year+"/1000001" {
		t.Fatalf("the seventh serial digit was lost: %q", next)
	}
}

func TestDocumentAutomaticNumberingConcurrentDistinctCommands(t *testing.T) {
	f, principal, registry := numberingFixture(t)
	year := f.scalar(`SELECT extract(year FROM transaction_timestamp() AT TIME ZONE 'Europe/Warsaw')::integer::text`)
	const total = 24
	type reply struct {
		data json.RawMessage
		err  error
	}
	responses := make(chan reply, total)
	for range total {
		go func() {
			raw, err := f.store.CommandDocument(t.Context(), principal, id(), "", numberingInput(f, "receipt", ""), registry)
			responses <- reply{raw, err}
		}()
	}
	var numbers []string
	for range total {
		response := <-responses
		if response.err != nil {
			t.Fatal(response.err)
		}
		var result struct {
			ID string `json:"document_id"`
		}
		if err := json.Unmarshal(response.data, &result); err != nil {
			t.Fatal(err)
		}
		numbers = append(numbers, f.scalar(`SELECT number FROM warehouse.documents WHERE id=$1`, result.ID))
	}
	sort.Strings(numbers)
	for index, number := range numbers {
		if want := fmt.Sprintf("PZ/%s/%06d", year, index+1); number != want {
			t.Fatalf("concurrent serial %d=%q want=%q", index+1, number, want)
		}
	}
}

func TestDocumentAutomaticNumberingUpgradeSeedsHistoryWithoutRenumbering(t *testing.T) {
	f, _, _ := numberingFixture(t)
	name := "wms_kernel_test_number_seed_" + strings.ReplaceAll(id(), "-", "")
	if _, err := f.admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER wms_test_owner"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := f.admin.Exec(cleanup, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error("owned historical fixture cleanup failed: ", err)
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
	build := exec.CommandContext(t.Context(), python, "-c", `from pathlib import Path; import sys; sys.path.insert(0,'scripts'); from migrate_core import build_script; print('SET ROLE wms_test_owner;'); print(build_script(Path('.'),through='0009'))`)
	build.Dir = root
	build.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
	baseline, err := build.Output()
	if err != nil {
		t.Fatal("cannot construct the immutable pre-numbering migration fixture: ", err)
	}
	psql, err := exec.LookPath("psql")
	if err != nil && runtime.GOOS == "windows" {
		psql = `C:\Program Files\PostgreSQL\17\bin\psql.exe`
		_, err = os.Stat(psql)
	}
	if err != nil {
		t.Fatal("historical migration verification requires psql: ", err)
	}
	env := append(os.Environ(), "PGHOST="+f.admin.Config().ConnConfig.Host,
		fmt.Sprint("PGPORT=", f.admin.Config().ConnConfig.Port), "PGUSER="+f.admin.Config().ConnConfig.User,
		"PGPASSWORD="+f.admin.Config().ConnConfig.Password, "PGDATABASE="+name, "PGCLIENTENCODING=UTF8")
	run := func(source []byte) {
		t.Helper()
		command := exec.CommandContext(t.Context(), psql, "-X", "-q", "-v", "ON_ERROR_STOP=1")
		command.Dir, command.Env, command.Stdin = root, env, bytes.NewReader(source)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("historical numbering migration fixture failed: %v\n%s", err, output)
		}
	}
	run(baseline)
	config := f.admin.Config().ConnConfig.Copy()
	config.Database = name
	connection, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	org, wh, owner := id(), id(), id()
	if _, err := connection.Exec(t.Context(), `INSERT INTO platform.organizations(id,code,name) VALUES($1,$1::uuid::text,'Historical numbering fixture')`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(t.Context(), `INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, wh, org); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(t.Context(), `INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, owner, org); err != nil {
		t.Fatal(err)
	}
	for _, document := range []struct{ kind, number string }{{"receipt", "PZ/2025/000041"}, {"transfer", "MM/2025/000087"}, {"receipt", "PZ/0000/000123"}, {"receipt", "PZ/2025/999999999999999999999999"}, {"receipt", "Faktura zewnętrzna Żółć / 17"}} {
		if _, err := connection.Exec(t.Context(), `INSERT INTO warehouse.documents(organization_id,warehouse_id,owner_id,kind,number,state) VALUES($1,$2,$3,$4,$5,'draft')`, org, wh, owner, document.kind, document.number); err != nil {
			t.Fatal(err)
		}
	}
	var before, after string
	if err := connection.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(d) ORDER BY id)::text FROM warehouse.documents d`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var oidBefore, oidAfter uint32
	if err := connection.QueryRow(t.Context(), `SELECT 'warehouse.command_document(uuid,uuid,uuid,jsonb)'::regprocedure::oid`).Scan(&oidBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(t.Context(), `GRANT EXECUTE ON FUNCTION warehouse.command_document(uuid,uuid,uuid,jsonb) TO wms_go_runtime`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile(filepath.Join(root, "db/migrations/0010_document_numbering.sql"))
	if err != nil {
		t.Fatal(err)
	}
	run(append([]byte("SET ROLE wms_test_owner;\n"), migration...))
	if err := connection.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(d) ORDER BY id)::text FROM warehouse.documents d`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := connection.QueryRow(t.Context(), `SELECT 'warehouse.command_document(uuid,uuid,uuid,jsonb)'::regprocedure::oid`).Scan(&oidAfter); err != nil {
		t.Fatal(err)
	}
	var acl bool
	if err := connection.QueryRow(t.Context(), `SELECT has_function_privilege('wms_go_runtime','warehouse.command_document(uuid,uuid,uuid,jsonb)','EXECUTE')`).Scan(&acl); err != nil {
		t.Fatal(err)
	}
	if before != after || oidBefore != oidAfter || !acl {
		t.Fatal("numbering upgrade rewrote documents or replaced the published command identity/ACL")
	}
	for _, check := range []struct{ kind, want string }{{"receipt", "PZ/2025/000042"}, {"transfer", "MM/2025/000088"}} {
		var number string
		if err := connection.QueryRow(t.Context(), `SELECT warehouse.allocate_document_number($1,$2,$3,'2025-06-01 12:00:00+00'::timestamptz)`, org, wh, check.kind).Scan(&number); err != nil {
			t.Fatal(err)
		}
		if number != check.want {
			t.Fatalf("historical manual seed for %s: got %q want %q", check.kind, number, check.want)
		}
	}
}
