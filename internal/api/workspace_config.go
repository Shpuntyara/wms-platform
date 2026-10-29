package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
)

type BrowserOIDC struct {
	URL      string `json:"url"`
	Realm    string `json:"realm"`
	ClientID string `json:"clientId"`
}

type NamedOrganization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type NamedWarehouse struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	Name           string `json:"name"`
}

type NamedOwner struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	Name           string `json:"name"`
}

type NamedItem struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	SKU            string `json:"sku"`
	Name           string `json:"name"`
	Unit           string `json:"unit"`
	QuantityScale  int    `json:"quantity_scale"`
}

type NamedPosition struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	WarehouseID    string `json:"warehouse_id"`
	Code           string `json:"code"`
	Kind           string `json:"kind"`
}

// WorkspaceConfig contains operator-maintained labels, never authorization.
// Catalog entries are returned only for the employee's current read scopes.
type WorkspaceConfig struct {
	OIDC          BrowserOIDC         `json:"oidc"`
	Organizations []NamedOrganization `json:"organizations"`
	Warehouses    []NamedWarehouse    `json:"warehouses"`
	Owners        []NamedOwner        `json:"owners"`
	Items         []NamedItem         `json:"items"`
	Positions     []NamedPosition     `json:"positions"`
}

// DecodeWorkspaceConfig validates an operator-provided UI label catalog.
func DecodeWorkspaceConfig(raw []byte) (WorkspaceConfig, error) {
	invalid := errors.New("invalid UI configuration: use the documented public OIDC settings and unique catalog entries")
	var config WorkspaceConfig
	if len(raw) > 4*1024*1024 {
		return config, invalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&config); err != nil {
		return WorkspaceConfig{}, invalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return WorkspaceConfig{}, invalid
	}
	if config.OIDC != (BrowserOIDC{}) {
		u, err := url.Parse(config.OIDC.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(config.OIDC.URL) > 2048 || !label(config.OIDC.Realm, 200) || strings.ContainsAny(config.OIDC.Realm, "/\\?#") || !label(config.OIDC.ClientID, 200) {
			return WorkspaceConfig{}, invalid
		}
	}
	seen := map[string]bool{}
	claim := func(kind, id string) bool {
		normalized, err := inventory.NormalizeID(id)
		key := kind + "|" + id
		if err != nil || normalized != id || seen[key] {
			return false
		}
		seen[key] = true
		return true
	}
	organizations := map[string]bool{}
	warehouses := map[[2]string]bool{}
	for _, v := range config.Organizations {
		if !claim("organization", v.ID) || !label(v.Name, 1000) {
			return WorkspaceConfig{}, invalid
		}
		organizations[v.ID] = true
	}
	for _, v := range config.Warehouses {
		if !claim("warehouse", v.ID) || !organizations[v.OrganizationID] || !label(v.Name, 1000) {
			return WorkspaceConfig{}, invalid
		}
		warehouses[[2]string{v.OrganizationID, v.ID}] = true
	}
	for _, v := range config.Owners {
		if !claim("owner", v.ID) || !organizations[v.OrganizationID] || !label(v.Name, 1000) {
			return WorkspaceConfig{}, invalid
		}
	}
	skus := map[[2]string]bool{}
	for _, v := range config.Items {
		key := [2]string{v.OrganizationID, v.SKU}
		if !claim("item", v.ID) || !organizations[v.OrganizationID] || !label(v.Name, 1000) || !label(v.SKU, 200) || !label(v.Unit, 32) || v.QuantityScale < 0 || v.QuantityScale > 6 || skus[key] {
			return WorkspaceConfig{}, invalid
		}
		skus[key] = true
	}
	addresses := map[[3]string]bool{}
	for _, v := range config.Positions {
		key := [3]string{v.OrganizationID, v.WarehouseID, strings.ToUpper(v.Code)}
		if !claim("position", v.ID) || !warehouses[[2]string{v.OrganizationID, v.WarehouseID}] || !label(v.Code, 200) || addresses[key] {
			return WorkspaceConfig{}, invalid
		}
		switch v.Kind {
		case "receiving", "storage", "staging", "shipping", "returns", "quarantine":
		default:
			return WorkspaceConfig{}, invalid
		}
		addresses[key] = true
	}
	return config, nil
}

func label(value string, limit int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && len(value) <= limit && !strings.ContainsFunc(value, unicode.IsControl)
}
