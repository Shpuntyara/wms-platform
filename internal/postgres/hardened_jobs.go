package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/jobs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// HardenedWorker maintains all warehouses in one dedicated client installation.
// It has no API identity signing key or user-facing command rights.
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

func (s *HardenedWorker) RuntimeLogin() string   { return s.profile.Login }
func (s *HardenedWorker) InstallationID() string { return s.profile.InstallationID }
func (s *HardenedWorker) transaction(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := beginDurableTransaction(ctx, s.pool)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT hardened_worker.profile()`).Scan(&raw); err != nil {
		return err
	}
	var current hardenedProfile
	if err := json.Unmarshal(raw, &current); err != nil {
		return err
	}
	if current.Profile != "onprem-v1" || current.InstallationID != s.profile.InstallationID || current.Login != s.profile.Login {
		return errors.New("on-premise worker installation identity changed")
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *HardenedWorker) ExpireReservations(ctx context.Context) (int, error) {
	var count int
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT hardened_worker.expire_reservations(100)`).Scan(&count)
	})
	if err != nil {
		return 0, err
	}
	return count, err
}
func (s *HardenedWorker) ClaimEvents(ctx context.Context, limit int) ([]jobs.Event, error) {
	var events []jobs.Event
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text,payload,token::text FROM hardened_worker.claim_outbox($1)`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var event jobs.Event
			if err := rows.Scan(&event.ID, &event.Payload, &event.Token); err != nil {
				return err
			}
			events = append(events, event)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}
func (s *HardenedWorker) QueueState(ctx context.Context) (jobs.QueueState, error) {
	var state jobs.QueueState
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT * FROM hardened_worker.queue_state()`).Scan(&state.Pending, &state.Dead, &state.OldestPendingSeconds, &state.ExpiredReservations, &state.OldestExpiredSeconds)
	})
	if err != nil {
		return jobs.QueueState{}, err
	}
	return state, err
}
func (s *HardenedWorker) CompleteEvent(ctx context.Context, e jobs.Event, success bool) error {
	var accepted bool
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT hardened_worker.complete_outbox($1,$2,$3)`, e.ID, e.Token, success).Scan(&accepted)
	})
	if err != nil {
		return err
	}
	if !accepted {
		return errors.New("outbox lease lost")
	}
	return nil
}
