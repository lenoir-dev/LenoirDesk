package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type BusinessOutboxRow struct {
	ID, TenantID, ActionID, LeaseToken string
	Generation                         int64
}
type BusinessHealth struct {
	DueActions, OldestDueSeconds, UnpublishedOutbox, OldestUnpublishedSeconds int64
	ActiveLeases, ExpiredLeases, StalledActions, DeadActions, UnknownActions  int64
	OldestUnknownSeconds, Retries24h                                          int64
}

type OperationalMetrics struct {
	OldestDeadSeconds, OldestPendingJobSeconds, CoverageGaps, OldestReconcileLagSeconds int64
	UnhealthyConnections, StorageBytes, SucceededActions24h, FailedActions24h           int64
}

func (s *Store) OperationalMetrics(ctx context.Context) (OperationalMetrics, error) {
	var m OperationalMetrics
	err := s.Pool.QueryRow(ctx, `SELECT * FROM operational_metrics()`).Scan(
		&m.OldestDeadSeconds, &m.OldestPendingJobSeconds, &m.CoverageGaps,
		&m.OldestReconcileLagSeconds, &m.UnhealthyConnections, &m.StorageBytes,
		&m.SucceededActions24h, &m.FailedActions24h)
	return m, err
}

func (s *Store) BusinessHealth(ctx context.Context) (BusinessHealth, error) {
	var h BusinessHealth
	err := s.Pool.QueryRow(ctx, `SELECT * FROM business_recovery_health()`).Scan(
		&h.DueActions, &h.OldestDueSeconds, &h.UnpublishedOutbox, &h.OldestUnpublishedSeconds,
		&h.ActiveLeases, &h.ExpiredLeases, &h.StalledActions, &h.DeadActions, &h.UnknownActions,
		&h.OldestUnknownSeconds, &h.Retries24h)
	return h, err
}

func (s *Store) QueueBusiness(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT queue_business_actions(16)`).Scan(&n)
	return n, err
}

func (s *Store) RepairBusiness(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT repair_business_actions(100)`).Scan(&n)
	return n, err
}

func (s *Store) ClaimBusinessOutbox(ctx context.Context) ([]BusinessOutboxRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,tenant_id,action_id,dispatch_generation,lease_token FROM claim_business_outbox(16)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BusinessOutboxRow
	for rows.Next() {
		var r BusinessOutboxRow
		if err = rows.Scan(&r.ID, &r.TenantID, &r.ActionID, &r.Generation, &r.LeaseToken); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) MarkBusinessPublished(ctx context.Context, r BusinessOutboxRow) error {
	tx, err := BeginTenant(ctx, s.Pool, r.TenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE business_action_outbox SET published_at=now(),lease_token=NULL,lease_until=NULL
 WHERE tenant_id=$1 AND id=$2 AND lease_token=$3 AND published_at IS NULL AND superseded_at IS NULL`, r.TenantID, r.ID, r.LeaseToken)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("business outbox lease lost")
	}
	return tx.Commit(ctx)
}

// PointerState reports whether the exact claimed generation still owns work.
// Only that case leaves a broker pointer unacknowledged for lease repair.
func (s *Store) PointerState(ctx context.Context, tenantID, actionID, token string, generation int64) (bool, error) {
	tx, err := BeginTenant(ctx, s.Pool, tenantID)
	if err != nil {
		return true, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var same bool
	err = tx.QueryRow(ctx, `SELECT status='running' AND lease_token=$3 AND generation=$4
 FROM business_actions WHERE tenant_id=$1 AND id=$2`, tenantID, actionID, token, generation).Scan(&same)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	return same, tx.Commit(ctx)
}
