package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/dbgen"
)

type Store struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	p, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return &Store{Pool: p}, nil
}
func (s *Store) Close()                         { s.Pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }
func (s *Store) Ready(ctx context.Context) error {
	var exists bool
	return s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM webhook_events LIMIT 1)`).Scan(&exists)
}

type Binding struct {
	TenantID, ConnectionID, AccountID, Mode string
	Ciphertext, Nonce                       []byte
	KeyVersion                              int
	Generation                              int64
}

var ErrNotFound = errors.New("not found")
var ErrCapacity = errors.New("tenant backlog capacity exhausted")
var ErrIgnoredCapacity = errors.New("tenant ignored event capacity exhausted")

func (s *Store) LookupStripe(ctx context.Context, ref string) (Binding, error) {
	hash := sha256.Sum256([]byte(ref))
	var b Binding
	err := s.Pool.QueryRow(ctx, `SELECT tenant_id,connection_id,account_id,mode,ciphertext,nonce,key_version,generation FROM lookup_stripe_connection($1)`, hash[:]).Scan(&b.TenantID, &b.ConnectionID, &b.AccountID, &b.Mode, &b.Ciphertext, &b.Nonce, &b.KeyVersion, &b.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	return b, err
}
func BeginTenant(ctx context.Context, pool *pgxpool.Pool, tenantID string) (pgx.Tx, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, tenantID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}
func BeginAuthorizedTenant(ctx context.Context, pool *pgxpool.Pool, tenantID string, sessionHash []byte) (pgx.Tx, error) {
	tx, err := BeginTenant(ctx, pool, tenantID)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('app.session_hash',$1,true)`, hex.EncodeToString(sessionHash)); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

type Incoming struct {
	Binding                                                                                    Binding
	EventID, EventType, APIVersion, CorrelationID, Traceparent, Classification, Reason, Source string
	Raw                                                                                        []byte
}

func (s *Store) Accept(ctx context.Context, in Incoming) (bool, error) {
	if in.Source == "" {
		in.Source = "webhook"
	}
	switch in.Source {
	case "webhook", "event_discovery", "known_state", "historical":
	default:
		return false, errors.New("invalid inbox source")
	}
	tx, err := BeginTenant(ctx, s.Pool, in.Binding.TenantID)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The revision is fixed in the same transaction as first acceptance. A replay never reselects it.
	var revisionID *string
	activeRevision, err := dbgen.New(tx).ActiveRevision(ctx, dbgen.ActiveRevisionParams{TenantID: in.Binding.TenantID, ConnectionID: in.Binding.ConnectionID})
	if errors.Is(err, pgx.ErrNoRows) {
		revisionID = nil
	} else if err != nil {
		return false, err
	} else {
		revisionID = &activeRevision
	}
	class, reason := in.Classification, in.Reason
	if class == "supported" && revisionID == nil {
		class, reason = "ignored", "automation_inactive"
	}
	// Serialize both admission budgets. Duplicated provider deliveries still
	// receive success while either budget is full.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,41906))`, in.Binding.TenantID); err != nil {
		return false, err
	}
	var duplicate bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM webhook_events
		 WHERE tenant_id=$1 AND provider='stripe' AND provider_account_id=$2 AND mode=$3 AND event_id=$4)`,
		in.Binding.TenantID, in.Binding.AccountID, in.Binding.Mode, in.EventID).Scan(&duplicate); err != nil {
		return false, err
	}
	if duplicate {
		return false, tx.Commit(ctx)
	}
	if class == "supported" {
		var backlog int64
		if err = tx.QueryRow(ctx, `SELECT
		 (SELECT count(*) FROM planning_jobs WHERE tenant_id=$1 AND status='pending') +
		 (SELECT count(*) FROM business_actions WHERE tenant_id=$1 AND status IN ('pending','queued','running','retry_wait'))`,
			in.Binding.TenantID).Scan(&backlog); err != nil {
			return false, err
		}
		if backlog >= 10000 {
			return false, ErrCapacity
		}
	} else {
		// Ignored IDs are durable tombstones: deleting them would let an old
		// event become actionable after a configuration change. Limit their
		// number and never retain their otherwise unused raw payload.
		var ignored int64
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM webhook_events
		 WHERE tenant_id=$1 AND classification='ignored'`, in.Binding.TenantID).Scan(&ignored); err != nil {
			return false, err
		}
		if ignored >= 10000 {
			return false, ErrIgnoredCapacity
		}
		in.Raw = []byte{}
	}
	var eventRowID string
	err = tx.QueryRow(ctx, `INSERT INTO webhook_events (tenant_id,connection_id,provider,provider_account_id,mode,event_id,event_type,event_api_version,raw_payload,classification,reason,automation_revision_id,correlation_id,connection_generation,source)
      VALUES ($1,$2,'stripe',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
      ON CONFLICT (tenant_id,provider,provider_account_id,mode,event_id) DO NOTHING RETURNING id`, in.Binding.TenantID, in.Binding.ConnectionID, in.Binding.AccountID, in.Binding.Mode, in.EventID, in.EventType, in.APIVersion, in.Raw, class, reason, revisionID, in.CorrelationID, in.Binding.Generation, in.Source).Scan(&eventRowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if class == "supported" {
		var jobID string
		err = tx.QueryRow(ctx, `INSERT INTO planning_jobs (tenant_id,event_row_id,automation_revision_id) VALUES ($1,$2,$3) RETURNING id`, in.Binding.TenantID, eventRowID, revisionID).Scan(&jobID)
		if err != nil {
			return false, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO outbox (tenant_id,job_id,dispatch_generation,correlation_id,traceparent) VALUES ($1,$2,1,$3,$4)`, in.Binding.TenantID, jobID, in.CorrelationID, in.Traceparent)
		if err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

type OutboxRow struct {
	ID, TenantID, JobID, CorrelationID, Traceparent, LeaseToken string
	Generation                                                  int64
}

func (s *Store) ClaimOutbox(ctx context.Context, limit int) ([]OutboxRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,tenant_id,job_id,dispatch_generation,correlation_id,traceparent,lease_token FROM claim_outbox($1,10)`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []OutboxRow
	for rows.Next() {
		var r OutboxRow
		if err = rows.Scan(&r.ID, &r.TenantID, &r.JobID, &r.Generation, &r.CorrelationID, &r.Traceparent, &r.LeaseToken); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (s *Store) MarkPublished(ctx context.Context, r OutboxRow) error {
	tx, err := BeginTenant(ctx, s.Pool, r.TenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE outbox SET published_at=now(),lease_token=NULL,lease_until=NULL WHERE tenant_id=$1 AND id=$2 AND lease_token=$3 AND published_at IS NULL`, r.TenantID, r.ID, r.LeaseToken)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("outbox lease lost")
	}
	return tx.Commit(ctx)
}
func (s *Store) Recover(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT claim_lost_dispatch(100)`).Scan(&n)
	return n, err
}
func (s *Store) Plan(ctx context.Context, tenantID, jobID string, generation int64) error {
	tx, err := BeginTenant(ctx, s.Pool, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	var storedGeneration int64
	var revID string
	err = tx.QueryRow(ctx, `SELECT status,dispatch_generation,automation_revision_id FROM planning_jobs WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantID, jobID).Scan(&status, &storedGeneration, &revID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "pending" || storedGeneration != generation {
		return tx.Commit(ctx)
	}
	var runID string
	err = tx.QueryRow(ctx, `INSERT INTO runs (tenant_id,planning_job_id,automation_revision_id) VALUES ($1,$2,$3) ON CONFLICT (tenant_id,planning_job_id) DO UPDATE SET status=runs.status RETURNING id`, tenantID, jobID, revID).Scan(&runID)
	if err != nil {
		return err
	}
	var template string
	var configRaw []byte
	var activated time.Time
	err = tx.QueryRow(ctx, `SELECT template_key,config,activated_at FROM automation_revisions WHERE tenant_id=$1 AND id=$2`, tenantID, revID).Scan(&template, &configRaw, &activated)
	if err != nil {
		return err
	}
	if template == business.Template {
		var cfg business.Config
		if json.Unmarshal(configRaw, &cfg) != nil || cfg.Validate() != nil || cfg.StripeConnectionID == "" {
			_, err = tx.Exec(ctx, `UPDATE runs SET status='blocked_config' WHERE tenant_id=$1 AND id=$2`, tenantID, runID)
		} else {
			err = s.planBusiness(ctx, tx, tenantID, jobID, runID, revID, cfg, activated)
		}
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE planning_jobs SET status='planned',planned_at=now() WHERE tenant_id=$1 AND id=$2`, tenantID, jobID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) planBusiness(ctx context.Context, tx pgx.Tx, tenantID, jobID, runID, revID string, cfg business.Config, activated time.Time) error {
	var raw []byte
	var account, mode, eventType, eventID, eventRowID, eventConnectionID, source string
	err := tx.QueryRow(ctx, `SELECT e.raw_payload,e.provider_account_id,e.mode,e.event_type,e.event_id,e.id,e.connection_id,e.source
      FROM planning_jobs j JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
	      WHERE j.tenant_id=$1 AND j.id=$2`, tenantID, jobID).Scan(&raw, &account, &mode, &eventType, &eventID, &eventRowID, &eventConnectionID, &source)
	if err != nil {
		return err
	}
	signal, err := business.ParseSignal(raw)
	if err != nil || signal.Type != eventType || signal.EventID != eventID || eventConnectionID != cfg.StripeConnectionID {
		_, err = tx.Exec(ctx, `UPDATE runs SET status='needs_attention' WHERE tenant_id=$1 AND id=$2`, tenantID, runID)
		return err
	}
	if signal.Created.Before(activated) || signal.Created.Before(cfg.ActivationTime) {
		_, err = tx.Exec(ctx, `UPDATE runs SET status='skipped',finished_at=now() WHERE tenant_id=$1 AND id=$2`, tenantID, runID)
		return err
	}
	_, err = business.CustomerKey(account, mode, signal.CustomerID)
	if err != nil {
		return err
	}
	crmKey := "crm|" + tenantID + "|" + revID + "|" + eventRowID
	var crmID string
	// A pending or queued action has not taken its provider snapshot yet. Link
	// this event's run to that durable obligation, but never reuse an action
	// that has started: its snapshot could predate the newly accepted event.
	// The aggregate lock serializes concurrent planners before they inspect
	// or create the candidate; the action row lock serializes with the claim.
	aggregate := tenantID + "|" + revID + "|" + cfg.StripeConnectionID + "|" + signal.CustomerID
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,41911))`, aggregate); err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT id FROM business_actions
 WHERE tenant_id=$1 AND automation_revision_id=$2 AND stripe_connection_id=$3
 AND customer_id=$4 AND kind='crm_projection' AND status IN ('pending','queued')
 ORDER BY created_at,id LIMIT 1 FOR UPDATE`, tenantID, revID, cfg.StripeConnectionID, signal.CustomerID).Scan(&crmID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `INSERT INTO business_actions(tenant_id,automation_revision_id,kind,effect_key,stripe_connection_id,customer_id)
       VALUES($1,$2,'crm_projection',$3,$4,$5) ON CONFLICT (tenant_id,effect_key)
       DO UPDATE SET updated_at=business_actions.updated_at RETURNING id`, tenantID, revID, crmKey, cfg.StripeConnectionID, signal.CustomerID).Scan(&crmID)
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO business_run_actions(tenant_id,run_id,action_id,kind) VALUES($1,$2,$3,'crm_projection') ON CONFLICT DO NOTHING`, tenantID, runID, crmID)
	if err != nil {
		return err
	}
	if signal.Type == "invoice.paid" && source == "webhook" {
		var workspace string
		err = tx.QueryRow(ctx, `SELECT provider_account_id FROM connections WHERE tenant_id=$1 AND id=$2 AND provider='slack'`, tenantID, cfg.SlackConnectionID).Scan(&workspace)
		if err != nil {
			return err
		}
		effect := business.InvoiceEffectKey(tenantID, account, mode, signal.InvoiceID, workspace, cfg.SlackChannelID)
		var slackID string
		err = tx.QueryRow(ctx, `INSERT INTO business_actions(tenant_id,automation_revision_id,kind,effect_key,stripe_connection_id,customer_id,invoice_id,depends_on)
        VALUES($1,$2,'slack_invoice_paid',$3,$4,$5,$6,$7) ON CONFLICT (tenant_id,effect_key)
        DO UPDATE SET updated_at=business_actions.updated_at RETURNING id`, tenantID, revID, effect, cfg.StripeConnectionID, signal.CustomerID, signal.InvoiceID, crmID).Scan(&slackID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO business_run_actions(tenant_id,run_id,action_id,kind) VALUES($1,$2,$3,'slack_invoice_paid') ON CONFLICT DO NOTHING`, tenantID, runID, slackID)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE runs SET status='running' WHERE tenant_id=$1 AND id=$2`, tenantID, runID)
	return err
}
func (s *Store) WaitForRun(ctx context.Context, tenantID, jobID string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		tx, err := BeginTenant(ctx, s.Pool, tenantID)
		if err == nil {
			var found bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE tenant_id=$1 AND planning_job_id=$2)`, tenantID, jobID).Scan(&found)
			_ = tx.Rollback(ctx)
			if err == nil && found {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("run not planned before deadline")
		case <-tick.C:
		}
	}
}
