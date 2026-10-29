package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeWorkspaceConfigRejectsSecretsAndAmbiguousCatalogs(t *testing.T) {
	valid := `{"oidc":{"url":"https://wms.example.invalid/auth","realm":"wms","clientId":"wms-browser"},"organizations":[{"id":"` + testOrg + `","name":"Складская компания"}],"warehouses":[{"id":"` + testWarehouse + `","organization_id":"` + testOrg + `","name":"Главный склад"}],"owners":[{"id":"` + testBalance + `","organization_id":"` + testOrg + `","name":"Владелец"}],"items":[{"id":"` + testLine + `","organization_id":"` + testOrg + `","sku":"SKU-1","name":"Товар","unit":"ea","quantity_scale":3}],"positions":[{"id":"` + testCommand + `","organization_id":"` + testOrg + `","warehouse_id":"` + testWarehouse + `","code":"A-01","kind":"storage"}]}`
	for _, tc := range []struct{ name, raw string }{
		{"secret field", strings.TrimSuffix(valid, "}") + `,"password":"do-not-publish"}`},
		{"secret OIDC field", strings.Replace(valid, `"clientId":"wms-browser"`, `"clientId":"wms-browser","clientSecret":"do-not-publish"`, 1)},
		{"multiple values", valid + " {}"},
		{"invalid identifier", strings.Replace(valid, testLine, "invalid", 1)},
		{"foreign organization", strings.Replace(valid, `"organization_id":"`+testOrg+`"`, `"organization_id":"`+testCommand+`"`, 1)},
		{"invalid precision", strings.Replace(valid, `"quantity_scale":3`, `"quantity_scale":7`, 1)},
		{"negative precision", strings.Replace(valid, `"quantity_scale":3`, `"quantity_scale":-1`, 1)},
		{"plaintext OIDC", strings.Replace(valid, "https://", "http://", 1)},
		{"URL credentials", strings.Replace(valid, "https://", "https://password@", 1)},
		{"URL query", strings.Replace(valid, "/auth\"", "/auth?secret=yes\"", 1)},
		{"empty name", strings.Replace(valid, `"name":"Товар"`, `"name":""`, 1)},
		{"excess size", strings.Repeat(" ", 4*1024*1024+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeWorkspaceConfig([]byte(tc.raw)); err == nil {
				t.Fatal("invalid or secret-bearing UI config was accepted")
			}
		})
	}
	t.Run("valid config remains exact", func(t *testing.T) {
		got, err := DecodeWorkspaceConfig([]byte(valid))
		if err != nil {
			t.Fatal(err)
		}
		if got.OIDC.ClientID != "wms-browser" || len(got.Items) != 1 || got.Items[0].QuantityScale != 3 || len(got.Positions) != 1 {
			t.Fatalf("valid configuration lost: %+v", got)
		}
		body, _ := json.Marshal(got)
		for _, tc := range []struct {
			name   string
			modify func(*WorkspaceConfig)
		}{
			{"duplicate item", func(c *WorkspaceConfig) { c.Items = append(c.Items, c.Items[0]) }},
			{"duplicate SKU", func(c *WorkspaceConfig) { v := c.Items[0]; v.ID = testCommand; c.Items = append(c.Items, v) }},
			{"duplicate scan address", func(c *WorkspaceConfig) { v := c.Positions[0]; v.ID = testLine; c.Positions = append(c.Positions, v) }},
			{"wrong address warehouse", func(c *WorkspaceConfig) { c.Positions[0].WarehouseID = testLine }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var c WorkspaceConfig
				_ = json.Unmarshal(body, &c)
				tc.modify(&c)
				raw, _ := json.Marshal(c)
				if _, err := DecodeWorkspaceConfig(raw); err == nil {
					t.Fatal("ambiguous catalog was accepted")
				}
			})
		}
	})
}
