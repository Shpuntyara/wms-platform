package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIndustryArchiveLabelsRoundTripWithoutErasingCatalog(t *testing.T) {
	raw := strings.Replace(orderWorkspaceJSON("16"), `"sku":"SR-M8"`, `"sku":"SR-M8","archived":true`, 1)
	c, err := DecodeWorkspaceConfig([]byte(raw))
	if err != nil {
		t.Fatalf("explicit prior-test catalog grouping rejected: %v", err)
	}
	encoded, _ := json.Marshal(c)
	var config struct {
		Items []struct {
			ID       string
			SKU      string
			Archived bool
		}
	}
	if err = json.Unmarshal(encoded, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Items) != 2 || config.Items[1].ID != testBalance || config.Items[1].SKU != "SR-M8" || !config.Items[1].Archived || config.Items[0].Archived {
		t.Fatalf("archive metadata changed item identity or omitted old rows: %s", encoded)
	}
}
