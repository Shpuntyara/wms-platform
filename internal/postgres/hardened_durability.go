package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func validateDurabilitySettings(fsync, commit, fullPages, checksums, ignoreChecksums, zeroPages, ignoreInvalid string) error {
	for _, setting := range []struct{ name, value string }{
		{"fsync", fsync}, {"full_page_writes", fullPages}, {"data_checksums", checksums},
	} {
		if setting.value != "on" {
			return fmt.Errorf("unsafe PostgreSQL durability: %s must be on", setting.name)
		}
	}
	if commit != "on" && commit != "remote_apply" {
		return errors.New("unsafe PostgreSQL durability: synchronous_commit must be on or remote_apply")
	}
	for _, setting := range []struct{ name, value string }{
		{"ignore_checksum_failure", ignoreChecksums}, {"zero_damaged_pages", zeroPages}, {"ignore_invalid_pages", ignoreInvalid},
	} {
		if setting.value != "off" {
			return fmt.Errorf("unsafe PostgreSQL durability: %s must be off", setting.name)
		}
	}
	return nil
}

type durabilityQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func requireDurablePostgres(ctx context.Context, query durabilityQuerier) error {
	if query == nil {
		return errors.New("database pool required")
	}
	var fsync, commit, fullPages, checksums, ignoreChecksums, zeroPages, ignoreInvalid string
	if err := query.QueryRow(ctx, `SELECT current_setting('fsync'),current_setting('synchronous_commit'),
	 current_setting('full_page_writes'),current_setting('data_checksums'),current_setting('ignore_checksum_failure'),
	 current_setting('zero_damaged_pages'),current_setting('ignore_invalid_pages')`).Scan(&fsync, &commit, &fullPages, &checksums, &ignoreChecksums, &zeroPages, &ignoreInvalid); err != nil {
		return fmt.Errorf("PostgreSQL durability preflight: %w", err)
	}
	return validateDurabilitySettings(fsync, commit, fullPages, checksums, ignoreChecksums, zeroPages, ignoreInvalid)
}

// beginDurableTransaction strengthens a connection's session default before any
// application statement. A stronger remote_apply deployment keeps that mode.
// The caller owns commit and rollback after this function succeeds.
func beginDurableTransaction(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	if pool == nil {
		return nil, errors.New("database pool required")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `SELECT set_config('synchronous_commit',
	 CASE current_setting('synchronous_commit') WHEN 'remote_apply' THEN 'remote_apply' ELSE 'on' END,true)`)
	if err == nil {
		err = requireDurablePostgres(ctx, tx)
	}
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
		return nil, err
	}
	return tx, nil
}
