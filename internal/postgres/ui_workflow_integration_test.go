//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/api"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/ui"
	"github.com/Shpuntyara/wms-platform/web"
)

// TestBrowserReceiptAndPutawayAgainstPostgres drives the shipped browser assets,
// the real HTTP handlers and the real SQL kernel. Only the external identity
// provider adapter is replaced with the fixture's existing test identity.
func TestBrowserReceiptAndPutawayAgainstPostgres(t *testing.T) {
	f := newFixture(t)
	for _, permission := range []string{"document.read", "document.create", "document.transition"} {
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, f.actor, f.org, f.wh, f.owner, permission)
	}
	f.exec(`UPDATE catalog.items SET sku='BOLT-M8' WHERE id=$1`, f.item)
	f.exec(`UPDATE warehouse.documents SET number='PZ-BAZA-0001',state='closed' WHERE organization_id=$1 AND kind='receipt'`, f.org)
	for _, address := range []struct{ balance, code, kind string }{
		{f.source, "PRZYJECIE-01", "receiving"},
		{f.target, "A-01-02", "storage"},
	} {
		f.exec(`UPDATE warehouse.locations l SET code=$2,kind=$3 FROM warehouse.positions p JOIN inventory.stock_balances b ON b.position_id=p.id WHERE l.id=p.location_id AND b.id=$1`, address.balance, address.code, address.kind)
	}
	position := func(balance string) string {
		t.Helper()
		return f.scalar(`SELECT position_id::text FROM inventory.stock_balances WHERE id=$1`, balance)
	}
	workspace := api.WorkspaceConfig{
		OIDC:          api.BrowserOIDC{URL: "https://identity.tests.invalid", Realm: "browser-test", ClientID: "wms-browser"},
		Organizations: []api.NamedOrganization{{ID: f.org, Name: "Przedsiębiorstwo magazynowe"}},
		Warehouses:    []api.NamedWarehouse{{ID: f.wh, OrganizationID: f.org, Name: "Magazyn Północny"}},
		Owners:        []api.NamedOwner{{ID: f.owner, OrganizationID: f.org, Name: "Właściciel główny"}},
		Items:         []api.NamedItem{{ID: f.item, OrganizationID: f.org, SKU: "BOLT-M8", Name: "Śruba M8", Unit: "szt.", QuantityScale: 3}},
		Positions: []api.NamedPosition{
			{ID: position(f.source), OrganizationID: f.org, WarehouseID: f.wh, Code: "PRZYJECIE-01", Kind: "receiving"},
			{ID: position(f.target), OrganizationID: f.org, WarehouseID: f.wh, Code: "A-01-02", Kind: "storage"},
		},
	}
	registry, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := api.New(api.Config{Backend: f.store, Auth: tokenAuth{subject: f.actor}, Modules: registry, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := ui.New(backend, web.Assets, workspace.OIDC)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("browser integration requires Node.js: ", err)
	}
	cli := filepath.Join(repo, "web", "node_modules", "@playwright", "test", "cli.js")
	if _, err := os.Stat(cli); err != nil {
		t.Fatal("browser integration requires npm ci and Playwright Chromium installation in web/: ", err)
	}
	evidence := os.Getenv("WMS_UI_LIVE_EVIDENCE_DIR")
	if evidence == "" {
		evidence = filepath.Join(repo, "work", "ui-live-browser")
	}
	evidence = filepath.Join(evidence, f.org)
	if err := os.MkdirAll(evidence, 0o755); err != nil {
		t.Fatal(err)
	}
	number := "PZ-TEST-" + f.org[:8]
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, node, cli, "test", "--config", "playwright.live.config.mjs")
	command.Dir = filepath.Join(repo, "web")
	command.Env = append(os.Environ(),
		"WMS_UI_LIVE_URL="+server.URL,
		"WMS_UI_LIVE_ACTOR="+f.actor,
		"WMS_UI_LIVE_OWNER="+f.owner,
		"WMS_UI_LIVE_ITEM="+f.item,
		"WMS_UI_LIVE_NUMBER="+number,
		"WMS_UI_LIVE_ARTIFACTS="+filepath.Join(evidence, "browser"),
	)
	output, runErr := command.CombinedOutput()
	if err := os.WriteFile(filepath.Join(evidence, "browser.txt"), output, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("Browser evidence: %s\n%s", evidence, output)
	if runErr != nil {
		t.Fatalf("real PostgreSQL browser workflow failed: %v", runErr)
	}

	// These assertions inspect committed kernel state independently of the UI.
	checks := []struct{ name, got, want string }{
		{"source returns to initial stock after receiving and putaway", f.scalar(`SELECT on_hand::text FROM inventory.stock_balances WHERE id=$1`, f.source), "10.000000"},
		{"target contains exactly the received amount", f.scalar(`SELECT on_hand::text FROM inventory.stock_balances WHERE id=$1`, f.target), "4.000000"},
		{"stock total increases once despite the lost committed response", f.scalar(`SELECT sum(on_hand)::text FROM inventory.stock_balances WHERE organization_id=$1 AND owner_id=$2`, f.org, f.owner), "14.000000"},
		{"one closed receipt was created", f.scalar(`SELECT count(*) FROM warehouse.documents WHERE organization_id=$1 AND number=$2 AND kind='receipt' AND state='closed'`, f.org, number), "1"},
		{"one receipt movement was committed", f.scalar(`SELECT count(*) FROM inventory.movements m JOIN warehouse.document_lines l ON l.id=m.document_line_id JOIN warehouse.documents d ON d.id=l.document_id WHERE d.organization_id=$1 AND d.number=$2 AND m.kind='receipt'`, f.org, number), "1"},
		{"one separate closed putaway document was created", f.scalar(`SELECT count(*) FROM warehouse.documents WHERE organization_id=$1 AND kind='transfer' AND state='closed'`, f.org), "1"},
		{"one putaway movement was committed", f.scalar(`SELECT count(*) FROM inventory.movements WHERE organization_id=$1 AND kind='transfer'`, f.org), "1"},
		{"putaway leaves no reservations", f.scalar(`SELECT count(*) FROM inventory.reservations WHERE organization_id=$1 AND active_qty>0`, f.org), "0"},
	}
	results := make(map[string]string, len(checks))
	for _, check := range checks {
		results[check.name] = check.got
		if check.got != check.want {
			t.Errorf("%s: got %s want %s", check.name, check.got, check.want)
		}
	}
	raw, err := json.MarshalIndent(map[string]any{"organization_id": f.org, "receipt_number": number, "checks": results}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "committed-state.json"), append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if t.Failed() {
		t.Fatal(fmt.Sprintf("kernel state mismatch; inspect %s", evidence))
	}
}
