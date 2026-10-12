package identity

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrInvalidReconciliation = errors.New("invalid reconciliation request")

type ReconcileCheckpoint struct {
	ConnectionID    string     `json:"connection_id"`
	Kind            string     `json:"kind"`
	WindowStart     time.Time  `json:"window_start"`
	WindowEnd       time.Time  `json:"window_end"`
	LastCompleteAt  *time.Time `json:"last_complete_at,omitempty"`
	LastAttemptAt   *time.Time `json:"last_attempt_at,omitempty"`
	GapCode         string     `json:"gap_code,omitempty"`
	ScannedCount    int64      `json:"scanned_count"`
	DiscoveredCount int64      `json:"discovered_count"`
}

type ReconcileRequest struct {
	ID, ConnectionID, Status, GapCode string
	FromAt, ToAt                      time.Time
	MaxEvents, Scanned, Discovered    int
	CreatedAt                         time.Time
	FinishedAt                        *time.Time
	ActorID, Reason                   string
}

type OperationalStatus struct {
	Checkpoints          []ReconcileCheckpoint `json:"checkpoints"`
	Requests             []ReconcileRequest    `json:"requests"`
	OldestOutboxSeconds  int64                 `json:"oldest_outbox_seconds"`
	OldestDueSeconds     int64                 `json:"oldest_due_seconds"`
	OldestUnknownSeconds int64                 `json:"oldest_unknown_seconds"`
	OldestDeadSeconds    int64                 `json:"oldest_dead_seconds"`
	UnknownActions       int64                 `json:"unknown_actions"`
	DeadActions          int64                 `json:"dead_actions"`
	OutboundEnabled      bool                  `json:"outbound_enabled"`
	NextRequestCursor    string                `json:"next_request_cursor,omitempty"`
}

func (s *Service) ReconciliationStatus(ctx context.Context, p Principal, tenant string) (OperationalStatus, error) {
	return s.ReconciliationStatusPage(ctx, p, tenant, "")
}

func (s *Service) OutboundExecutionEnabled(ctx context.Context, p Principal, tenant string) (bool, error) {
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "read")
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var enabled bool
	if err = tx.QueryRow(ctx, `SELECT read_outbound_gate($1,$2)`, p.TokenHash, tenant).Scan(&enabled); err != nil {
		return false, err
	}
	return enabled, tx.Commit(ctx)
}

func (s *Service) EligibleHistoricalConnectionIDs(ctx context.Context, p Principal, tenant string) (map[string]bool, error) {
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "read")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT c.id FROM connections c WHERE c.tenant_id=$1 AND c.provider='stripe'
 AND c.mode='test' AND c.status='active' AND c.health_status='verified'
 AND c.execution_environment IN ('simulation','test')
 AND EXISTS (SELECT 1 FROM automation_revisions ar WHERE ar.tenant_id=c.tenant_id
  AND ar.connection_id=c.id AND ar.status='active' AND ar.template_key='subscription_billing_sync.v1')
 ORDER BY c.created_at DESC LIMIT 100`, tenant)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return ids, tx.Commit(ctx)
}

func (s *Service) ReconciliationStatusPage(ctx context.Context, p Principal, tenant, cursor string) (OperationalStatus, error) {
	c, err := parseCursor(cursor)
	if err != nil {
		return OperationalStatus{}, err
	}
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "read")
	if err != nil {
		return OperationalStatus{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var out OperationalStatus
	rows, err := tx.Query(ctx, `SELECT stripe_connection_id,kind,window_start,window_end,last_complete_at,last_attempt_at,
 coalesce(gap_code,''),scanned_count,discovered_count FROM reconcile_checkpoints
 WHERE tenant_id=$1 ORDER BY kind,stripe_connection_id LIMIT 100`, tenant)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var item ReconcileCheckpoint
		if err = rows.Scan(&item.ConnectionID, &item.Kind, &item.WindowStart, &item.WindowEnd,
			&item.LastCompleteAt, &item.LastAttemptAt, &item.GapCode, &item.ScannedCount, &item.DiscoveredCount); err != nil {
			rows.Close()
			return out, err
		}
		out.Checkpoints = append(out.Checkpoints, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	var beforeAt *time.Time
	var beforeID *string
	if c != nil {
		beforeAt, beforeID = &c.CreatedAt, &c.ID
	}
	rows, err = tx.Query(ctx, `SELECT id,stripe_connection_id,status,coalesce(gap_code,''),from_at,to_at,
 max_events,scanned_count,discovered_count,created_at,finished_at,actor_user_id::text,reason FROM reconcile_requests
 WHERE tenant_id=$1 AND ($2::timestamptz IS NULL OR (created_at,id)<($2,$3::uuid))
 ORDER BY created_at DESC,id DESC LIMIT 21`, tenant, beforeAt, beforeID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var item ReconcileRequest
		if err = rows.Scan(&item.ID, &item.ConnectionID, &item.Status, &item.GapCode,
			&item.FromAt, &item.ToAt, &item.MaxEvents, &item.Scanned, &item.Discovered, &item.CreatedAt,
			&item.FinishedAt, &item.ActorID, &item.Reason); err != nil {
			rows.Close()
			return out, err
		}
		out.Requests = append(out.Requests, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Requests) > 20 {
		out.Requests = out.Requests[:20]
		last := out.Requests[len(out.Requests)-1]
		out.NextRequestCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	if err = tx.QueryRow(ctx, `SELECT read_outbound_gate($1,$2)`, p.TokenHash, tenant).Scan(&out.OutboundEnabled); err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT
 (SELECT coalesce(extract(epoch from now()-min(created_at))::bigint,0) FROM business_action_outbox
  WHERE tenant_id=$1 AND published_at IS NULL AND superseded_at IS NULL),
 (SELECT coalesce(extract(epoch from now()-min(next_attempt_at))::bigint,0) FROM business_actions
  WHERE tenant_id=$1 AND status='retry_wait' AND next_attempt_at<=now()),
 (SELECT coalesce(extract(epoch from now()-min(updated_at))::bigint,0) FROM business_actions
  WHERE tenant_id=$1 AND status='unknown'),
 (SELECT coalesce(extract(epoch from now()-min(updated_at))::bigint,0) FROM business_actions
  WHERE tenant_id=$1 AND status='dead'),
 (SELECT count(*) FROM business_actions WHERE tenant_id=$1 AND status='unknown'),
 (SELECT count(*) FROM business_actions WHERE tenant_id=$1 AND status='dead')`, tenant).
		Scan(&out.OldestOutboxSeconds, &out.OldestDueSeconds, &out.OldestUnknownSeconds,
			&out.OldestDeadSeconds, &out.UnknownActions, &out.DeadActions)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

type HistoricalReconcileInput struct {
	ConnectionID string    `json:"connection_id"`
	FromAt       time.Time `json:"from_at"`
	ToAt         time.Time `json:"to_at"`
	MaxEvents    int       `json:"max_events"`
	Reason       string    `json:"reason"`
}

func (i HistoricalReconcileInput) Validate() error {
	reason := strings.TrimSpace(i.Reason)
	if _, err := uuid.Parse(i.ConnectionID); err != nil || i.FromAt.IsZero() || i.ToAt.IsZero() ||
		!i.FromAt.Before(i.ToAt) || i.FromAt.Before(time.Now().Add(-29*24*time.Hour)) ||
		i.ToAt.After(time.Now().Add(-2*time.Minute)) || i.MaxEvents < 1 || i.MaxEvents > 500 ||
		utf8.RuneCountInString(reason) < 12 || utf8.RuneCountInString(reason) > 500 {
		return ErrInvalidReconciliation
	}
	return nil
}

func (s *Service) RequestHistoricalReconciliation(ctx context.Context, p Principal, tenant string,
	input HistoricalReconcileInput, correlation string) (string, error) {
	if err := input.Validate(); err != nil {
		return "", err
	}
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "operate")
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var valid bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM connections c JOIN automation_revisions r
 ON r.tenant_id=c.tenant_id AND r.connection_id=c.id
 WHERE c.tenant_id=$1 AND c.id=$2 AND c.provider='stripe' AND c.mode='test'
 AND c.status='active' AND c.health_status='verified'
 AND c.execution_environment IN ('simulation','test')
 AND r.status='active' AND r.template_key='subscription_billing_sync.v1')`, tenant, input.ConnectionID).Scan(&valid)
	if err != nil {
		return "", err
	}
	if !valid {
		return "", ErrInvalidReconciliation
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	var id string
	err = s.appDB.Pool.QueryRow(ctx, `SELECT request_historical_reconcile($1,$2,$3,$4,$5,$6,$7,$8)`,
		p.TokenHash, tenant, input.ConnectionID, input.FromAt, input.ToAt, input.MaxEvents, strings.TrimSpace(input.Reason), correlation).Scan(&id)
	return id, err
}
