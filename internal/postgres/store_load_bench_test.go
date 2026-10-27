//go:build integration && load

package postgres_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type historyReadCase struct {
	name, resource  string
	allOwners, deep bool
}

var historyReadCases = []historyReadCase{
	{name: "MovementsOwnerFirst", resource: "movements"},
	{name: "MovementsOwnerDeep", resource: "movements", deep: true},
	{name: "MovementsAllDeep", resource: "movements", allOwners: true, deep: true},
	{name: "ReservationsOwnerDeep", resource: "reservations", deep: true},
	{name: "StockOwnerDeep", resource: "stock_balances", deep: true},
}

func loadPhaseDirectory(t testing.TB) string {
	t.Helper()
	label := os.Getenv("WMS_LOAD_LABEL")
	if label == "" {
		label = "current"
	}
	if label != filepath.Base(label) || strings.ContainsAny(label, "\\/:.") {
		t.Fatal("invalid load label")
	}
	return filepath.Join(loadDirectory(t), label)
}

func (s *loadService) readPath(t testing.TB, c historyReadCase) (string, string) {
	t.Helper()
	resource := c.resource
	if resource == "stock_balances" {
		resource = "stock-balances"
	}
	path := s.f.scopePath(resource) + "&limit=50"
	token := s.ownerToken
	if c.allOwners {
		token = s.allToken
	}
	status, result, _ := s.request(t.Context(), "GET", path, token, "", nil)
	if status != 200 {
		t.Fatalf("read preparation status=%d code=%s", status, result.Code)
	}
	if c.deep {
		if result.Pagination.NextCursor == "" {
			t.Fatal("history fixture is too small for pagination")
		}
		raw, err := base64.RawURLEncoding.DecodeString(result.Pagination.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		var cursor map[string]any
		if err = json.Unmarshal(raw, &cursor); err != nil {
			t.Fatal(err)
		}
		key := c.resource
		if c.allOwners {
			key += "_all"
		}
		cursor["id"] = s.seed.DeepIDs[key]
		raw, err = json.Marshal(cursor)
		if err != nil {
			t.Fatal(err)
		}
		// Equivalent to a cursor returned at the90th percentile; no OFFSET is
		// sent to the application. Only fixture setup computes this anchor.
		path += "&cursor=" + base64.RawURLEncoding.EncodeToString(raw)
	}
	return path, token
}

func BenchmarkHistoryHTTP(b *testing.B) {
	s := newLoadService(b)
	for _, c := range historyReadCases {
		b.Run(c.name, func(b *testing.B) {
			path, token := s.readPath(b, c)
			// Warm OIDC/JWKS, connections, data and pgx's prepared-statement cache.
			for range 20 {
				status, result, _ := s.request(b.Context(), "GET", path, token, "", nil)
				if status != 200 {
					b.Fatalf("warm-up status=%d code=%s", status, result.Code)
				}
			}
			query := s.capture.snapshot()
			b.ReportAllocs()
			for b.Loop() {
				status, result, _ := s.request(b.Context(), "GET", path, token, "", nil)
				if status != 200 {
					b.Fatalf("HTTP status=%d code=%s", status, result.Code)
				}
				var rows []map[string]any
				if err := json.Unmarshal(result.Data, &rows); err != nil || len(rows) == 0 || len(rows) > 50 {
					b.Fatalf("invalid bounded page: rows=%d err=%v", len(rows), err)
				}
				for _, row := range rows {
					if row["organization_id"] != s.seed.Org || row["warehouse_id"] != s.seed.Warehouse || (!c.allOwners && row["owner_id"] != s.seed.Owner) {
						b.Fatal("history benchmark leaked an unauthorized row")
					}
				}
			}
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				writeHistoryPlan(b, s, query, c.name, mode)
			}
		})
	}
}

func loadSQLLiteral(t testing.TB, value any) string {
	t.Helper()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	switch v := value.(type) {
	case nil:
		return "NULL"
	case string:
		return quote(v)
	case bool:
		if v {
			return "TRUE"
		}
		return "FALSE"
	case int:
		return strconv.Itoa(v)
	case []string:
		values := make([]string, len(v))
		for i, s := range v {
			values[i] = quote(s)
		}
		return "ARRAY[" + strings.Join(values, ",") + "]::text[]"
	default:
		t.Fatalf("unsupported captured bind type %T", value)
		return ""
	}
}

func writeHistoryPlan(t testing.TB, s *loadService, query loadQuery, name, mode string) {
	t.Helper()
	if query.SQL == "" {
		t.Fatal("application query was not captured")
	}
	conn, err := s.f.runtime.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	_, err = conn.Exec(t.Context(), "SET plan_cache_mode="+mode)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(t.Context(), "PREPARE wms_load_plan AS "+query.SQL)
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, len(query.Args))
	for i, value := range query.Args {
		values[i] = loadSQLLiteral(t, value)
	}
	var raw []byte
	err = conn.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,SETTINGS,FORMAT JSON) EXECUTE wms_load_plan("+strings.Join(values, ",")+")").Scan(&raw)
	_, cleanupErr := conn.Exec(t.Context(), "DEALLOCATE wms_load_plan; RESET plan_cache_mode")
	if err != nil {
		t.Fatal(err)
	}
	if cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	var plan any
	if err = json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	writeLoadJSON(t, filepath.Join(loadPhaseDirectory(t), "plans", fmt.Sprintf("%s_%s.json", name, mode)), map[string]any{"sql": query.SQL, "args": query.Args, "plan": plan})
}

// Logical I/O is a stable regression signal; an elapsed-time threshold would
// make this correctness check depend on the speed of the developer machine.
func TestLoadPageWorkStaysBounded(t *testing.T) {
	s := newLoadService(t)
	for _, c := range historyReadCases {
		t.Run(c.name, func(t *testing.T) {
			path, token := s.readPath(t, c)
			status, result, _ := s.request(t.Context(), "GET", path, token, "", nil)
			if status != 200 {
				t.Fatalf("status=%d code=%s", status, result.Code)
			}
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				writeHistoryPlan(t, s, s.capture.snapshot(), c.name, mode)
				raw, err := os.ReadFile(filepath.Join(loadPhaseDirectory(t), "plans", fmt.Sprintf("%s_%s.json", c.name, mode)))
				if err != nil {
					t.Fatal(err)
				}
				var report struct {
					Plan []struct {
						Root struct {
							Hits  int `json:"Shared Hit Blocks"`
							Reads int `json:"Shared Read Blocks"`
						} `json:"Plan"`
					} `json:"plan"`
				}
				if err := json.Unmarshal(raw, &report); err != nil {
					t.Fatal(err)
				}
				if len(report.Plan) != 1 {
					t.Fatal("missing executed query plan")
				}
				blocks := report.Plan[0].Root.Hits + report.Plan[0].Root.Reads
				if blocks > 1000 {
					t.Errorf("%s reads %d shared blocks for at most 51 rows; pagination scans history", mode, blocks)
				}
			}
		})
	}
}
