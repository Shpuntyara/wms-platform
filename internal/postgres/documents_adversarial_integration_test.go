//go:build integration

package postgres_test

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func documentSQL(t *testing.T, f *fixture, actor, command, document string, body map[string]any) (json.RawMessage, error) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if document != "" {
		doc = document
	}
	var result []byte
	err = f.runtime.QueryRow(t.Context(), `SELECT warehouse.command_document($1,$2,$3,$4::jsonb)`, command, actor, doc, raw).Scan(&result)
	return result, err
}

func requireDocumentSQLProblem(t *testing.T, err error, code, message string) {
	t.Helper()
	var problem *pgconn.PgError
	if !errors.As(err, &problem) || problem.Code != code || problem.Message != message {
		t.Fatalf("SQL error=%v, want %s/%s", err, code, message)
	}
}

func documentCreateBody(f *fixture, number string) map[string]any {
	return map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": "receipt", "number": number, "lines": []any{map[string]any{"item_id": f.item, "quantity": "1.125"}}}
}

func assertSingleDocumentEffect(t *testing.T, f *fixture, key string) {
	t.Helper()
	for _, table := range []string{"operations", "audit_entries", "outbox"} {
		column := "operation_id"
		if table == "operations" {
			column = "id"
		}
		if got := f.scalar(`SELECT count(*) FROM platform.`+table+` WHERE `+column+`=$1`, key); got != "1" {
			t.Fatalf("%s for command=%s, want exactly one", table, got)
		}
	}
}

func TestDocumentAdversarialCommandIdentityAndAuthorization(t *testing.T) {
	t.Run("another authorized actor cannot reuse the same input and ID", func(t *testing.T) {
		f := newFixture(t)
		grantDocumentPermissions(f)
		secondActor := id()
		f.exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'https://tests.invalid',$1::uuid::text)`, secondActor)
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.create')`, secondActor, f.org, f.wh, f.owner)
		key := id()
		body := documentCreateBody(f, id())
		original, err := documentSQL(t, f, f.actor, key, "", body)
		if err != nil {
			t.Fatal(err)
		}
		leaked, err := documentSQL(t, f, secondActor, key, "", body)
		requireDocumentSQLProblem(t, err, "22023", "idempotency_key_reused")
		if len(leaked) != 0 {
			t.Fatal("cross-actor replay leaked the original result")
		}
		replayed, err := documentSQL(t, f, f.actor, key, "", body)
		if err != nil || string(replayed) != string(original) {
			t.Fatal("rejected attack changed the legitimate actor's durable result")
		}
		assertSingleDocumentEffect(t, f, key)
	})

	t.Run("runtime SQL enforces owner on create and transition", func(t *testing.T) {
		f := newFixture(t)
		grantDocumentPermissions(f)
		body := documentCreateBody(f, id())
		body["owner_id"] = f.otherOwner
		key := id()
		_, err := documentSQL(t, f, f.actor, key, "", body)
		requireDocumentSQLProblem(t, err, "42501", "forbidden")
		if f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, key) != "0" || f.scalar(`SELECT count(*) FROM warehouse.documents WHERE organization_id=$1 AND number=$2`, f.org, body["number"]) != "0" {
			t.Fatal("unauthorized create left durable state")
		}
		foreign, _ := f.doc("receipt", "1", f.otherOwner)
		transition := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 1, "state": "cancelled"}
		_, err = documentSQL(t, f, f.actor, key, foreign, transition)
		requireDocumentSQLProblem(t, err, "42501", "forbidden")
		if f.scalar(`SELECT state||':'||version FROM warehouse.documents WHERE id=$1`, foreign) != "active:1" || f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, key) != "0" {
			t.Fatal("unauthorized transition changed the foreign owner document")
		}
	})

	t.Run("revoked permission blocks replay before exposing a receipt", func(t *testing.T) {
		f := newFixture(t)
		grantDocumentPermissions(f)
		key := id()
		body := documentCreateBody(f, id())
		if _, err := documentSQL(t, f, f.actor, key, "", body); err != nil {
			t.Fatal(err)
		}
		f.exec(`DELETE FROM platform.actor_grants WHERE actor_id=$1 AND permission='document.create'`, f.actor)
		_, err := documentSQL(t, f, f.actor, key, "", body)
		requireDocumentSQLProblem(t, err, "42501", "forbidden")
		requireStatus(t, f.request(f.handler(nil, nil), "POST", "/api/v1/documents", body, key), 403)
		assertSingleDocumentEffect(t, f, key)
	})

	t.Run("ID binds warehouse owner and document as well as input", func(t *testing.T) {
		f := newFixture(t)
		grantDocumentPermissions(f)
		key := id()
		body := documentCreateBody(f, id())
		if _, err := documentSQL(t, f, f.actor, key, "", body); err != nil {
			t.Fatal(err)
		}
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.create')`, f.actor, f.org, f.wh, f.otherOwner)
		body["owner_id"] = f.otherOwner
		_, err := documentSQL(t, f, f.actor, key, "", body)
		requireDocumentSQLProblem(t, err, "22023", "idempotency_key_reused")
		wh := id()
		f.exec(`INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES($1,$2,$1::uuid::text)`, wh, f.org)
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'document.create')`, f.actor, f.org, wh, f.owner)
		body["owner_id"], body["warehouse_id"] = f.owner, wh
		_, err = documentSQL(t, f, f.actor, key, "", body)
		requireDocumentSQLProblem(t, err, "22023", "idempotency_key_reused")
		first, _ := f.doc("receipt", "1", f.owner)
		second, _ := f.doc("receipt", "1", f.owner)
		transitionKey := id()
		transition := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 1, "state": "cancelled"}
		if _, err := documentSQL(t, f, f.actor, transitionKey, first, transition); err != nil {
			t.Fatal(err)
		}
		_, err = documentSQL(t, f, f.actor, transitionKey, second, transition)
		requireDocumentSQLProblem(t, err, "22023", "idempotency_key_reused")
		if f.scalar(`SELECT state||':'||version FROM warehouse.documents WHERE id=$1`, second) != "active:1" {
			t.Fatal("reusing transition ID changed a different document")
		}
		assertSingleDocumentEffect(t, f, key)
		assertSingleDocumentEffect(t, f, transitionKey)
	})
}

func TestDocumentAdversarialConcurrentCommands(t *testing.T) {
	t.Run("eight identical retries and competing transitions", func(t *testing.T) {
		f := newFixture(t)
		grantDocumentPermissions(f)
		h := f.handler(nil, nil)
		key, number := id(), id()
		body := documentCreateBody(f, number)
		start := make(chan struct{})
		responses := make(chan *httptest.ResponseRecorder, 8)
		for range 8 {
			go func() {
				<-start
				responses <- f.request(h, "POST", "/api/v1/documents", body, key)
			}()
		}
		close(start)
		first := <-responses
		requireStatus(t, first, 201)
		for range 7 {
			got := <-responses
			requireStatus(t, got, 201)
			if got.Body.String() != first.Body.String() {
				t.Fatal("same command produced a different receipt under contention")
			}
		}
		doc := data(t, first)["document_id"].(string)
		if f.scalar(`SELECT count(*) FROM warehouse.documents WHERE organization_id=$1 AND number=$2`, f.org, number) != "1" || f.scalar(`SELECT count(*) FROM warehouse.document_lines WHERE document_id=$1`, doc) != "1" {
			t.Fatal("concurrent retries duplicated document or lines")
		}
		assertSingleDocumentEffect(t, f, key)
		start = make(chan struct{})
		keys := []string{id(), id()}
		for i, state := range []string{"active", "cancelled"} {
			go func() {
				<-start
				transition := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 1, "state": state}
				responses <- f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, keys[i])
			}()
		}
		close(start)
		a, b := <-responses, <-responses
		if !((a.Code == 201 && b.Code == 409) || (a.Code == 409 && b.Code == 201)) {
			t.Fatalf("competing transitions=%d/%d: %s %s", a.Code, b.Code, a.Body, b.Body)
		}
		if f.scalar(`SELECT version::text||':'||content_revision::text FROM warehouse.documents WHERE id=$1`, doc) != "2:2" {
			t.Fatal("both competing transitions took effect")
		}
		for _, table := range []string{"operations", "audit_entries", "outbox"} {
			column := "operation_id"
			if table == "operations" {
				column = "id"
			}
			if f.scalar(`SELECT count(*) FROM platform.`+table+` WHERE `+column+`=ANY($1::uuid[])`, keys) != "1" {
				t.Fatalf("competing transitions left duplicate or missing %s", table)
			}
		}
	})

	t.Run("conflicting requests with same ID cannot both succeed", func(t *testing.T) {
		f := newFixture(t)
		grantDocumentPermissions(f)
		h := f.handler(nil, nil)
		key := id()
		bodies := []map[string]any{documentCreateBody(f, id()), documentCreateBody(f, id())}
		responses := make(chan *httptest.ResponseRecorder, 2)
		start := make(chan struct{})
		for _, body := range bodies {
			go func() { <-start; responses <- f.request(h, "POST", "/api/v1/documents", body, key) }()
		}
		close(start)
		a, b := <-responses, <-responses
		if !((a.Code == 201 && b.Code == 409) || (a.Code == 409 && b.Code == 201)) {
			t.Fatalf("conflicting creates=%d/%d: %s %s", a.Code, b.Code, a.Body, b.Body)
		}
		denied := a
		if denied.Code == 201 {
			denied = b
		}
		if !strings.Contains(denied.Body.String(), "idempotency_key_reused") {
			t.Fatalf("losing request did not report ID conflict: %s", denied.Body)
		}
		if f.scalar(`SELECT count(*) FROM warehouse.documents WHERE organization_id=$1 AND number=ANY($2::text[])`, f.org, []string{bodies[0]["number"].(string), bodies[1]["number"].(string)}) != "1" {
			t.Fatal("conflicting request created a second document")
		}
		assertSingleDocumentEffect(t, f, key)
	})
}
