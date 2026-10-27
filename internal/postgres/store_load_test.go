//go:build integration && load

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type loadCaseReport struct {
	Name                    string
	Concurrency, Repetition int
	loadSummary
	GuardCalls        int64
	PoolWaitMS        float64
	PoolEmptyAcquires int64
	DBWaitSamples     map[string]int
	TopSQL            []json.RawMessage
	Reconciled        bool
}

func monitorLoad(t *testing.T, s *loadService) func() map[string]int {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	samples := map[string]int{}
	go func() {
		defer close(done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rows, err := s.f.admin.Query(ctx, `SELECT coalesce(wait_event_type,'CPU'),coalesce(wait_event,'running'),count(*) FROM pg_stat_activity WHERE application_name='wms-load-api' AND state<>'idle' GROUP BY 1,2`)
				if err != nil {
					continue
				}
				for rows.Next() {
					var kind, event string
					var n int
					if rows.Scan(&kind, &event, &n) == nil {
						samples[kind+":"+event] += n
					}
				}
				rows.Close()
			}
		}
	}()
	return func() map[string]int { cancel(); <-done; return samples }
}

func loadTopSQL(t *testing.T, s *loadService) []json.RawMessage {
	t.Helper()
	rows, err := s.f.admin.Query(t.Context(), `SELECT jsonb_build_object('calls',calls,'total_ms',round(total_exec_time::numeric,3),'mean_ms',round(mean_exec_time::numeric,3),'rows',rows,'shared_hits',shared_blks_hit,'shared_reads',shared_blks_read,'wal_bytes',wal_bytes,'query',left(query,500)) FROM pg_stat_statements WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database()) ORDER BY total_exec_time DESC LIMIT 8`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []json.RawMessage
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		result = append(result, json.RawMessage(raw))
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func prepareLiveLoadFixture(t *testing.T, s *loadService) *fixture {
	t.Helper()
	f := newFixture(t)
	for _, permission := range []string{"stock.read", "reservation.read", "reservation.create", "reservation.release", "movement.read", "movement.create", "operation.read"} {
		f.exec(`INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES($1,$2,$3,$4,$5)`, s.seed.Actor, f.org, f.wh, f.owner, permission)
	}
	f.actor = s.seed.Actor
	return f
}

func parallelLoad(t *testing.T, s *loadService, name string, workers, repetition int, action func(int, *loadObservations)) loadCaseReport {
	t.Helper()
	s.f.exec(`SELECT pg_stat_statements_reset()`)
	stopMonitor := monitorLoad(t, s)
	before := s.f.runtime.Stat()
	guards := s.guards.Load()
	results := make([]loadObservations, workers)
	startGate := make(chan struct{})
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Go(func() { <-startGate; action(worker, &results[worker]) })
	}
	start := time.Now()
	close(startGate)
	wg.Wait()
	elapsed := time.Since(start)
	waits := stopMonitor()
	after := s.f.runtime.Stat()
	var combined loadObservations
	combined.codes = map[string]int{}
	for _, r := range results {
		combined.latencies = append(combined.latencies, r.latencies...)
		combined.successes += r.successes
		combined.conflicts += r.conflicts
		combined.technical += r.technical
		for code, count := range r.codes {
			combined.codes[code] += count
		}
	}
	return loadCaseReport{Name: name, Concurrency: workers, Repetition: repetition, loadSummary: combined.summary(elapsed), GuardCalls: s.guards.Load() - guards, PoolWaitMS: float64(after.EmptyAcquireWaitTime()-before.EmptyAcquireWaitTime()) / float64(time.Millisecond), PoolEmptyAcquires: after.EmptyAcquireCount() - before.EmptyAcquireCount(), DBWaitSamples: waits, TopSQL: loadTopSQL(t, s)}
}

func writeLoadRequest(ctx context.Context, s *loadService, o *loadObservations, path string, body any) (bool, map[string]any) {
	status, result, elapsed := s.request(ctx, "POST", path, s.ownerToken, id(), body)
	o.record(status, result.Code, elapsed)
	if status != 201 {
		return false, nil
	}
	var data map[string]any
	if json.Unmarshal(result.Data, &data) != nil {
		o.technical++
		return false, nil
	}
	return true, data
}

func runLoadCycles(t *testing.T, s *loadService, kind string, workers, repetition int, duration time.Duration) loadCaseReport {
	t.Helper()
	f := prepareLiveLoadFixture(t, s)
	_, credit := f.doc("receipt", "10000000", f.owner)
	f.exec(`SELECT inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,10000000)`, id(), f.actor, credit, f.source)
	lines := make([]string, workers)
	sources := make([]string, workers)
	for worker := range workers {
		_, lines[worker] = f.doc("shipment", "10000000", f.owner)
		sources[worker] = f.source
		if kind == "sku_distributed" {
			loc, pos := id(), id()
			f.exec(`INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES($1,$2,$3,$1::uuid::text,'storage')`, loc, f.org, f.wh)
			f.exec(`INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES($1,$2,$3,$4)`, pos, f.org, f.wh, loc)
			sources[worker] = f.scalar(`SELECT inventory.ensure_balance($1,$2,$3,$4,$5,'available')::text`, f.org, f.wh, f.owner, f.item, pos)
			_, credit = f.doc("receipt", "1000000", f.owner)
			f.exec(`SELECT inventory.post_movement($1,$2,$3,1,'receipt',NULL,$4,1000000)`, id(), f.actor, credit, sources[worker])
		}
	}
	var deadline time.Time
	// Initialized before the gate opens; no request is canceled just because
	// the offering window ended. In-flight complete cycles are drained.
	deadline = time.Now().Add(duration)
	report := parallelLoad(t, s, kind, workers, repetition, func(worker int, o *loadObservations) {
		version := 1
		for time.Now().Before(deadline) {
			reserve := f.reserveBody(lines[worker], "1", version)
			reserve["balance_id"] = sources[worker]
			ok, result := writeLoadRequest(t.Context(), s, o, "/api/v1/reservations", reserve)
			if !ok {
				return
			}
			version++
			reservation, ok := result["reservation_id"].(string)
			if !ok {
				t.Error("reserve result missing ID")
				return
			}
			source := sources[worker]
			if kind == "mixed_hot" && worker%2 == 0 {
				body := f.movementBody(lines[worker], "transfer", "1", version, reservation)
				body["source_balance_id"] = source
				ok, result = writeLoadRequest(t.Context(), s, o, "/api/v1/movements", body)
				if !ok {
					return
				}
				version++
				reservation, ok = result["new_reservation_id"].(string)
				if !ok {
					t.Error("pick result missing relocated reservation")
					return
				}
				source = f.target
			}
			issue := f.movementBody(lines[worker], "issue", "1", version, reservation)
			issue["source_balance_id"] = source
			ok, _ = writeLoadRequest(t.Context(), s, o, "/api/v1/movements", issue)
			if !ok {
				return
			}
			version++
		}
	})
	reconcileLoad(t, f)
	if got := f.scalar(`SELECT sum(active_qty)::text FROM inventory.reservations WHERE organization_id=$1`, f.org); got != "0.000000" {
		t.Errorf("unfinished cycle reservations=%s", got)
	}
	if report.TechnicalErrors != 0 || report.Conflicts != 0 {
		t.Errorf("unexpected write failures in %s: %+v", kind, report.Statuses)
	}
	report.Reconciled = true
	return report
}

func runLoadContentionContract(t *testing.T, s *loadService, kind string) loadCaseReport {
	t.Helper()
	f := prepareLiveLoadFixture(t, s)
	workers := 64
	lines := make([]string, workers)
	for i := range workers {
		_, lines[i] = f.doc("shipment", "1000", f.owner)
	}
	command := id()
	report := parallelLoad(t, s, kind, workers, 1, func(worker int, o *loadObservations) {
		line := lines[worker]
		key := id()
		if kind == "same_command" {
			line = lines[0]
			key = command
		}
		if kind == "same_document" {
			line = lines[0]
		}
		status, result, elapsed := s.request(t.Context(), "POST", "/api/v1/reservations", s.ownerToken, key, f.reserveBody(line, "1", 1))
		o.record(status, result.Code, elapsed)
	})
	wantSuccess, wantReserved := 10, "10.000000"
	if kind == "same_command" {
		wantSuccess, wantReserved = workers, "1.000000"
	}
	if kind == "same_document" {
		wantSuccess, wantReserved = 1, "1.000000"
	}
	if report.Successes != wantSuccess || report.TechnicalErrors != 0 {
		t.Errorf("contention contract %s: successes=%d statuses=%v", kind, report.Successes, report.Statuses)
	}
	if got := f.scalar(`SELECT reserved_qty::text FROM inventory.stock_balances WHERE id=$1`, f.source); got != wantReserved {
		t.Errorf("reserved=%s want=%s", got, wantReserved)
	}
	if kind == "same_command" && report.GuardCalls != 1 {
		t.Errorf("same ID ran required guard %d times", report.GuardCalls)
	}
	reconcileLoad(t, f)
	report.Reconciled = true
	return report
}

func TestLoadConcurrent(t *testing.T) {
	s := newLoadService(t)
	duration := time.Duration(loadNumber(t, "WMS_LOAD_SECONDS", 5, 1, 30)) * time.Second
	repetitions := loadNumber(t, "WMS_LOAD_REPETITIONS", 3, 1, 10)
	var reports []loadCaseReport
	t.Cleanup(func() {
		writeLoadJSON(t, filepath.Join(loadPhaseDirectory(t), "concurrency.json"), map[string]any{"environment": loadBuildMetadata(), "fixture": s.seed, "cases": reports, "passed": !t.Failed()})
	})
	for _, kind := range []string{"hot_balance", "mixed_hot", "sku_distributed"} {
		for _, workers := range []int{1, 16, 64} {
			for sample := 1; sample <= repetitions; sample++ {
				t.Run(fmt.Sprintf("%s/c%d/run%d", kind, workers, sample), func(t *testing.T) { reports = append(reports, runLoadCycles(t, s, kind, workers, sample, duration)) })
			}
		}
	}
	for _, kind := range []string{"last_units", "same_command", "same_document"} {
		t.Run(kind, func(t *testing.T) { reports = append(reports, runLoadContentionContract(t, s, kind)) })
	}
	for _, c := range []historyReadCase{historyReadCases[1], historyReadCases[2]} {
		t.Run("read_"+c.name, func(t *testing.T) {
			path, token := s.readPath(t, c)
			deadline := time.Now().Add(duration)
			report := parallelLoad(t, s, "read_"+c.name, 16, 1, func(_ int, o *loadObservations) {
				for time.Now().Before(deadline) {
					status, result, elapsed := s.request(t.Context(), "GET", path, token, "", nil)
					o.record(status, result.Code, elapsed)
				}
			})
			if report.TechnicalErrors != 0 {
				t.Errorf("read overload errors: %v", report.Statuses)
			}
			report.Reconciled = true
			reports = append(reports, report)
		})
	}
	reconcileLoad(t, s.f)
}
