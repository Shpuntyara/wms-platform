package jobs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// MonitorHandler is served on a dedicated loopback listener. Never mount it on
// the public API: queue statistics span all organizations.
func (w *Worker) MonitorHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/background", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Cache-Control", "no-store")
		if w.Ready() != nil {
			rw.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(rw).Encode(w.Snapshot())
	})
	mux.HandleFunc("GET /metrics", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s := w.Snapshot()
		ready := 0
		if w.Ready() == nil {
			ready = 1
		}
		_, _ = fmt.Fprintf(rw, "wms_background_ready %d\nwms_outbox_delivered_total %d\nwms_outbox_delivery_errors_total %d\nwms_outbox_claim_errors_total %d\nwms_reservations_expired_total %d\nwms_reservation_expiry_errors_total %d\nwms_outbox_pending %d\nwms_outbox_dead %d\nwms_outbox_oldest_pending_seconds %g\nwms_reservations_expired_pending %d\nwms_reservations_oldest_expired_seconds %g\nwms_outbox_last_pass_timestamp_seconds %d\nwms_expiry_last_success_timestamp_seconds %d\nwms_background_monitor_timestamp_seconds %d\n", ready, s.Delivered, s.DeliveryErrors, s.RelayErrors, s.Expired, s.ExpiryErrors, s.Queue.Pending, s.Queue.Dead, s.Queue.OldestPendingSeconds, s.Queue.ExpiredReservations, s.Queue.OldestExpiredSeconds, unixOrZero(s.RelayAt), unixOrZero(s.ExpiryAt), unixOrZero(s.MonitorAt))
	})
	return mux
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
