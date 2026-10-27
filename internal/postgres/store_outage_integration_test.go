//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/jobs"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBrokerOutageKeepsAPIAvailableAndRecovers(t *testing.T) {
	f := newFixture(t)
	outage := 2 * time.Second
	if raw := os.Getenv("WMS_BROKER_OUTAGE_SECONDS"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 2 || seconds > 600 {
			t.Fatal("invalid outage duration")
		}
		outage = time.Duration(seconds) * time.Second
	}
	var mu sync.Mutex
	delivered := map[string]bool{}
	receiver := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(400)
			return
		}
		// The database outbox envelope includes the stable event identity.
		event, _ := payload["event_id"].(string)
		if event == "" {
			t.Error("missing event identity")
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		delivered[event] = true
		mu.Unlock()
		w.WriteHeader(204)
	})
	broker := httptest.NewTLSServer(receiver)
	address := broker.Listener.Addr().String()
	url := broker.URL
	tlsConfig := broker.TLS.Clone()
	client := broker.Client()
	broker.Close() // A real refused TCP connection throughout the outage.
	defer client.CloseIdleConnections()
	config := f.runtime.Config().Copy()
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store, err := postgres.New(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	worker := &jobs.Worker{Store: store, Sender: brokerSender{client: client, url: url}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	defer func() { cancel(); <-done }()
	api := f.handler(nil, nil)
	start := time.Now()
	next := time.NewTicker(200 * time.Millisecond)
	defer next.Stop()
	writes := 0
	unhealthyObserved := false
	for time.Since(start) < outage {
		<-next.C
		ready := httptest.NewRecorder()
		api.ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
		requireStatus(t, ready, 200)
		requireStatus(t, f.request(api, "GET", f.scopePath("stock-balances"), nil, ""), 200)
		body := f.reserveBody(f.line, "0.001", 1)
		body["expected_document_version"] = json.Number(f.scalar(`SELECT version FROM warehouse.documents WHERE id=$1`, f.document))
		requireStatus(t, f.request(api, "POST", "/api/v1/reservations", body, id()), 201)
		writes++
		if worker.Ready() != nil && worker.Snapshot().Queue.OldestPendingSeconds > 60 {
			unhealthyObserved = true
		}
	}
	pending := f.scalar(`SELECT count(*) FROM platform.outbox WHERE operation_id IN(SELECT id FROM platform.operations WHERE organization_id=$1) AND delivered_at IS NULL`, f.org)
	if pending == "0" || worker.Snapshot().DeliveryErrors == 0 {
		t.Fatalf("outage did not exercise durable failures: pending=%s %+v", pending, worker.Snapshot())
	}
	if outage > 70*time.Second && !unhealthyObserved {
		t.Fatal("long outage did not exercise background unhealthy threshold")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	restored := httptest.NewUnstartedServer(receiver)
	restored.Listener.Close()
	restored.Listener = listener
	restored.TLS = tlsConfig
	restored.StartTLS()
	defer restored.Close()
	recovery := time.Now()
	deadline := time.NewTimer(330 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatalf("queue failed recovery: %+v", worker.Snapshot())
		case <-next.C:
			ready := httptest.NewRecorder()
			api.ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
			requireStatus(t, ready, 200)
			remaining := f.scalar(`SELECT count(*) FROM platform.outbox WHERE operation_id IN(SELECT id FROM platform.operations WHERE organization_id=$1) AND delivered_at IS NULL`, f.org)
			if remaining != "0" {
				continue
			}
			if f.scalar(`SELECT count(*) FROM platform.outbox WHERE operation_id IN(SELECT id FROM platform.operations WHERE organization_id=$1) AND dead_at IS NOT NULL`, f.org) != "0" {
				t.Fatal("dead letters after outage")
			}
			rows, err := f.admin.Query(t.Context(), `SELECT id::text FROM platform.outbox WHERE operation_id IN(SELECT id FROM platform.operations WHERE organization_id=$1)`, f.org)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for rows.Next() {
				var event string
				if err := rows.Scan(&event); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				ok := delivered[event]
				mu.Unlock()
				if !ok {
					t.Errorf("acknowledged event missing at receiver: %s", event)
				}
				count++
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			t.Logf("actual outage=%s recovery=%s writes=%d durable_events=%d delivery_errors=%d background_unhealthy_observed=%v", outage, time.Since(recovery), writes, count, worker.Snapshot().DeliveryErrors, unhealthyObserved)
			return
		}
	}
}
