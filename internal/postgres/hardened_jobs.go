package postgres

import (
	"context"
	"errors"

	"github.com/Shpuntyara/wms-platform/internal/jobs"
	"github.com/jackc/pgx/v5/pgxpool"
)

// HardenedWorker has warehouse-bound maintenance rights and no API identity key.
type HardenedWorker struct {
	pool    *pgxpool.Pool
	profile hardenedProfile
}

func NewHardenedWorker(ctx context.Context, pool *pgxpool.Pool) (*HardenedWorker, error) {
	p, err := loadHardenedProfile(ctx, pool, "worker")
	if err != nil {
		return nil, err
	}
	return &HardenedWorker{pool: pool, profile: p}, nil
}

func (s *HardenedWorker) RuntimeLogin() string { return s.profile.Login }
func (s *HardenedWorker) DeploymentScope() (string, string) {
	return s.profile.OrganizationID, s.profile.WarehouseID
}
func (s *HardenedWorker) ExpireReservations(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT hardened_worker.expire_reservations(100)`).Scan(&count)
	return count, err
}
func (s *HardenedWorker) ClaimEvents(ctx context.Context, limit int) ([]jobs.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text,payload,token::text FROM hardened_worker.claim_outbox($1)`, limit)
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
func (s *HardenedWorker) QueueState(ctx context.Context) (jobs.QueueState, error) {
	var state jobs.QueueState
	err := s.pool.QueryRow(ctx, `SELECT * FROM hardened_worker.queue_state()`).Scan(&state.Pending, &state.Dead, &state.OldestPendingSeconds, &state.ExpiredReservations, &state.OldestExpiredSeconds)
	return state, err
}
func (s *HardenedWorker) CompleteEvent(ctx context.Context, e jobs.Event, success bool) error {
	var accepted bool
	err := s.pool.QueryRow(ctx, `SELECT hardened_worker.complete_outbox($1,$2,$3)`, e.ID, e.Token, success).Scan(&accepted)
	if err != nil {
		return err
	}
	if !accepted {
		return errors.New("outbox lease lost")
	}
	return nil
}
