//go:build integration && load

package postgres_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/api"
	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type loadSeed struct {
	Org, Warehouse, Owner, Actor, AllActor, Item, Source, Target, Foreign string
	Movements, Reservations, Balances                                     int
	DeepIDs                                                               map[string]string
}

func loadDirectory(t testing.TB) string {
	t.Helper()
	dir := os.Getenv("WMS_LOAD_DIR")
	if dir == "" {
		t.Fatal("WMS_LOAD_DIR must point to an isolated load session")
	}
	return dir
}

func writeLoadJSON(t testing.TB, path string, value any) {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadNumber(t testing.TB, key string, fallback, low, high int) int {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < low || n > high {
		t.Fatalf("invalid bounded load option %s", key)
	}
	return n
}

func TestLoadSeed(t *testing.T) {
	f := newFixture(t)
	seed := loadSeed{Org: f.org, Warehouse: f.wh, Owner: f.owner, Actor: f.actor, AllActor: id(), Item: f.item, Source: f.source, Target: f.target, Foreign: f.foreign,
		Movements: loadNumber(t, "WMS_LOAD_MOVEMENTS", 1000000, 1000, 2000000), Reservations: loadNumber(t, "WMS_LOAD_RESERVATIONS", 250000, 1000, 500000), Balances: loadNumber(t, "WMS_LOAD_BALANCES", 100000, 200, 200000), DeepIDs: map[string]string{}}
	if seed.Balances%200 != 0 {
		t.Fatal("load balances must be divisible by 200")
	}
	_, credit := f.doc("receipt", "10000000", f.owner)
	f.exec(`SELECT inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,10000000)`, id(), f.actor, credit, f.source)
	f.exec(`INSERT INTO platform.actors(id,issuer,subject) VALUES($1,'https://tests.invalid',$1::uuid::text)`, seed.AllActor)
	for _, permission := range []string{"stock.read", "reservation.read", "movement.read"} {
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,permission) VALUES($1,$2,$3,$4)`, seed.AllActor, f.org, f.wh, permission)
	}
	raw, err := os.ReadFile("../../tests/load/seed-history.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.admin.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = tx.Rollback(ctx)
	}()
	if _, err = tx.Exec(t.Context(), `CREATE TEMP TABLE load_config(org uuid,wh uuid,owner_id uuid,actor uuid,item uuid,wh2 uuid,movements bigint,reservations bigint,balances bigint) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `INSERT INTO load_config VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, f.org, f.wh, f.owner, f.actor, f.item, id(), seed.Movements, seed.Reservations, seed.Balances); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeding movements=%d reservations=%d balances=%d with normal constraints", seed.Movements, seed.Reservations, seed.Balances)
	if _, err = tx.Exec(t.Context(), string(raw)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.exec(`VACUUM (ANALYZE)`)
	for _, resource := range []string{"movements", "reservations", "stock_balances"} {
		seed.DeepIDs[resource] = f.scalar("SELECT id::text FROM inventory."+resource+" WHERE organization_id=$1 AND warehouse_id=$2 AND owner_id=$3 ORDER BY id OFFSET (SELECT count(*)*9/10 FROM inventory."+resource+" WHERE organization_id=$1 AND warehouse_id=$2 AND owner_id=$3) LIMIT 1", f.org, f.wh, f.owner)
		seed.DeepIDs[resource+"_all"] = f.scalar("SELECT id::text FROM inventory."+resource+" WHERE organization_id=$1 AND warehouse_id=$2 ORDER BY id OFFSET (SELECT count(*)*9/10 FROM inventory."+resource+" WHERE organization_id=$1 AND warehouse_id=$2) LIMIT 1", f.org, f.wh)
	}
	writeLoadJSON(t, filepath.Join(loadDirectory(t), "fixture.json"), seed)
	reconcileLoad(t, f)
}

func reconcileLoad(t testing.TB, f *fixture) {
	t.Helper()
	queries := []string{
		`WITH active AS (SELECT balance_id,sum(active_qty) qty FROM inventory.reservations WHERE organization_id=$1 GROUP BY balance_id) SELECT count(*)::text FROM inventory.stock_balances b LEFT JOIN active a ON a.balance_id=b.id WHERE b.organization_id=$1 AND b.reserved_qty<>coalesce(a.qty,0)`,
		`WITH deltas AS (SELECT l.target_balance_id balance_id,l.quantity qty FROM inventory.movement_lines l JOIN inventory.movements m ON m.id=l.movement_id WHERE m.organization_id=$1 AND l.target_balance_id IS NOT NULL UNION ALL SELECT l.source_balance_id,-l.quantity FROM inventory.movement_lines l JOIN inventory.movements m ON m.id=l.movement_id WHERE m.organization_id=$1 AND l.source_balance_id IS NOT NULL), totals AS (SELECT balance_id,sum(qty) qty FROM deltas GROUP BY balance_id) SELECT count(*)::text FROM inventory.stock_balances b LEFT JOIN totals a ON a.balance_id=b.id WHERE b.organization_id=$1 AND b.on_hand<>coalesce(a.qty,0)`,
		`WITH active AS (SELECT document_line_id,sum(active_qty) qty FROM inventory.reservations WHERE organization_id=$1 GROUP BY document_line_id) SELECT count(*)::text FROM warehouse.document_lines l LEFT JOIN active a ON a.document_line_id=l.id WHERE l.organization_id=$1 AND l.reserved_qty<>coalesce(a.qty,0)`,
		`SELECT count(*)::text FROM platform.operations WHERE organization_id=$1 AND completed_at IS NULL`,
		`SELECT count(*)::text FROM platform.operations o WHERE organization_id=$1 AND (NOT EXISTS(SELECT 1 FROM platform.audit_entries a WHERE a.operation_id=o.id) OR NOT EXISTS(SELECT 1 FROM platform.outbox b WHERE b.operation_id=o.id))`,
	}
	for i, query := range queries {
		if got := f.scalar(query, f.org); got != "0" {
			t.Fatalf("load reconciliation%d mismatches=%s", i, got)
		}
	}
}

type loadQuery struct {
	SQL  string
	Args []any
}
type loadCapture struct {
	mu   sync.Mutex
	last loadQuery
}

func (c *loadCapture) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(d.SQL, "SELECT jsonb_build_object") {
		c.mu.Lock()
		c.last = loadQuery{d.SQL, slices.Clone(d.Args)}
		c.mu.Unlock()
	}
	return ctx
}
func (c *loadCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (c *loadCapture) snapshot() loadQuery                                             { c.mu.Lock(); defer c.mu.Unlock(); return c.last }

type loadService struct {
	f                    *fixture
	seed                 loadSeed
	server               *httptest.Server
	client               *http.Client
	ownerToken, allToken string
	guards               atomic.Int64
	capture              *loadCapture
}

func newLoadService(t testing.TB) *loadService {
	t.Helper()
	dir := loadDirectory(t)
	var seed loadSeed
	raw, err := os.ReadFile(filepath.Join(dir, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	adminDSN, runtimeDSN := os.Getenv("WMS_TEST_ADMIN_DSN"), os.Getenv("WMS_TEST_RUNTIME_DSN")
	if err = validateTestDatabaseDSNs(adminDSN, runtimeDSN); err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.New(t.Context(), adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	config, err := pgxpool.ParseConfig(runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 20
	config.MinConns = 20
	config.ConnConfig.RuntimeParams["application_name"] = "wms-load-api"
	capture := &loadCapture{}
	config.ConnConfig.Tracer = capture
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store, err := postgres.New(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, admin: admin, runtime: pool, store: store, org: seed.Org, wh: seed.Warehouse, owner: seed.Owner, actor: seed.Actor, item: seed.Item, source: seed.Source, target: seed.Target, foreign: seed.Foreign}
	s := &loadService{f: f, seed: seed, capture: capture}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuer := "https://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/.well-known/openid-configuration" {
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
			return
		}
		if r.URL.Path == "/keys" {
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "load", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(provider.Close)
	f.exec(`UPDATE platform.actors SET issuer=$1 WHERE id IN($2,$3)`, provider.URL, seed.Actor, seed.AllActor)
	verifier, err := auth.NewOIDC(t.Context(), provider.URL, "wms-api", provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	s.ownerToken = signLoadToken(t, key, provider.URL, seed.Actor)
	s.allToken = signLoadToken(t, key, provider.URL, seed.AllActor)
	points := []string{modules.ReservationPoint, modules.ReleasePoint, modules.MovementPoint, modules.DispatchPoint}
	m := modules.Manifest{ManifestVersion: 1, ID: "load.required", Version: "1.0.0", Compatibility: modules.Compatibility{Core: ">=0.3.0 <1.0.0", SDK: ">=2.0.0 <3.0.0"}}
	for _, point := range points {
		m.Guards = append(m.Guards, modules.GuardSpec{Point: point, Handler: "Check", Required: true, FailurePolicy: "deny", TimeoutMS: 100})
	}
	code := modules.Module{ID: m.ID, Version: m.Version, GuardPoints: map[string][]string{"Check": points}, Guards: map[string]modules.Guard{"Check": func(ctx context.Context, _ modules.Check) error { s.guards.Add(1); return ctx.Err() }}}
	registry, err := modules.NewRegistry([]modules.Manifest{m}, []modules.Module{code}, []string{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.New(api.Config{Backend: store, Auth: verifier, Modules: registry})
	if err != nil {
		t.Fatal(err)
	}
	s.server = httptest.NewServer(handler)
	t.Cleanup(s.server.Close)
	transport := &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 128, MaxConnsPerHost: 128, IdleConnTimeout: time.Minute}
	s.client = &http.Client{Transport: transport, Timeout: 8 * time.Second}
	t.Cleanup(transport.CloseIdleConnections)
	return s
}

func signLoadToken(t testing.TB, key *rsa.PrivateKey, issuer, subject string) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "at+jwt", "kid": "load"})
	body, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "wms-api", "sub": subject, "exp": time.Now().Add(2 * time.Hour).Unix()})
	value := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	hash := sha256.Sum256([]byte(value))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	return value + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type loadResponse struct {
	Data       json.RawMessage `json:"data"`
	Code       string          `json:"code"`
	Pagination struct {
		NextCursor string `json:"next_cursor"`
	} `json:"pagination"`
}

func (s *loadService) request(ctx context.Context, method, path, token, command string, body any) (int, loadResponse, time.Duration) {
	start := time.Now()
	var result loadResponse
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, result, time.Since(start)
	}
	r, err := http.NewRequestWithContext(ctx, method, s.server.URL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, result, time.Since(start)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	if command != "" {
		r.Header.Set("Idempotency-Key", command)
	}
	response, err := s.client.Do(r)
	if err != nil {
		result.Code = "client_transport_error"
		return 0, result, time.Since(start)
	}
	defer response.Body.Close()
	if err = json.NewDecoder(response.Body).Decode(&result); err != nil {
		result.Code = "invalid_response"
		return 0, result, time.Since(start)
	}
	return response.StatusCode, result, time.Since(start)
}

type loadLatency struct {
	Count                      int `json:"count"`
	P50MS, P95MS, P99MS, MaxMS float64
}

func summarizeLatencies(input []time.Duration) loadLatency {
	values := slices.Clone(input)
	slices.Sort(values)
	if len(values) == 0 {
		return loadLatency{}
	}
	q := func(p float64) float64 {
		return float64(values[int(math.Ceil(float64(len(values))*p))-1]) / float64(time.Millisecond)
	}
	return loadLatency{Count: len(values), P50MS: q(.5), P95MS: q(.95), P99MS: q(.99), MaxMS: q(1)}
}

type loadObservations struct {
	latencies                       []time.Duration
	codes                           map[string]int
	successes, conflicts, technical int
}

func (o *loadObservations) record(status int, code string, elapsed time.Duration) {
	o.latencies = append(o.latencies, elapsed)
	if o.codes == nil {
		o.codes = map[string]int{}
	}
	o.codes[fmt.Sprintf("%d:%s", status, code)]++
	if status >= 200 && status < 300 {
		o.successes++
	} else if status == 409 {
		o.conflicts++
	} else {
		o.technical++
	}
}

type loadSummary struct {
	Latency                               loadLatency
	Statuses                              map[string]int
	Successes, Conflicts, TechnicalErrors int
	RequestsPerSecond, ElapsedSeconds     float64
}

func (o *loadObservations) summary(elapsed time.Duration) loadSummary {
	return loadSummary{Latency: summarizeLatencies(o.latencies), Statuses: o.codes, Successes: o.successes, Conflicts: o.conflicts, TechnicalErrors: o.technical, RequestsPerSecond: float64(len(o.latencies)) / elapsed.Seconds(), ElapsedSeconds: elapsed.Seconds()}
}

func loadBuildMetadata() map[string]any {
	return map[string]any{"go": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH, "logical_cpus": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0), "driver": "closed-loop real loopback HTTP, real RSA OIDC JWT verification, runtime PostgreSQL role, required synthetic pure guards", "limitations": "App, driver, OIDC fixture and PostgreSQL share one host. Warm-cache local measurement; no ingress, real customer guards, external IdP or failover."}
}
