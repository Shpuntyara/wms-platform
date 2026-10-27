//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Shpuntyara/wms-platform/internal/jobs"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDeclarativeV2SQLAlternativesAndExactDecimals(t *testing.T) {
	f := newFixture(t)
	f.exec(`CREATE SCHEMA mod_security_v2;CREATE TABLE mod_security_v2.facts(organization_id uuid,warehouse_id uuid,document_id uuid,content_revision bigint,payload jsonb);CREATE VIEW mod_security_v2.document_facts_v1 AS SELECT * FROM mod_security_v2.facts;GRANT USAGE ON SCHEMA mod_security_v2 TO wms_test_owner,wms_test_runtime;GRANT SELECT ON mod_security_v2.document_facts_v1 TO wms_test_owner,wms_test_runtime`)
	t.Cleanup(func() {
		_, _ = f.admin.Exec(context.Background(), `DELETE FROM platform.modules WHERE id='security.v2';DROP SCHEMA mod_security_v2 CASCADE`)
	})
	m := modules.Manifest{ManifestVersion: 1, ID: "security.v2", Version: "1.0.0", Runtime: "declarative_v2", Compatibility: modules.Compatibility{Core: ">=0.5.0 <1.0.0", SDK: ">=2.0.0 <3.0.0"}, Data: modules.ManifestData{Schema: "mod_security_v2", TargetSchemaVersion: 1}, Guards: []modules.GuardSpec{{Point: modules.ReservationPoint, Handler: "Allow", Required: true, FailurePolicy: "deny", TimeoutMS: 100}}, Policies: []json.RawMessage{json.RawMessage(`{"handler":"Allow","any":[{"conditions":[{"field":"zone","op":"in","values":["cold","dry"]},{"field":"weight","op":"decimal_lte","value":"999999999999999999.125"},{"field":"weight","op":"decimal_gte","value":"0"}]},{"conditions":[{"field":"override","op":"is_true"},{"field":"evidence","op":"exists"}]}]}`)}}
	raw, _ := json.Marshal(m)
	f.exec(`INSERT INTO platform.modules(id,version,schema_name,state,manifest) VALUES($1,$2,$3,'active',$4)`, m.ID, m.Version, m.Data.Schema, raw)
	h := f.handler([]modules.Manifest{m}, nil)
	for _, tc := range []struct {
		facts string
		allow bool
	}{
		{`{"zone":"dry","weight":"999999999999999999.125"}`, true},
		{`{"zone":"dry","weight":"999999999999999999.126"}`, false},
		{`{"zone":"dry","weight":"NaN"}`, false},
		{`{"zone":"dry","weight":"-1"}`, false},
		{`{"zone":"unknown","weight":"1"}`, false},
		{`{"override":true,"evidence":"check-1"}`, true},
		{`{"override":"true","evidence":"check-1"}`, false},
		{`{"override":true,"evidence":null}`, false},
		{`{}`, false},
	} {
		f.exec(`DELETE FROM mod_security_v2.facts`)
		f.exec(`INSERT INTO mod_security_v2.facts SELECT organization_id,warehouse_id,id,content_revision,$1::jsonb FROM warehouse.documents WHERE id=$2`, tc.facts, f.document)
		version := f.scalar(`SELECT version FROM warehouse.documents WHERE id=$1`, f.document)
		// Evaluate the same authoritative policy through API and direct kernel SQL.
		body := f.reserveBody(f.line, "0.001", 1)
		body["expected_document_version"] = json.Number(version)
		status := 409
		if tc.allow {
			status = 201
		}
		requireStatus(t, f.request(h, "POST", "/api/v1/reservations", body, id()), status)
		f.exec(`UPDATE mod_security_v2.facts SET content_revision=(SELECT content_revision FROM warehouse.documents WHERE id=$1)`, f.document)
		_, err := f.runtime.Exec(t.Context(), `SELECT inventory.reserve_stock($1,$2,$3,$4::bigint,$5,0.001)`, id(), f.actor, f.line, f.scalar(`SELECT version FROM warehouse.documents WHERE id=$1`, f.document), f.source)
		if (err == nil) != tc.allow {
			t.Fatalf("SQL allow=%v facts=%s err=%v", tc.allow, tc.facts, err)
		}
	}
	version := f.scalar(`SELECT version FROM warehouse.documents WHERE id=$1`, f.document)
	body := f.reserveBody(f.line, "0.001", 1)
	body["expected_document_version"] = json.Number(version)
	f.exec(`INSERT INTO mod_security_v2.facts SELECT * FROM mod_security_v2.facts`)
	requireStatus(t, f.request(h, "POST", "/api/v1/reservations", body, id()), 409)
	f.exec(`UPDATE mod_security_v2.facts SET content_revision=0`)
	requireStatus(t, f.request(h, "POST", "/api/v1/reservations", body, id()), 409)
	f.exec(`DELETE FROM mod_security_v2.facts`)
	requireStatus(t, f.request(h, "POST", "/api/v1/reservations", body, id()), 409)

}

type brokerSender struct {
	client *http.Client
	url    string
}

func (s brokerSender) Send(ctx context.Context, e jobs.Event) error {
	r, err := http.NewRequestWithContext(ctx, "POST", s.url, bytes.NewReader(e.Payload))
	if err != nil {
		return err
	}
	response, err := s.client.Do(r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 204 {
		return errors.New("broker failed")
	}
	return nil
}
func TestRelayDatabaseHTTPBurst(t *testing.T) {
	f := newFixture(t)
	f.exec(`INSERT INTO platform.outbox(operation_id,event_type,aggregate_id,payload) SELECT o.id,'test.background.'||g,n.id,jsonb_build_object('event_id',gen_random_uuid()) FROM (SELECT id FROM platform.operations WHERE organization_id=$1 LIMIT 1) o CROSS JOIN (SELECT id FROM warehouse.documents WHERE id=$2) n CROSS JOIN generate_series(1,1000) g`, f.org, f.document)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(time.Millisecond); w.WriteHeader(204) }))
	defer server.Close()
	client := server.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 64
	transport.MaxConnsPerHost = 64
	client.Transport = transport
	defer transport.CloseIdleConnections()
	jobConfig := f.runtime.Config().Copy()
	jobConfig.MaxConns = 4
	jobConfig.MinConns = 1
	jobPool, err := pgxpool.NewWithConfig(t.Context(), jobConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer jobPool.Close()
	jobStore, err := postgres.New(t.Context(), jobPool)
	if err != nil {
		t.Fatal(err)
	}
	worker := jobs.Worker{Store: jobStore, Sender: brokerSender{client: client, url: server.URL}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	defer func() { cancel(); <-done }()
	start := time.Now()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatalf("burst did not drain: %+v", worker.Snapshot())
		case <-ticker.C:
			if f.scalar(`SELECT count(*) FROM platform.outbox WHERE event_type LIKE 'test.background.%' AND delivered_at IS NULL`) == "0" {
				elapsed := time.Since(start)
				t.Logf("1000 database events over TLS HTTP in %s (%.0f events/s)", elapsed, 1000/elapsed.Seconds())
				return
			}
		}
	}
}

func TestBackgroundQueueHealthReportsDeadAndExpired(t *testing.T) {
	f := newFixture(t)
	baseline, err := f.store.QueueState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE platform.outbox SET dead_at=clock_timestamp(),created_at=clock_timestamp()-interval '90 seconds' WHERE operation_id IN (SELECT id FROM platform.operations WHERE organization_id=$1)`, f.org)
	f.exec(`SELECT inventory.reserve_stock($1,$2,$3,1,$4,1,clock_timestamp()+interval '50 milliseconds')`, id(), f.actor, f.line, f.source)
	time.Sleep(60 * time.Millisecond)
	state, err := f.store.QueueState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Dead <= baseline.Dead || state.ExpiredReservations < 1 || state.OldestExpiredSeconds <= 0 {
		t.Fatalf("missing backlog: %+v", state)
	}
}
