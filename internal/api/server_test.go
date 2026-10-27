package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/Shpuntyara/wms-platform/internal/modules"
)

const (
	testOrg       = "00000000-0000-4000-8000-000000000001"
	testWarehouse = "00000000-0000-4000-8000-000000000002"
	testActor     = "00000000-0000-4000-8000-000000000003"
	testLine      = "00000000-0000-4000-8000-000000000004"
	testBalance   = "00000000-0000-4000-8000-000000000005"
	testCommand   = "00000000-0000-4000-8000-000000000006"
)

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, token string) (auth.Identity, error) {
	if token != "valid" {
		return auth.Identity{}, errors.New("invalid token")
	}
	return auth.Identity{Issuer: "issuer", Subject: "worker"}, nil
}

type fakeBackend struct {
	err       error
	principal inventory.Principal
	calls     int
	actor     string
}

func (f *fakeBackend) ResolvePrincipal(context.Context, auth.Identity) (inventory.Principal, error) {
	return f.principal, nil
}
func (f *fakeBackend) List(context.Context, inventory.Resource, inventory.Scope, inventory.Filters) (inventory.Page, error) {
	f.calls++
	return inventory.Page{Data: []json.RawMessage{json.RawMessage(`{"on_hand":"12.125000"}`)}}, f.err
}
func (f *fakeBackend) Get(context.Context, inventory.Resource, inventory.Scope, string) (json.RawMessage, error) {
	f.calls++
	return json.RawMessage(`{"id":"resource"}`), f.err
}
func (f *fakeBackend) Execute(_ context.Context, p inventory.Principal, c inventory.Command, _ *modules.Registry) (json.RawMessage, error) {
	f.calls++
	f.actor = p.ActorID
	b, _ := json.Marshal(map[string]string{"operation_id": c.ID})
	return b, f.err
}
func (f *fakeBackend) Ping(context.Context) error { return f.err }

func testHandler(t *testing.T) (http.Handler, *fakeBackend) {
	t.Helper()
	backend := &fakeBackend{principal: inventory.Principal{ActorID: testActor}}
	for _, permission := range []string{"stock.read", "reservation.read", "reservation.create", "reservation.release", "movement.read", "movement.create", "operation.read"} {
		backend.principal.Grants = append(backend.principal.Grants, inventory.Grant{OrganizationID: testOrg, WarehouseID: testWarehouse, Permission: permission})
	}
	registry, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{Backend: backend, Auth: fakeAuth{}, Modules: registry})
	if err != nil {
		t.Fatal(err)
	}
	return handler, backend
}

func request(handler http.Handler, method, path, body, token, key, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestServerRejectsInvalidRequestsBeforeBackend(t *testing.T) {
	valid := `{"organization_id":"` + testOrg + `","warehouse_id":"` + testWarehouse + `","document_line_id":"` + testLine + `","expected_document_version":1,"balance_id":"` + testBalance + `","quantity":"1"}`
	path := "/api/v1/stock-balances?organization_id=" + testOrg + "&warehouse_id=" + testWarehouse
	for _, tc := range []struct {
		name, method, path, body, token, key, contentType string
		status                                            int
	}{
		{"unauthenticated", "GET", path, "", "", "", "", 401},
		{"invalid token", "GET", path, "", "invalid", "", "", 401},
		{"missing scope", "GET", "/api/v1/stock-balances", "", "valid", "", "", 400},
		{"invalid limit", "GET", path + "&limit=201", "", "valid", "", "", 400},
		{"negative limit", "GET", path + "&limit=-1", "", "valid", "", "", 400},
		{"foreign warehouse", "GET", strings.ReplaceAll(path, testWarehouse, testLine), "", "valid", "", "", 403},
		{"missing idempotency", "POST", "/api/v1/reservations", valid, "valid", "", "application/json", 400},
		{"bad idempotency", "POST", "/api/v1/reservations", valid, "valid", "bad", "application/json", 400},
		{"numeric quantity", "POST", "/api/v1/reservations", strings.ReplaceAll(valid, `"quantity":"1"`, `"quantity":1`), "valid", testCommand, "application/json", 400},
		{"zero quantity", "POST", "/api/v1/reservations", strings.ReplaceAll(valid, `"quantity":"1"`, `"quantity":"0"`), "valid", testCommand, "application/json", 400},
		{"unknown fields", "POST", "/api/v1/reservations", strings.TrimSuffix(valid, "}") + `,"actor_id":"forged"}`, "valid", testCommand, "application/json", 400},
		{"trailing json", "POST", "/api/v1/reservations", valid + ` {}`, "valid", testCommand, "application/json", 400},
		{"wrong media type", "POST", "/api/v1/reservations", valid, "valid", testCommand, "text/plain", 415},
		{"large body", "POST", "/api/v1/reservations", strings.Repeat(" ", 1024*1024+1), "valid", testCommand, "application/json", 413},
		{"method not allowed", "PUT", path, "", "valid", "", "", 405},
		{"unknown route", "GET", "/api/v1/not-a-resource", "", "valid", "", "", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, backend := testHandler(t)
			w := request(h, tc.method, tc.path, tc.body, tc.token, tc.key, tc.contentType)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatalf("problem content type=%q", w.Header().Get("Content-Type"))
			}
			var problem map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
				t.Fatal(err)
			}
			if problem["code"] == nil || problem["trace_id"] == nil || problem["type"] == nil {
				t.Fatalf("incomplete problem: %v", problem)
			}
			if backend.calls != 0 {
				t.Fatalf("backend called %d times", backend.calls)
			}
		})
	}
}

func TestServerReturnsDecimalStringsAndAuthenticatedActor(t *testing.T) {
	h, b := testHandler(t)
	w := request(h, "GET", "/api/v1/stock-balances?organization_id="+testOrg+"&warehouse_id="+testWarehouse, "", "valid", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"on_hand":"12.125000"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	body := `{"organization_id":"` + testOrg + `","warehouse_id":"` + testWarehouse + `","document_line_id":"` + testLine + `","expected_document_version":1,"balance_id":"` + testBalance + `","quantity":"1"}`
	w = request(h, "POST", "/api/v1/reservations", body, "valid", testCommand, "application/json")
	if w.Code != 201 || b.actor != testActor || !strings.Contains(w.Body.String(), testCommand) {
		t.Fatalf("actor=%q status=%d body=%s", b.actor, w.Code, w.Body)
	}
}

func TestServerRedactsUnexpectedErrors(t *testing.T) {
	h, b := testHandler(t)
	b.err = errors.New("postgres://user:secret-password@private-db")
	w := request(h, "GET", "/api/v1/stock-balances?organization_id="+testOrg+"&warehouse_id="+testWarehouse, "", "valid", "", "")
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret-password") || strings.Contains(w.Body.String(), "private-db") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}

func TestServerHealth(t *testing.T) {
	h, b := testHandler(t)
	b.err = errors.New("database unavailable")
	if w := request(h, "GET", "/livez", "", "", "", ""); w.Code != 200 {
		t.Fatalf("liveness=%d", w.Code)
	}
	if w := request(h, "GET", "/readyz", "", "", "", ""); w.Code != 503 {
		t.Fatalf("readiness=%d", w.Code)
	}
}

func TestAdjustmentsRequireSeparatePermission(t *testing.T) {
	h, b := testHandler(t)
	body := `{"organization_id":"` + testOrg + `","warehouse_id":"` + testWarehouse + `","document_line_id":"` + testLine + `","expected_document_version":1,"kind":"adjustment_out","source_balance_id":"` + testBalance + `","quantity":"1","reason":"stock count"}`
	w := request(h, "POST", "/api/v1/movements", body, "valid", testCommand, "application/json")
	if w.Code != 403 || b.calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, b.calls, w.Body)
	}
}

func TestReleaseReasonUnicodeLength(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, character string
	}{{name: "ASCII", character: "a"}, {name: "Cyrillic", character: "я"}, {name: "emoji", character: "📦"}} {
		for _, length := range []int{2000, 2001} {
			name := tc.name + "/within limit"
			status, calls := 201, 1
			if length > 2000 {
				name = tc.name + "/over limit"
				status, calls = 400, 0
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				h, backend := testHandler(t)
				body, err := json.Marshal(map[string]any{"organization_id": testOrg, "warehouse_id": testWarehouse, "expected_document_version": 1, "quantity": "1", "reason": strings.Repeat(tc.character, length)})
				if err != nil {
					t.Fatal(err)
				}
				w := request(h, "POST", "/api/v1/reservations/"+testBalance+"/releases", string(body), "valid", testCommand, "application/json")
				if w.Code != status || backend.calls != calls {
					t.Fatalf("status=%d calls=%d want status=%d calls=%d body=%s", w.Code, backend.calls, status, calls, w.Body)
				}
			})
		}
	}
}
