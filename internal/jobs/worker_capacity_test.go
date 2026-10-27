package jobs

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type capacityStore struct {
	mu        sync.Mutex
	remaining int
	completed atomic.Int64
	expired   atomic.Int64
}

func (s *capacityStore) ExpireReservations(context.Context) (int, error) {
	s.expired.Add(1)
	return 0, nil
}
func (s *capacityStore) ClaimEvent(ctx context.Context) (Event, bool, error) {
	events, err := s.ClaimEvents(ctx, 1)
	if len(events) == 0 {
		return Event{}, false, err
	}
	return events[0], true, err
}
func (s *capacityStore) ClaimEvents(_ context.Context, n int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n = min(n, s.remaining)
	events := make([]Event, n)
	for i := range events {
		events[i] = Event{ID: fmt.Sprint(s.remaining - i)}
	}
	s.remaining -= n
	return events, nil
}
func (s *capacityStore) CompleteEvent(context.Context, Event, bool) error {
	s.completed.Add(1)
	return nil
}

type capacitySender struct{ active, peak atomic.Int64 }

func (s *capacitySender) Send(ctx context.Context, _ Event) error {
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for old := s.peak.Load(); n > old && !s.peak.CompareAndSwap(old, n); old = s.peak.Load() {
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Millisecond):
		return nil
	}
}
func TestRelayDrainsBurstWithoutPerEventPollingDelay(t *testing.T) {
	store := &capacityStore{remaining: 1000}
	sender := &capacitySender{}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	worker := Worker{Store: store, Sender: sender}
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for store.completed.Load() < 1000 {
		select {
		case <-ctx.Done():
			<-done
			t.Fatalf("delivered %d/1000: relay cannot keep up with 500 events/s", store.completed.Load())
		case <-ticker.C:
		}
	}
	cancel()
	<-done
	if n := sender.peak.Load(); n < 2 || n > 16 {
		t.Fatalf("unbounded or missing parallelism: %d", n)
	}
	if store.expired.Load() == 0 {
		t.Fatal("expiry starved")
	}
}

func TestBackgroundReadinessDetectsStallsAndBacklogs(t *testing.T) {
	now := time.Now()
	healthy := Status{RelayAt: now, ExpiryAt: now, MonitorAt: now}
	for _, tc := range []struct {
		name  string
		state Status
		ready bool
	}{
		{"healthy", healthy, true},
		{"not_started", Status{}, false},
		{"relay_stalled", Status{RelayAt: now.Add(-30 * time.Second), ExpiryAt: now, MonitorAt: now}, false},
		{"expiry_stalled", Status{RelayAt: now, ExpiryAt: now.Add(-30 * time.Second), MonitorAt: now}, false},
		{"monitor_stalled", Status{RelayAt: now, ExpiryAt: now, MonitorAt: now.Add(-30 * time.Second)}, false},
		{"dead_event", Status{RelayAt: now, ExpiryAt: now, MonitorAt: now, Queue: QueueState{Dead: 1}}, false},
		{"outbox_age", Status{RelayAt: now, ExpiryAt: now, MonitorAt: now, Queue: QueueState{OldestPendingSeconds: 61}}, false},
		{"expiry_age", Status{RelayAt: now, ExpiryAt: now, MonitorAt: now, Queue: QueueState{OldestExpiredSeconds: 61}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := Worker{state: tc.state}
			if (w.Ready() == nil) != tc.ready {
				t.Fatal(w.Ready())
			}
		})
	}
}

type blockedSender struct{ release chan struct{} }

func (s blockedSender) Send(ctx context.Context, e Event) error {
	if e.ID == "100" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.release:
		}
	}
	return nil
}
func TestSlowDeliveryDoesNotHoldOtherSlots(t *testing.T) {
	store := &capacityStore{remaining: 100}
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	worker := Worker{Store: store, Sender: blockedSender{release: release}}
	go func() { defer close(done); worker.Run(ctx) }()
	defer func() { close(release); cancel(); <-done }()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for store.completed.Load() < 99 {
		select {
		case <-timer.C:
			t.Fatalf("one slow request held other slots: completed=%d", store.completed.Load())
		case <-ticker.C:
		}
	}
}

func TestBackgroundMonitorExposesCountersAndUnhealthyStatus(t *testing.T) {
	worker := Worker{state: Status{Delivered: 7, DeliveryErrors: 2, Queue: QueueState{Dead: 1}}}
	handler := worker.MonitorHandler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/health/background", nil))
	if response.Code != 503 {
		t.Fatal(response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	for _, metric := range []string{"wms_background_ready 0", "wms_outbox_delivered_total 7", "wms_outbox_delivery_errors_total 2", "wms_outbox_dead 1", "wms_background_monitor_timestamp_seconds 0"} {
		if !strings.Contains(response.Body.String(), metric) {
			t.Fatalf("missing %s", metric)
		}
	}
}
