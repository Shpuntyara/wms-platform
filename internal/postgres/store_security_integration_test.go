//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"github.com/Shpuntyara/wms-platform/internal/clientmodules/dispatchquality"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"os"
	"strings"
	"testing"
	"time"
)

func installSecurityModule(t *testing.T, f *fixture) {
	t.Helper()
	m := dispatchquality.Manifest()
	raw, _ := json.Marshal(m)
	f.exec(`INSERT INTO platform.modules(id,version,schema_name,state,manifest) VALUES($1,$2,$3,'active',$4)`, m.ID, m.Version, m.Data.Schema, raw)
	t.Cleanup(func() { _, _ = f.admin.Exec(context.Background(), `DELETE FROM platform.modules WHERE id=$1`, m.ID) })
}

func TestSecurityDirectSQLCannotBypassDispatchGuard(t *testing.T) {
	f := newFixture(t)
	var result []byte
	err := f.runtime.QueryRow(t.Context(), `SELECT inventory.reserve_stock($1,$2,$3,1,$4,2)`, id(), f.actor, f.line, f.source).Scan(&result)
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	_ = json.Unmarshal(result, &r)
	installSecurityModule(t, f)
	command := id()
	_, err = f.runtime.Exec(t.Context(), `SELECT inventory.post_movement($1,$2,$3,2,'issue',$4,NULL,1,$5)`, command, f.actor, f.line, f.source, r["reservation_id"])
	if err == nil {
		t.Fatal("direct SQL bypassed required quality guard")
	}
	if f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, command) != "0" {
		t.Fatal("denied command committed")
	}
}

func TestSecurityDisabledModuleStillBlocksSQL(t *testing.T) {
	f := newFixture(t)
	installSecurityModule(t, f)
	f.exec(`UPDATE platform.modules SET state='disabled' WHERE id='example.dispatch_quality'`)
	_, err := f.runtime.Exec(t.Context(), `SELECT inventory.reserve_stock($1,$2,$3,1,$4,1)`, id(), f.actor, f.line, f.source)
	if err == nil {
		t.Fatal("disabling required module silently removed enforcement")
	}
}

func TestSecurityRegistryCannotOmitInstalledModule(t *testing.T) {
	f := newFixture(t)
	installSecurityModule(t, f)
	r, _ := modules.NewRegistry(nil, nil, nil)
	if err := f.store.ValidateModules(t.Context(), r); err == nil {
		t.Fatal("empty registry accepted installed required module")
	}
}

func TestSecurityExpiryReleasesExactlyOnce(t *testing.T) {
	f := newFixture(t)
	f.exec(`SELECT inventory.reserve_stock($1,$2,$3,1,$4,3,clock_timestamp()+interval '50 milliseconds')`, id(), f.actor, f.line, f.source)
	time.Sleep(70 * time.Millisecond)
	for range 2 {
		if _, err := f.runtime.Exec(t.Context(), `SELECT inventory.expire_reservations(10)`); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.scalar(`SELECT (reserved_qty=0)::text FROM inventory.stock_balances WHERE id=$1`, f.source); got != "true" {
		t.Fatalf("expired stock still held: %s", got)
	}
	if got := f.scalar(`SELECT count(*) FROM inventory.reservation_events WHERE reservation_id IN (SELECT id FROM inventory.reservations WHERE document_line_id=$1) AND reason='expired'`, f.line); got != "1" {
		t.Fatalf("expiry events=%s", got)
	}
	if got := f.scalar(`SELECT (reserved_qty=0)::text FROM warehouse.document_lines WHERE id=$1`, f.line); got != "true" {
		t.Fatal(got)
	}
}

func TestSecurityQualityRequiresAuthorizedIndependentReviewerAndCurrentPolicy(t *testing.T) {
	f := newFixture(t)
	var raw []byte
	if err := f.runtime.QueryRow(t.Context(), `SELECT inventory.reserve_stock($1,$2,$3,1,$4,3)`, id(), f.actor, f.line, f.source).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	_ = json.Unmarshal(raw, &r)
	installSecurityModule(t, f)
	reviewer := id()
	f.exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'https://tests.invalid',$1::uuid::text)`, reviewer)
	f.exec(`INSERT INTO mod_example_dispatch_quality.policies(organization_id,warehouse_id,revision) VALUES($1,$2,7)`, f.org, f.wh)
	approve := func(who string, policy int) {
		f.exec(`INSERT INTO mod_example_dispatch_quality.checks(organization_id,warehouse_id,document_id,checked_content_revision,policy_revision,decision,reviewer_id,evidence_reference,expires_at) VALUES($1,$2,$3,2,$4,'approved',$5,'https://evidence.invalid/check',clock_timestamp()+interval '1 hour')`, f.org, f.wh, f.document, policy, who)
	}
	issue := func() error {
		_, err := f.runtime.Exec(t.Context(), `SELECT inventory.post_movement($1,$2,$3,2,'issue',$4,NULL,1,$5)`, id(), f.actor, f.line, f.source, r["reservation_id"])
		return err
	}
	approve(reviewer, 7)
	if issue() == nil {
		t.Fatal("unauthorized reviewer approved dispatch")
	}
	f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'quality.approve'),($5,$2,$3,$4,'quality.approve')`, reviewer, f.org, f.wh, f.owner, f.actor)
	approve(f.actor, 7)
	if issue() == nil {
		t.Fatal("self approval accepted")
	}
	approve(reviewer, 6)
	if issue() == nil {
		t.Fatal("obsolete policy accepted")
	}
	approve(reviewer, 7)
	if err := issue(); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityOutboxLeaseFencesStaleAcknowledgement(t *testing.T) {
	f := newFixture(t)
	var event, token string
	var payload []byte
	var attempt int
	err := f.runtime.QueryRow(t.Context(), `SELECT id::text,payload,token::text,attempt FROM platform.claim_outbox(1)`).Scan(&event, &payload, &token, &attempt)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE platform.outbox SET leased_until=clock_timestamp()-interval '1 second' WHERE id=$1`, event)
	var accepted bool
	if err := f.runtime.QueryRow(t.Context(), `SELECT platform.complete_outbox($1,$2,true)`, event, token).Scan(&accepted); err != nil || accepted {
		t.Fatalf("stale ack accepted=%v err=%v", accepted, err)
	}
	// Claim only this event again to check that a lease generation changes.
	f.exec(`UPDATE platform.outbox SET available_at=clock_timestamp()-interval '1 day' WHERE id=$1`, event)
	var next string
	if err := f.runtime.QueryRow(t.Context(), `SELECT token::text FROM platform.claim_outbox(1)`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next == token {
		t.Fatal("lease token reused")
	}
	if err := f.runtime.QueryRow(t.Context(), `SELECT platform.complete_outbox($1,$2,true)`, event, next).Scan(&accepted); err != nil || !accepted {
		t.Fatalf("current ack=%v err=%v", accepted, err)
	}
}

func TestSecurityRuntimeCannotOwnModuleExecutionSurface(t *testing.T) {
	f := newFixture(t)
	f.exec(`GRANT CREATE ON SCHEMA mod_example_dispatch_quality TO wms_test_runtime`)
	t.Cleanup(func() {
		_, _ = f.admin.Exec(context.Background(), `REVOKE CREATE ON SCHEMA mod_example_dispatch_quality FROM wms_test_runtime`)
	})
	if _, err := postgres.New(t.Context(), f.runtime); err == nil {
		t.Fatal("runtime can replace a module execution surface")
	}
}

func TestSecurityDeclarativePolicyEnforcedByAPIAndSQL(t *testing.T) {
	f := newFixture(t)
	f.exec(`CREATE SCHEMA mod_security_dynamic;CREATE TABLE mod_security_dynamic.facts(organization_id uuid,warehouse_id uuid,document_id uuid,content_revision bigint,payload jsonb);CREATE VIEW mod_security_dynamic.document_facts_v1 AS SELECT * FROM mod_security_dynamic.facts;GRANT USAGE ON SCHEMA mod_security_dynamic TO wms_test_runtime,wms_test_owner;GRANT SELECT ON mod_security_dynamic.document_facts_v1 TO wms_test_runtime,wms_test_owner`)
	t.Cleanup(func() {
		_, _ = f.admin.Exec(context.Background(), `DELETE FROM platform.modules WHERE id='security.dynamic';DROP SCHEMA mod_security_dynamic CASCADE`)
	})
	m := modules.Manifest{ManifestVersion: 1, ID: "security.dynamic", Version: "1.0.0", Runtime: "declarative_v1", Compatibility: modules.Compatibility{Core: ">=0.3.0 <1.0.0", SDK: ">=2.0.0 <3.0.0"}, Data: modules.ManifestData{Schema: "mod_security_dynamic", TargetSchemaVersion: 1}, Guards: []modules.GuardSpec{{Point: modules.ReservationPoint, Handler: "Allow", Required: true, FailurePolicy: "deny", TimeoutMS: 100}}, Policies: []json.RawMessage{json.RawMessage(`{"handler":"Allow","conditions":[{"field":"decision","op":"eq","value":"allowed"}]}`)}}
	raw, _ := json.Marshal(m)
	f.exec(`INSERT INTO platform.modules(id,version,schema_name,state,manifest) VALUES($1,$2,$3,'active',$4)`, m.ID, m.Version, m.Data.Schema, raw)
	f.exec(`INSERT INTO mod_security_dynamic.facts VALUES($1,$2,$3,1,'{"decision":"denied"}')`, f.org, f.wh, f.document)
	registry, err := modules.NewRegistry([]modules.Manifest{m}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ValidateModules(t.Context(), registry); err != nil {
		t.Fatal(err)
	}
	h := f.handler([]modules.Manifest{m}, nil)
	requireStatus(t, f.request(h, "POST", "/api/v1/reservations", f.reserveBody(f.line, "1", 1), id()), 409)
	if _, err := f.runtime.Exec(t.Context(), `SELECT inventory.reserve_stock($1,$2,$3,1,$4,1)`, id(), f.actor, f.line, f.source); err == nil {
		t.Fatal("SQL bypassed declarative policy")
	}
	f.exec(`UPDATE mod_security_dynamic.facts SET payload='{"decision":"allowed"}'`)
	requireStatus(t, f.request(h, "POST", "/api/v1/reservations", f.reserveBody(f.line, "1", 1), id()), 201)
	// Removing a guard from the installed manifest must not remove its requirement.
	f.exec(`UPDATE platform.modules SET manifest=jsonb_set(manifest,'{guards}','[]') WHERE id=$1`, m.ID)
	if _, err := f.runtime.Exec(t.Context(), `SELECT inventory.reserve_stock($1,$2,$3,2,$4,1)`, id(), f.actor, f.line, f.source); err == nil {
		t.Fatal("removing guard bypassed retained policy")
	}
}

func TestSecurityDispatchUpgradeTo12(t *testing.T) {
	f := newFixture(t)
	_ = seedDispatchUpgrade(t, f)
	if err := runDispatchUpgrade(t, f, upgradeDispatchSQL(t)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../db/examples/upgrade_dispatch_quality_1_2.psql")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, `\`) || line == "BEGIN;" || line == "COMMIT;" {
			continue
		}
		lines = append(lines, line)
	}
	for range 2 {
		if err := runDispatchUpgrade(t, f, strings.Join(lines, "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.scalar(`SELECT version FROM platform.modules WHERE id='example.dispatch_quality'`); got != "1.2.0" {
		t.Fatal(got)
	}
	installed, err := f.store.InstalledModules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r, err := modules.NewRegistry(installed, []modules.Module{dispatchquality.New()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ValidateModules(t.Context(), r); err != nil {
		t.Fatal(err)
	}
}
