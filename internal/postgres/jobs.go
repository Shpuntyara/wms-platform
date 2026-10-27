package postgres

import (
	"context"
	"errors"
	"github.com/Shpuntyara/wms-platform/internal/jobs"
	"github.com/jackc/pgx/v5"
)

func (s *Store) ExpireReservations(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT inventory.expire_reservations(100)`).Scan(&count)
	return count, err
}
func (s *Store) ClaimEvent(ctx context.Context) (jobs.Event, bool, error) {
	var e jobs.Event
	var attempt int
	err := s.pool.QueryRow(ctx, `SELECT id::text,payload,token::text,attempt FROM platform.claim_outbox(1)`).Scan(&e.ID, &e.Payload, &e.Token, &attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, false, nil
	}
	return e, err == nil, err
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
