package postgres

import (
	"context"
	"errors"
	"github.com/Shpuntyara/wms-platform/internal/jobs"
)

func (s *Store) ExpireReservations(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT inventory.expire_reservations(100)`).Scan(&count)
	return count, err
}
func (s *Store) ClaimEvents(ctx context.Context, limit int) ([]jobs.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text,payload,token::text FROM platform.claim_outbox($1)`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []jobs.Event
	for rows.Next() {
		var event jobs.Event
		if err := rows.Scan(&event.ID, &event.Payload, &event.Token); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
func (s *Store) QueueState(ctx context.Context) (jobs.QueueState, error) {
	var state jobs.QueueState
	err := s.pool.QueryRow(ctx, `SELECT pending,dead,coalesce(extract(epoch FROM clock_timestamp()-oldest_pending_at),0),expired_reservations,coalesce(extract(epoch FROM clock_timestamp()-oldest_expired_at),0) FROM contracts_v1.background_health`).Scan(&state.Pending, &state.Dead, &state.OldestPendingSeconds, &state.ExpiredReservations, &state.OldestExpiredSeconds)
	return state, err
}
func (s *Store) CompleteEvent(ctx context.Context, e jobs.Event, success bool) error {
	var accepted bool
	err := s.pool.QueryRow(ctx, `SELECT platform.complete_outbox($1,$2,$3)`, e.ID, e.Token, success).Scan(&accepted)
	if err != nil {
		return err
	}
	if !accepted {
		return errors.New("outbox lease lost")
	}
	return nil
}
