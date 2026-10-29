//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"

	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These failures occur at the socket boundary of a real PostgreSQL transaction.
// A generic error models a transport wrapper which does not implement net.Error.
type documentCommitFault struct {
	net.Conn
	mode    string
	dropped *atomic.Bool
	tail    []byte
}

func (c *documentCommitFault) Write(b []byte) (int, error) {
	if c.mode == "before_send" && bytes.Contains(bytes.ToUpper(b), []byte("COMMIT\x00")) && c.dropped.CompareAndSwap(false, true) {
		_ = c.Conn.Close()
		return 0, errors.New("simulated transport write failure")
	}
	return c.Conn.Write(b)
}

func (c *documentCommitFault) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	combined := append(c.tail, b[:n]...)
	if c.mode != "before_send" && bytes.Contains(combined, []byte{'C', 0, 0, 0, 11, 'C', 'O', 'M', 'M', 'I', 'T', 0}) && c.dropped.CompareAndSwap(false, true) {
		_ = c.Conn.Close()
		if c.mode == "after_ack_eof" {
			return 0, io.EOF
		}
		return 0, errors.New("simulated transport read failure")
	}
	if len(combined) > 11 {
		combined = combined[len(combined)-11:]
	}
	c.tail = bytes.Clone(combined)
	return n, err
}

func grantDocumentPermissions(f *fixture) {
	f.t.Helper()
	for _, permission := range []string{"document.read", "document.create", "document.transition"} {
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, f.actor, f.org, f.wh, f.owner, permission)
	}
}

func TestDocumentCommandsRecoverFromCommitTransportFailure(t *testing.T) {
	for _, action := range []string{"create", "transition"} {
		for _, mode := range []string{"before_send", "after_ack_eof", "after_ack_generic"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				f := newFixture(t)
				grantDocumentPermissions(f)
				cfg, err := pgxpool.ParseConfig(os.Getenv("WMS_TEST_RUNTIME_DSN"))
				if err != nil {
					t.Fatal(err)
				}
				var dropped atomic.Bool
				cfg.MaxConns = 1
				cfg.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
					conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
					if err != nil {
						return nil, err
					}
					return &documentCommitFault{Conn: conn, mode: mode, dropped: &dropped}, nil
				}
				pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				f.store, err = postgres.New(t.Context(), pool)
				if err != nil {
					t.Fatal(err)
				}
				h := f.handler(nil, nil)
				key, number := id(), id()
				path := "/api/v1/documents"
				body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": "receipt", "number": number, "lines": []any{map[string]any{"item_id": f.item, "quantity": "2.125"}}}
				if action == "transition" {
					path += "/" + f.document + "/transitions"
					body = map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 1, "state": "cancelled"}
				}
				w := f.request(h, "POST", path, body, key)
				if !dropped.Load() {
					t.Fatal("COMMIT fault was not injected")
				}
				committed := "1"
				if mode == "before_send" {
					committed = "0"
				}
				if got := f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1 AND completed_at IS NOT NULL`, key); got != committed {
					t.Fatalf("committed operations before retry=%s want=%s", got, committed)
				}
				if action == "create" {
					if got := f.scalar(`SELECT count(*) FROM warehouse.documents WHERE organization_id=$1 AND number=$2`, f.org, number); got != committed {
						t.Fatalf("documents before retry=%s want=%s", got, committed)
					}
				} else {
					want := "cancelled:2:2"
					if mode == "before_send" {
						want = "active:1:1"
					}
					if got := f.scalar(`SELECT state||':'||version||':'||content_revision FROM warehouse.documents WHERE id=$1`, f.document); got != want {
						t.Fatalf("document before retry=%s want=%s", got, want)
					}
				}
				requireStatus(t, w, 503)
				if !strings.Contains(w.Body.String(), "same command identifier") || w.Header().Get("Retry-After") == "" {
					t.Fatalf("missing same-ID recovery instruction: %s", w.Body)
				}
				recovered := f.request(h, "POST", path, body, key)
				requireStatus(t, recovered, 201)
				replayed := f.request(h, "POST", path, body, key)
				requireStatus(t, replayed, 201)
				if recovered.Body.String() != replayed.Body.String() {
					t.Fatal("recovery did not return the durable original result")
				}
				result := data(t, recovered)
				doc := result["document_id"].(string)
				wantState, wantVersion, wantLines := "draft", "1", "1"
				if action == "transition" {
					wantState, wantVersion = "cancelled", "2"
				}
				if got := f.scalar(`SELECT state||':'||version||':'||content_revision FROM warehouse.documents WHERE id=$1`, doc); got != wantState+":"+wantVersion+":"+wantVersion {
					t.Fatalf("document changed twice after recovery: %s", got)
				}
				if got := f.scalar(`SELECT count(*) FROM warehouse.document_lines WHERE document_id=$1`, doc); got != wantLines {
					t.Fatalf("lines after recovery=%s", got)
				}
				for _, table := range []string{"operations", "audit_entries", "outbox"} {
					column := "operation_id"
					if table == "operations" {
						column = "id"
					}
					if got := f.scalar(`SELECT count(*) FROM platform.`+table+` WHERE `+column+`=$1`, key); got != "1" {
						t.Fatalf("%s after recovery=%s want=1", table, got)
					}
				}
				if got := f.scalar(`SELECT on_hand::text||':'||reserved_qty::text FROM inventory.stock_balances WHERE id=$1`, f.source); got != "10.000000:0.000000" {
					t.Fatalf("document recovery changed stock: %s", got)
				}
			})
		}
	}
}

func TestDocumentCommandsPreserveDefinitiveCommitRejection(t *testing.T) {
	for _, action := range []string{"create", "transition"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			grantDocumentPermissions(f)
			h := f.handler(nil, nil)
			// A deferred constraint fires only at COMMIT, after command_document
			// has returned its result. This is a confirmed rollback, not an
			// ambiguous transport failure.
			name := "test_document_commit_" + strings.ReplaceAll(id(), "-", "")
			f.exec(`CREATE FUNCTION warehouse.` + name + `() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$ BEGIN
			 IF NEW.organization_id='` + f.org + `'::uuid THEN RAISE EXCEPTION 'test deferred constraint' USING ERRCODE='23514'; END IF;
			 RETURN NULL; END $$`)
			f.exec(`CREATE CONSTRAINT TRIGGER ` + name + ` AFTER INSERT OR UPDATE ON warehouse.documents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION warehouse.` + name + `()`)
			t.Cleanup(func() {
				f.cleanupExec(`DROP TRIGGER IF EXISTS ` + name + ` ON warehouse.documents`)
				f.cleanupExec(`DROP FUNCTION IF EXISTS warehouse.` + name + `()`)
			})
			key, number := id(), id()
			path := "/api/v1/documents"
			body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": "receipt", "number": number, "lines": []any{map[string]any{"item_id": f.item, "quantity": "1"}}}
			if action == "transition" {
				path += "/" + f.document + "/transitions"
				body = map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": 1, "state": "cancelled"}
			}
			w := f.request(h, "POST", path, body, key)
			requireStatus(t, w, 409)
			if !strings.Contains(w.Body.String(), "inventory_constraint_conflict") || w.Header().Get("Retry-After") != "" {
				t.Fatalf("confirmed rollback became ambiguous: %s", w.Body)
			}
			if f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, key) != "0" {
				t.Fatal("failed COMMIT kept the operation receipt")
			}
			if action == "create" && f.scalar(`SELECT count(*) FROM warehouse.documents WHERE organization_id=$1 AND number=$2`, f.org, number) != "0" {
				t.Fatal("failed COMMIT kept the created document")
			}
			if action == "transition" && f.scalar(`SELECT state||':'||version FROM warehouse.documents WHERE id=$1`, f.document) != "active:1" {
				t.Fatal("failed COMMIT kept the transition")
			}
		})
	}
}

func TestDocumentCommandProperties(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	h := f.handler(nil, nil)
	// Fixed seed makes CI failures reproducible; quick reports the failing input.
	config := &quick.Config{MaxCount: 64, Rand: rand.New(rand.NewSource(20261028))}
	property := func(seed uint64) bool {
		kind := []string{"receipt", "shipment", "transfer", "adjustment"}[seed%4]
		lineCount := 1 + int((seed>>2)%5)
		milli := 1 + (seed>>5)%999999
		quantity := fmt.Sprintf("%d.%03d", milli/1000, milli%1000)
		lines := make([]any, lineCount)
		for i := range lines {
			lines[i] = map[string]any{"item_id": f.item, "quantity": quantity}
		}
		createKey := id()
		body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": kind, "number": id(), "lines": lines}
		if kind == "shipment" {
			body["recipient"] = "Odbiorca dokumentu z historii generowanej"
		}
		created := f.request(h, "POST", "/api/v1/documents", body, createKey)
		if created.Code != 201 {
			t.Logf("seed=%d create=%d: %s", seed, created.Code, created.Body)
			return false
		}
		doc := data(t, created)["document_id"].(string)
		wantQuantity := fmt.Sprintf("%d.%06d", milli/1000, milli%1000*1000)
		if got := f.scalar(`SELECT count(*) FROM warehouse.document_lines WHERE document_id=$1 AND requested_qty=$2::numeric AND reserved_qty=0 AND fulfilled_qty=0`, doc, wantQuantity); got != fmt.Sprint(lineCount) {
			t.Logf("seed=%d lines=%s want=%d", seed, got, lineCount)
			return false
		}
		version, state := 1, "draft"
		// Exercise both allowed paths to cancellation and reject incomplete close.
		if (seed>>8)%2 == 0 {
			key := id()
			transition := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": version, "state": "active"}
			activated := f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, key)
			if activated.Code != 201 {
				t.Logf("seed=%d activation=%d: %s", seed, activated.Code, activated.Body)
				return false
			}
			replay := f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, key)
			if replay.Code != 201 || replay.Body.String() != activated.Body.String() {
				return false
			}
			version, state = 2, "active"
			closeKey := id()
			transition["state"], transition["expected_document_version"] = "closed", version
			closed := f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, closeKey)
			if closed.Code != 409 || f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, closeKey) != "0" {
				t.Logf("seed=%d incomplete close=%d", seed, closed.Code)
				return false
			}
		}
		// Stale version must leave no receipt, audit or outbox, so the ID can be retried.
		transitionKey := id()
		transition := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": version + 1, "state": "cancelled"}
		stale := f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, transitionKey)
		if stale.Code != 409 || f.scalar(`SELECT state||':'||version FROM warehouse.documents WHERE id=$1`, doc) != fmt.Sprintf("%s:%d", state, version) || f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, transitionKey) != "0" {
			t.Logf("seed=%d stale version changed state or kept operation", seed)
			return false
		}
		transition["expected_document_version"] = version
		cancelled := f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, transitionKey)
		if cancelled.Code != 201 {
			t.Logf("seed=%d cancel=%d: %s", seed, cancelled.Code, cancelled.Body)
			return false
		}
		for range 1 + int((seed>>9)%4) {
			replay := f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", transition, transitionKey)
			if replay.Code != 201 || replay.Body.String() != cancelled.Body.String() {
				return false
			}
		}
		// Replaying creation after later transitions must return its original snapshot.
		replayed := f.request(h, "POST", "/api/v1/documents", body, createKey)
		if replayed.Code != 201 || replayed.Body.String() != created.Body.String() {
			t.Logf("seed=%d create replay changed after transition", seed)
			return false
		}
		body["number"] = id()
		reused := f.request(h, "POST", "/api/v1/documents", body, createKey)
		if reused.Code != 409 || !strings.Contains(reused.Body.String(), "idempotency_key_reused") {
			return false
		}
		if f.scalar(`SELECT state||':'||version||':'||content_revision FROM warehouse.documents WHERE id=$1`, doc) != fmt.Sprintf("cancelled:%d:%d", version+1, version+1) {
			return false
		}
		terminalKey := id()
		terminal := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "expected_document_version": version + 1, "state": "active"}
		invalid := f.request(h, "POST", "/api/v1/documents/"+doc+"/transitions", terminal, terminalKey)
		if invalid.Code != 409 || f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, terminalKey) != "0" || f.scalar(`SELECT state FROM warehouse.documents WHERE id=$1`, doc) != "cancelled" {
			t.Logf("seed=%d terminal document was reactivated: %d", seed, invalid.Code)
			return false
		}
		for _, key := range []string{createKey, transitionKey} {
			if f.scalar(`SELECT count(*) FROM platform.audit_entries WHERE operation_id=$1`, key) != "1" || f.scalar(`SELECT count(*) FROM platform.outbox WHERE operation_id=$1`, key) != "1" {
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentInvalidGeneratedLineRollsBackWholeCreate(t *testing.T) {
	f := newFixture(t)
	grantDocumentPermissions(f)
	h := f.handler(nil, nil)
	property := func(prefix uint8, suffix uint8) bool {
		lines := make([]any, 0, int(prefix%8+suffix%8)+1)
		for range int(prefix % 8) {
			lines = append(lines, map[string]any{"item_id": f.item, "quantity": "1.125"})
		}
		lines = append(lines, map[string]any{"item_id": f.item, "quantity": "0.0001"})
		for range int(suffix % 8) {
			lines = append(lines, map[string]any{"item_id": f.item, "quantity": "2.500"})
		}
		key, number := id(), id()
		body := map[string]any{"organization_id": f.org, "warehouse_id": f.wh, "owner_id": f.owner, "kind": "receipt", "number": number, "lines": lines}
		w := f.request(h, "POST", "/api/v1/documents", body, key)
		if w.Code != 400 {
			t.Logf("invalid line at %d with %d followers: %d %s", prefix%8, suffix%8, w.Code, w.Body)
			return false
		}
		if f.scalar(`SELECT count(*) FROM warehouse.documents WHERE organization_id=$1 AND number=$2`, f.org, number) != "0" || f.scalar(`SELECT count(*) FROM platform.operations WHERE id=$1`, key) != "0" {
			return false
		}
		// The failed command ID and document number must remain usable.
		body["lines"] = []any{map[string]any{"item_id": f.item, "quantity": "1"}}
		retried := f.request(h, "POST", "/api/v1/documents", body, key)
		if retried.Code != 201 {
			return false
		}
		var durable json.RawMessage
		if err := f.admin.QueryRow(t.Context(), `SELECT result FROM platform.operations WHERE id=$1`, key).Scan(&durable); err != nil {
			t.Fatal(err)
		}
		return len(durable) > 0
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 40, Rand: rand.New(rand.NewSource(7282026))}); err != nil {
		t.Fatal(err)
	}
}
