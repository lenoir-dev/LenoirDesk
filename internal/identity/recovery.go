package identity

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

var ErrInvalidRecovery = errors.New("invalid recovery decision")
var ErrRecoveryConflict = errors.New("recovery conflict")

type RecoveryRequest struct {
	Decision                 string `json:"decision"`
	Reason                   string `json:"reason"`
	ExpectedGeneration       int64  `json:"expected_generation"`
	IdempotencyKey           string `json:"idempotency_key,omitempty"`
	ExternalChannel          string `json:"external_channel,omitempty"`
	ExternalTS               string `json:"external_ts,omitempty"`
	AcknowledgeDuplicateRisk bool   `json:"acknowledge_duplicate_risk,omitempty"`
}

type RecoveryResult struct {
	DecisionID string `json:"decision_id"`
	Status     string `json:"status"`
	Generation int64  `json:"generation"`
}

func (s *Service) RecoverBusinessAction(ctx context.Context, p Principal, tenantID, actionID string,
	req RecoveryRequest, correlation string) (RecoveryResult, error) {
	if _, err := uuid.Parse(actionID); err != nil {
		return RecoveryResult{}, store.ErrNotFound
	}
	if req.ExpectedGeneration < 1 || len(strings.TrimSpace(req.Reason)) < 1 || len(req.Reason) > 1000 ||
		len(req.IdempotencyKey) > 128 {
		return RecoveryResult{}, ErrInvalidRecovery
	}
	switch req.Decision {
	case "manual_retry", "confirm_unknown", "leave_unknown", "risk_retry":
	default:
		return RecoveryResult{}, ErrInvalidRecovery
	}
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenantID, "operate")
	if err != nil {
		return RecoveryResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize equal idempotency keys before the function checks its unique row.
	// The action row lock then serializes different decisions and generations.
	if req.IdempotencyKey != "" {
		_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, tenantID+"|"+actionID+"|"+req.IdempotencyKey)
		if err != nil {
			return RecoveryResult{}, err
		}
	}
	var key, channel, ts any
	if req.IdempotencyKey != "" {
		key = req.IdempotencyKey
	}
	if req.ExternalChannel != "" {
		channel = req.ExternalChannel
	}
	if req.ExternalTS != "" {
		ts = req.ExternalTS
	}
	var result RecoveryResult
	err = tx.QueryRow(ctx, `SELECT decision_id,action_status,action_generation FROM decide_business_action(
	 $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, p.TokenHash, tenantID, actionID, req.Decision,
		req.Reason, req.ExpectedGeneration, key, channel, ts, req.AcknowledgeDuplicateRisk, correlation).
		Scan(&result.DecisionID, &result.Status, &result.Generation)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) {
			switch pg.Code {
			case "P0002":
				return RecoveryResult{}, store.ErrNotFound
			case "42501":
				return RecoveryResult{}, ErrForbidden
			case "40001", "23505":
				return RecoveryResult{}, ErrRecoveryConflict
			case "22023":
				return RecoveryResult{}, ErrInvalidRecovery
			}
		}
		return RecoveryResult{}, err
	}
	return result, tx.Commit(ctx)
}
