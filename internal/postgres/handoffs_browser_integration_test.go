//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/api"
	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/ui"
	"github.com/Shpuntyara/wms-platform/web"
)

type handoffBrowserAuth struct{ sender, receiver string }

func (a handoffBrowserAuth) Authenticate(_ context.Context, token string) (auth.Identity, error) {
	subject := ""
	switch token {
	case "valid-sender":
		subject = a.sender
	case "valid-receiver":
		subject = a.receiver
	default:
		return auth.Identity{}, errors.New("invalid test identity")
	}
	return auth.Identity{Issuer: "https://tests.invalid", Subject: subject}, nil
}

// Only the external OIDC adapter is replaced; browser requests use real HTTP,
// independently resolved employee grants and PostgreSQL custody transactions.
func TestBrowserHandoffsAgainstPostgres(t *testing.T) {
	f := newHandoffFixture(t)
	f.exec(`DELETE FROM platform.actor_grants WHERE actor_id IN($1,$2) AND permission NOT IN('stock.read','handoff.read','handoff.send','handoff.accept')`, f.sender, f.receiver)
	f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,'stock.read') ON CONFLICT DO NOTHING`, f.receiver, f.org, f.wh, f.owner)
	f.exec(`UPDATE catalog.items SET sku='SR-M8' WHERE id=$1`, f.item)
	workspace := api.WorkspaceConfig{
		OIDC:          api.BrowserOIDC{URL: "https://identity.tests.invalid", Realm: "handoff-test", ClientID: "wms-browser"},
		Organizations: []api.NamedOrganization{{ID: f.org, Name: "METALFORM"}},
		Warehouses:    []api.NamedWarehouse{{ID: f.wh, OrganizationID: f.org, Name: "Magazyn metalu"}},
		Owners:        []api.NamedOwner{{ID: f.owner, OrganizationID: f.org, Name: "METALFORM"}},
		Items:         []api.NamedItem{{ID: f.item, OrganizationID: f.org, SKU: "SR-M8", Name: "Śruba M8", Unit: "szt.", QuantityScale: 3}},
	}
	for i, code := range []string{"A1", "SPAWANIE", "SZLIFOWANIE", "LAKIERNIA", "MONTAZ"} {
		workspace.Positions = append(workspace.Positions, api.NamedPosition{ID: f.positions[i], OrganizationID: f.org, WarehouseID: f.wh, Code: code, Kind: "storage"})
	}
	registry, err := modules.NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := api.New(api.Config{Backend: f.store, Auth: handoffBrowserAuth{sender: f.sender, receiver: f.receiver}, Modules: registry, Workspace: workspace})
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
		t.Fatal("real browser requires Node.js", err)
	}
	cli := filepath.Join(repo, "web", "node_modules", "@playwright", "test", "cli.js")
	if _, err = os.Stat(cli); err != nil {
		t.Fatal("real browser requires npm ci and Chromium", err)
	}
	evidence := os.Getenv("WMS_HANDOFF_LIVE_EVIDENCE_DIR")
	if evidence == "" {
		evidence = filepath.Join(repo, "work", "handoff-live-browser")
	}
	evidence = filepath.Join(evidence, f.org)
	if err = os.MkdirAll(evidence, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, node, cli, "test", "--config", "playwright.handoff-live.config.mjs")
	command.Dir = filepath.Join(repo, "web")
	command.Env = append(os.Environ(), "WMS_HANDOFF_LIVE_URL="+server.URL, "WMS_HANDOFF_LIVE_ORG="+f.org, "WMS_HANDOFF_LIVE_WAREHOUSE="+f.wh, "WMS_HANDOFF_LIVE_OWNER="+f.owner, "WMS_HANDOFF_LIVE_ITEM="+f.item, "WMS_HANDOFF_LIVE_SOURCE="+f.source, "WMS_HANDOFF_LIVE_DESTINATION_DEPARTMENT="+f.departments[1], "WMS_HANDOFF_LIVE_SENDER="+f.sender, "WMS_HANDOFF_LIVE_RECEIVER="+f.receiver, "WMS_HANDOFF_LIVE_ARTIFACTS="+filepath.Join(evidence, "browser"))
	output, runErr := command.CombinedOutput()
	if err = os.WriteFile(filepath.Join(evidence, "browser.txt"), output, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("Real handoff browser evidence: %s\n%s", evidence, output)
	if runErr != nil {
		t.Fatal("real PostgreSQL handoff browser workflow failed", runErr)
	}
	if f.scalar(`SELECT (count(*)>0 AND bool_and(state='accepted'))::text FROM warehouse.handoffs WHERE organization_id=$1`, f.org) != "true" {
		t.Fatal("HANDOFF_ASSERTION browser did not finish authenticated handoff")
	}
	if f.scalar(`SELECT sum(on_hand)::text FROM inventory.stock_balances WHERE organization_id=$1`, f.org) != "10.000000" {
		t.Fatal("HANDOFF_ASSERTION browser lost or manufactured stock")
	}
	if f.scalar(`SELECT count(*) FROM catalog.items WHERE organization_id=$1`, f.org) != "1" {
		t.Fatal("HANDOFF_ASSERTION browser manufactured product identity")
	}
	if f.scalar(`SELECT (count(*)=2*(SELECT count(*) FROM warehouse.handoffs WHERE organization_id=$1))::text FROM warehouse.documents WHERE organization_id=$1 AND kind='transfer' AND state='closed'`, f.org) != "true" {
		t.Fatal("HANDOFF_ASSERTION browser did not post exactly two MM for each handoff")
	}
}
