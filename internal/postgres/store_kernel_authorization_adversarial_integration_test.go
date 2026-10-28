//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/jackc/pgx/v5/pgconn"
)

func installKernelDeclarativeGuard(t *testing.T, f *fixture, policy string) {
	t.Helper()
	f.exec(`CREATE SCHEMA mod_kernel_guard;
 CREATE TABLE mod_kernel_guard.facts(organization_id uuid,warehouse_id uuid,document_id uuid,content_revision bigint,payload jsonb);
 CREATE VIEW mod_kernel_guard.document_facts_v1 AS SELECT * FROM mod_kernel_guard.facts;
 GRANT USAGE ON SCHEMA mod_kernel_guard TO wms_test_owner,wms_test_runtime;
 GRANT SELECT ON mod_kernel_guard.document_facts_v1 TO wms_test_owner,wms_test_runtime`)
	m := modules.Manifest{ManifestVersion: 1, ID: "kernel.guard", Version: "1.0.0", Runtime: "declarative_v2", Compatibility: modules.Compatibility{Core: ">=0.5.0 <1.0.0", SDK: ">=2.0.0 <3.0.0"}, Data: modules.ManifestData{Schema: "mod_kernel_guard", TargetSchemaVersion: 1}, Guards: []modules.GuardSpec{{Point: modules.ReservationPoint, Handler: "Allow", Required: true, FailurePolicy: "deny", TimeoutMS: 100}}, Policies: []json.RawMessage{json.RawMessage(policy)}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO platform.modules(id,version,schema_name,state,manifest) VALUES($1,$2,$3,'active',$4)`, m.ID, m.Version, m.Data.Schema, raw)
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := f.admin.Exec(ctx, `DELETE FROM platform.modules WHERE id='kernel.guard'; DROP SCHEMA mod_kernel_guard CASCADE`)
		if err != nil {
			t.Error(err)
		}
	})
}

func requireKernelGuardDenied(t *testing.T, f *fixture, version int) {
	t.Helper()
	command := id()
	_, err := f.runtime.Exec(t.Context(), `SELECT inventory.reserve_stock($1,$2,$3,$4,$5,1)`, command, f.actor, f.line, version, f.source)
	var problem *pgconn.PgError
	if !errors.As(err, &problem) || problem.Code != "P0001" || problem.Message != "module_guard_denied" {
		t.Fatalf("guard result=%v, want module_guard_denied", err)
	}
	if got := f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, command); got != "0" {
		t.Fatalf("denied guard committed operation: %s", got)
	}
}

func TestKernelAuthorizationDeclarativeFactsRemainScopedAndUnique(t *testing.T) {
	f := newFixture(t)
	installKernelDeclarativeGuard(t, f, `{"handler":"Allow","any":[{"conditions":[{"field":"decision","op":"eq","value":"allow"}]}]}`)
	for _, tc := range []struct {
		name         string
		org, wh, doc string
		revision     int
		rows         int
	}{
		{name: "missing", org: f.org, wh: f.wh, doc: f.document, revision: 1, rows: 0},
		{name: "foreign organization", org: id(), wh: f.wh, doc: f.document, revision: 1, rows: 1},
		{name: "foreign warehouse", org: f.org, wh: id(), doc: f.document, revision: 1, rows: 1},
		{name: "foreign document", org: f.org, wh: f.wh, doc: id(), revision: 1, rows: 1},
		{name: "stale content revision", org: f.org, wh: f.wh, doc: f.document, revision: 0, rows: 1},
		{name: "ambiguous matching facts", org: f.org, wh: f.wh, doc: f.document, revision: 1, rows: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.exec(`DELETE FROM mod_kernel_guard.facts`)
			for range tc.rows {
				f.exec(`INSERT INTO mod_kernel_guard.facts VALUES($1,$2,$3,$4,'{"decision":"allow"}')`, tc.org, tc.wh, tc.doc, tc.revision)
			}
			requireKernelGuardDenied(t, f, 1)
			if got := f.scalar(`SELECT version||':'||content_revision FROM warehouse.documents WHERE id=$1`, f.document); got != "1:1" {
				t.Fatalf("denied policy changed document: %s", got)
			}
		})
	}
	f.exec(`DELETE FROM mod_kernel_guard.facts;`)
	f.exec(`INSERT INTO mod_kernel_guard.facts VALUES($1,$2,$3,1,'{"decision":"allow"}')`, f.org, f.wh, f.document)
	if _, err := f.runtime.Exec(t.Context(), `SELECT inventory.reserve_stock($1,$2,$3,1,$4,1)`, id(), f.actor, f.line, f.source); err != nil {
		t.Fatal(err)
	}
	// A successful change advances the content revision; the old facts cannot
	// authorize another write merely because the document ID is unchanged.
	requireKernelGuardDenied(t, f, 2)
}

func TestKernelAuthorizationDeclarativeAlternativesRequireAllConjuncts(t *testing.T) {
	f := newFixture(t)
	installKernelDeclarativeGuard(t, f, `{"handler":"Allow","any":[{"conditions":[{"field":"decision","op":"eq","value":"allow"},{"field":"evidence","op":"exists"}]},{"conditions":[{"field":"override","op":"is_true"}]}]}`)
	for _, payload := range []string{`{"decision":"allow"}`, `{"evidence":"proof"}`, `{"override":"true"}`, `{"decision":"deny","evidence":"proof","override":false}`} {
		f.exec(`DELETE FROM mod_kernel_guard.facts`)
		f.exec(`INSERT INTO mod_kernel_guard.facts VALUES($1,$2,$3,1,$4)`, f.org, f.wh, f.document, payload)
		requireKernelGuardDenied(t, f, 1)
	}
	for _, payload := range []string{`{"decision":"allow","evidence":"proof"}`, `{"decision":"deny","override":true}`} {
		f.exec(`DELETE FROM mod_kernel_guard.facts`)
		version := f.scalar(`SELECT version FROM warehouse.documents WHERE id=$1`, f.document)
		f.exec(`INSERT INTO mod_kernel_guard.facts VALUES($1,$2,$3,$4,$5)`, f.org, f.wh, f.document, version, payload)
		if _, err := f.runtime.Exec(t.Context(), `SELECT inventory.reserve_stock($1,$2,$3,$4,$5,1)`, id(), f.actor, f.line, version, f.source); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKernelAuthorizationTypedRulesNeverCoerceOrRound(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name, rule, facts string
		want              bool
	}{
		{name: "string equality", rule: `{"field":"v","op":"eq","value":"7"}`, facts: `{"v":"7"}`, want: true},
		{name: "numeric equality is not string", rule: `{"field":"v","op":"eq","value":"7"}`, facts: `{"v":7}`},
		{name: "missing neq is false", rule: `{"field":"v","op":"neq","value":"7"}`, facts: `{}`},
		{name: "unequal strings", rule: `{"field":"v","op":"neq","value":"7"}`, facts: `{"v":"8"}`, want: true},
		{name: "null is absent", rule: `{"field":"v","op":"exists"}`, facts: `{"v":null}`},
		{name: "empty value exists", rule: `{"field":"v","op":"exists"}`, facts: `{"v":""}`, want: true},
		{name: "boolean true", rule: `{"field":"v","op":"is_true"}`, facts: `{"v":true}`, want: true},
		{name: "string true is not boolean", rule: `{"field":"v","op":"is_true"}`, facts: `{"v":"true"}`},
		{name: "decimal gte below threshold", rule: `{"field":"v","op":"decimal_gte","value":"100000000000000000.2"}`, facts: `{"v":"100000000000000000.1"}`},
		{name: "decimal lte above threshold", rule: `{"field":"v","op":"decimal_lte","value":"100000000000000000.1"}`, facts: `{"v":"100000000000000000.2"}`},
		{name: "exact decimal equality", rule: `{"field":"v","op":"decimal_gte","value":"100000000000000000.2"}`, facts: `{"v":"100000000000000000.2"}`, want: true},
		{name: "NaN is not a number", rule: `{"field":"v","op":"decimal_gte","value":"0"}`, facts: `{"v":"NaN"}`},
		{name: "membership", rule: `{"field":"v","op":"in","values":["dry","cold"]}`, facts: `{"v":"cold"}`, want: true},
		{name: "membership denied", rule: `{"field":"v","op":"in","values":["dry","cold"]}`, facts: `{"v":"hot"}`},
		{name: "future must be timestamp", rule: `{"field":"v","op":"future"}`, facts: `{"v":"infinity"}`},
		{name: "empty string denied", rule: `{"field":"v","op":"nonempty"}`, facts: `{"v":"   "}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			if err := f.admin.QueryRow(t.Context(), `SELECT platform.declarative_condition_v2($1::jsonb,$2::jsonb,$3::uuid)`, tc.rule, tc.facts, f.actor).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
	for _, value := range []string{f.actor, strings.ToUpper(f.actor)} {
		var got bool
		facts, _ := json.Marshal(map[string]string{"reviewer": value})
		if err := f.admin.QueryRow(t.Context(), `SELECT platform.declarative_condition_v2('{"field":"reviewer","op":"not_actor"}',$1::jsonb,$2::uuid)`, facts, f.actor).Scan(&got); err != nil || got {
			t.Fatalf("self review accepted: %v %v", got, err)
		}
	}
}
