package retention

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

// Policy is a pilot data-minimization baseline. Dedup/event and effect keys
// are deliberately not expired; external replay can outlive a UI history row.
type Policy struct {
	RawDays, HistoryDays, AuditDays int
	BatchSize                       int
}

func DefaultPolicy() Policy {
	return Policy{RawDays: 7, HistoryDays: 30, AuditDays: 90, BatchSize: 100}
}

func PolicyFromEnv() (Policy, error) {
	p := DefaultPolicy()
	for _, item := range []struct {
		name  string
		value *int
	}{
		{"RELAYDESK_RETENTION_RAW_DAYS", &p.RawDays},
		{"RELAYDESK_RETENTION_HISTORY_DAYS", &p.HistoryDays},
		{"RELAYDESK_RETENTION_AUDIT_DAYS", &p.AuditDays},
		{"RELAYDESK_RETENTION_BATCH_SIZE", &p.BatchSize},
	} {
		if raw := os.Getenv(item.name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return Policy{}, fmt.Errorf("%s must be an integer", item.name)
			}
			*item.value = n
		}
	}
	return p, p.Validate()
}

func (p Policy) Validate() error {
	if p.RawDays < 7 || p.HistoryDays < 30 || p.AuditDays < 90 || p.BatchSize < 1 || p.BatchSize > 500 {
		return errors.New("unsafe retention policy")
	}
	return nil
}

type Result struct {
	RawRedacted, ProviderCallsRemoved, AttemptsRemoved, RunsRemoved, AuditRemoved int
}

func Batch(ctx context.Context, db *store.Store, policy Policy) (Result, error) {
	if db == nil {
		return Result{}, errors.New("retention database unavailable")
	}
	if err := policy.Validate(); err != nil {
		return Result{}, err
	}
	var result Result
	err := db.Pool.QueryRow(ctx, `SELECT * FROM retention_batch($1,$2,$3,$4)`,
		policy.BatchSize, policy.RawDays, policy.HistoryDays, policy.AuditDays).
		Scan(&result.RawRedacted, &result.ProviderCallsRemoved, &result.AttemptsRemoved, &result.RunsRemoved, &result.AuditRemoved)
	return result, err
}
