package api

import (
	"strings"
	"testing"
)

func TestQueryFiltersCannotDisappearOnMalformedEncoding(t *testing.T) {
	path := "/api/v1/stock-balances?organization_id=" + testOrg + "&warehouse_id=" + testWarehouse
	for _, query := range []string{"owner_id=%zz", "item_id=%", "position_id=%Q0", "owner_id=" + testBalance + ";ignored=value", "owner_id=%zz&owner_id=" + testBalance} {
		t.Run(query, func(t *testing.T) {
			h, backend := testHandler(t)
			w := request(h, "GET", path+"&"+query, "", "valid", "", "")
			if w.Code != 400 || backend.calls != 0 {
				t.Fatalf("malformed filter reached backend: status=%d calls=%d body=%s", w.Code, backend.calls, w.Body)
			}
		})
	}
}

func TestRequestBodySizeBoundaryIsExact(t *testing.T) {
	valid := `{"organization_id":"` + testOrg + `","warehouse_id":"` + testWarehouse + `","document_line_id":"` + testLine + `","expected_document_version":1,"balance_id":"` + testBalance + `","quantity":"1"}`
	for _, tc := range []struct {
		name   string
		size   int
		status int
	}{
		{name: "exactly one MiB", size: 1024 * 1024, status: 201},
		{name: "one byte above limit", size: 1024*1024 + 1, status: 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, backend := testHandler(t)
			body := valid + strings.Repeat(" ", tc.size-len(valid))
			w := request(h, "POST", "/api/v1/reservations", body, "valid", testCommand, "application/json")
			if w.Code != tc.status || tc.status == 413 && backend.calls != 0 {
				t.Fatalf("size=%d status=%d calls=%d body=%s", tc.size, w.Code, backend.calls, w.Body)
			}
		})
	}
}
