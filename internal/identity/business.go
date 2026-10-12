package identity

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

type BillingAutomation struct {
	RevisionID string          `json:"revision_id"`
	Revision   int             `json:"revision"`
	Status     string          `json:"status"`
	Paused     bool            `json:"paused"`
	Config     business.Config `json:"config"`
}

func (s *Service) ConfigureBilling(ctx context.Context, p Principal, tenant string, cfg business.Config, correlation string) (string, error) {
	cfg.ActivationTime = cfg.ActivationTime.UTC()
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	for _, id := range []string{cfg.StripeConnectionID, cfg.HubSpotConnectionID, cfg.SlackConnectionID} {
		if _, err := uuid.Parse(id); err != nil {
			return "", errors.New("connection IDs must be UUIDs")
		}
	}
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "operate")
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	raw, _ := json.Marshal(cfg)
	var revisionID string
	err = tx.QueryRow(ctx, `SELECT configure_billing_sync($1,$2,$3,$4,$5,$6)`, p.TokenHash, tenant, cfg.StripeConnectionID, raw, cfg.ActivationTime, correlation).Scan(&revisionID)
	if err != nil {
		return "", err
	}
	return revisionID, tx.Commit(ctx)
}
func (s *Service) Billing(ctx context.Context, p Principal, tenant, stripeID string) (BillingAutomation, error) {
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "read")
	if err != nil {
		return BillingAutomation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var result BillingAutomation
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT r.id,r.revision,r.status,r.config,coalesce(g.paused,false)
 FROM automation_revisions r LEFT JOIN automation_gates g ON g.tenant_id=r.tenant_id AND g.stripe_connection_id=r.connection_id
 WHERE r.tenant_id=$1 AND r.connection_id=$2 AND r.template_key=$3 AND r.status='active'`, tenant, stripeID, business.Template).Scan(&result.RevisionID, &result.Revision, &result.Status, &raw, &result.Paused)
	if errors.Is(err, pgx.ErrNoRows) {
		return BillingAutomation{}, store.ErrNotFound
	}
	if err != nil {
		return BillingAutomation{}, err
	}
	if err = json.Unmarshal(raw, &result.Config); err != nil {
		return BillingAutomation{}, err
	}
	return result, tx.Commit(ctx)
}
func (s *Service) PauseBilling(ctx context.Context, p Principal, tenant, stripeID string, paused bool, correlation string) error {
	if _, err := uuid.Parse(stripeID); err != nil {
		return store.ErrNotFound
	}
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "operate")
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_billing_pause($1,$2,$3,$4,$5)`, p.TokenHash, tenant, stripeID, paused, correlation)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Service) LinkContact(ctx context.Context, p Principal, tenant, hubspot, key, contact, correlation string) error {
	return s.LinkContactReviewed(ctx, p, tenant, hubspot, key, contact, "API explicit Contact bind", correlation)
}

func (s *Service) LinkContactReviewed(ctx context.Context, p Principal, tenant, hubspot, key, contact, reason, correlation string) error {
	if _, err := uuid.Parse(hubspot); err != nil {
		return store.ErrNotFound
	}
	if len(reason) < 3 || len(reason) > 1000 || len(correlation) > 128 {
		return ErrContactConflict
	}
	preview, err := s.PreviewContact(ctx, p, tenant, hubspot, key, contact)
	if err != nil {
		return err
	}
	if preview.ID != contact || preview.IdentityStatus == "conflict" {
		return ErrContactConflict
	}
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "operate")
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT link_billing_contact_reviewed($1,$2,$3,$4,$5,$6,$7)`, p.TokenHash, tenant, hubspot, key, contact, reason, correlation)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type RunAction struct {
	ID                 string           `json:"id"`
	Kind               string           `json:"kind"`
	Status             string           `json:"status"`
	Attempts           int              `json:"attempts"`
	ErrorClass         string           `json:"error_class,omitempty"`
	NextAttemptAt      *time.Time       `json:"next_attempt_at,omitempty"`
	ContactID          string           `json:"contact_id,omitempty"`
	SlackChannel       string           `json:"slack_channel,omitempty"`
	SlackTS            string           `json:"slack_ts,omitempty"`
	Generation         int64            `json:"generation"`
	FirstFailedAt      *time.Time       `json:"first_failed_at,omitempty"`
	LastFailedAt       *time.Time       `json:"last_failed_at,omitempty"`
	ConfirmationSource string           `json:"confirmation_source,omitempty"`
	ResolutionReason   string           `json:"resolution_reason,omitempty"`
	DependsOn          string           `json:"depends_on,omitempty"`
	Timeline           []ActionAttempt  `json:"timeline"`
	Decisions          []ActionDecision `json:"decisions"`
}
type ActionAttempt struct {
	ID               string         `json:"id"`
	Number           int            `json:"number"`
	Outcome          string         `json:"outcome,omitempty"`
	StartedAt        time.Time      `json:"started_at"`
	RequestStartedAt *time.Time     `json:"request_started_at,omitempty"`
	FinishedAt       *time.Time     `json:"finished_at,omitempty"`
	Calls            []ProviderCall `json:"calls"`
}
type ProviderCall struct {
	Provider  string    `json:"provider"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    *int      `json:"status,omitempty"`
	Outcome   string    `json:"outcome,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
}
type ActionDecision struct {
	Decision   string    `json:"decision"`
	Reason     string    `json:"reason"`
	FromStatus string    `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	CreatedAt  time.Time `json:"created_at"`
}
type RunDetail struct {
	ID          string          `json:"id"`
	Status      string          `json:"status"`
	EventID     string          `json:"event_id"`
	EventType   string          `json:"event_type"`
	EventSource string          `json:"event_source"`
	AcceptedAt  time.Time       `json:"accepted_at"`
	RevisionID  string          `json:"revision_id"`
	Revision    int             `json:"revision"`
	StripeMode  string          `json:"stripe_mode"`
	CustomerID  string          `json:"customer_id,omitempty"`
	InvoiceID   string          `json:"invoice_id,omitempty"`
	CustomerKey string          `json:"customer_key,omitempty"`
	Config      business.Config `json:"config"`
	Actions     []RunAction     `json:"actions"`
	Audit       []AuditEntry    `json:"audit"`
	CreatedAt   time.Time       `json:"created_at"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
}

func (s *Service) BusinessRun(ctx context.Context, p Principal, tenant, runID string) (RunDetail, error) {
	if _, err := uuid.Parse(runID); err != nil {
		return RunDetail{}, store.ErrNotFound
	}
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "read")
	if err != nil {
		return RunDetail{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var d RunDetail
	var rawConfig []byte
	var stripeAccount, stripeMode string
	err = tx.QueryRow(ctx, `SELECT r.id,r.status,e.event_id,e.event_type,e.source,e.received_at,
 r.automation_revision_id,ar.revision,r.created_at,r.finished_at,
 ar.config,c.provider_account_id,c.mode
 FROM runs r JOIN planning_jobs j ON j.tenant_id=r.tenant_id AND j.id=r.planning_job_id
 JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
 JOIN automation_revisions ar ON ar.tenant_id=r.tenant_id AND ar.id=r.automation_revision_id
 JOIN connections c ON c.tenant_id=ar.tenant_id AND c.id=ar.connection_id
 WHERE r.tenant_id=$1 AND r.id=$2`, tenant, runID).Scan(&d.ID, &d.Status, &d.EventID, &d.EventType, &d.EventSource, &d.AcceptedAt,
		&d.RevisionID, &d.Revision, &d.CreatedAt, &d.FinishedAt,
		&rawConfig, &stripeAccount, &stripeMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunDetail{}, store.ErrNotFound
	}
	if err != nil {
		return RunDetail{}, err
	}
	if err = json.Unmarshal(rawConfig, &d.Config); err != nil {
		return RunDetail{}, err
	}
	d.StripeMode = stripeMode
	rows, err := tx.Query(ctx, `SELECT a.id,a.kind,a.status,a.attempts,coalesce(a.error_class,''),a.next_attempt_at,
 coalesce(a.contact_id,''),coalesce(a.slack_channel,''),coalesce(a.slack_ts,''),a.customer_id,a.invoice_id,
 a.generation,a.first_failed_at,a.last_failed_at,coalesce(a.confirmation_source,''),coalesce(a.resolution_reason,''),coalesce(a.depends_on::text,'')
 FROM business_run_actions l JOIN business_actions a ON a.tenant_id=l.tenant_id AND a.id=l.action_id
 WHERE l.tenant_id=$1 AND l.run_id=$2 ORDER BY a.kind`, tenant, runID)
	if err != nil {
		return RunDetail{}, err
	}
	for rows.Next() {
		var a RunAction
		var customer, invoice string
		if err = rows.Scan(&a.ID, &a.Kind, &a.Status, &a.Attempts, &a.ErrorClass, &a.NextAttemptAt, &a.ContactID, &a.SlackChannel, &a.SlackTS, &customer, &invoice,
			&a.Generation, &a.FirstFailedAt, &a.LastFailedAt, &a.ConfirmationSource, &a.ResolutionReason, &a.DependsOn); err != nil {
			rows.Close()
			return RunDetail{}, err
		}
		d.CustomerID = customer
		if invoice != "" {
			d.InvoiceID = invoice
		}
		d.Actions = append(d.Actions, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return RunDetail{}, err
	}
	if d.CustomerID != "" {
		d.CustomerKey, err = business.CustomerKey(stripeAccount, stripeMode, d.CustomerID)
		if err != nil {
			return RunDetail{}, err
		}
	}
	for i := range d.Actions {
		a := &d.Actions[i]
		attemptRows, queryErr := tx.Query(ctx, `SELECT id,attempt_number,coalesce(outcome,''),started_at,request_started_at,finished_at
 FROM business_attempts WHERE tenant_id=$1 AND action_id=$2 ORDER BY attempt_number`, tenant, a.ID)
		if queryErr != nil {
			return RunDetail{}, queryErr
		}
		for attemptRows.Next() {
			var item ActionAttempt
			if queryErr = attemptRows.Scan(&item.ID, &item.Number, &item.Outcome, &item.StartedAt, &item.RequestStartedAt, &item.FinishedAt); queryErr != nil {
				attemptRows.Close()
				return RunDetail{}, queryErr
			}
			a.Timeline = append(a.Timeline, item)
		}
		queryErr = attemptRows.Err()
		attemptRows.Close()
		if queryErr != nil {
			return RunDetail{}, queryErr
		}
		for j := range a.Timeline {
			callRows, callErr := tx.Query(ctx, `SELECT provider,method,request_path,response_status,coalesce(outcome,''),
 coalesce(provider_request_id,''),request_started_at FROM business_provider_calls
 WHERE tenant_id=$1 AND attempt_id=$2 ORDER BY request_started_at,id`, tenant, a.Timeline[j].ID)
			if callErr != nil {
				return RunDetail{}, callErr
			}
			for callRows.Next() {
				var call ProviderCall
				if callErr = callRows.Scan(&call.Provider, &call.Method, &call.Path, &call.Status, &call.Outcome,
					&call.RequestID, &call.StartedAt); callErr != nil {
					callRows.Close()
					return RunDetail{}, callErr
				}
				a.Timeline[j].Calls = append(a.Timeline[j].Calls, call)
			}
			callErr = callRows.Err()
			callRows.Close()
			if callErr != nil {
				return RunDetail{}, callErr
			}
		}
		decisionRows, queryErr := tx.Query(ctx, `SELECT decision,reason,from_status,to_status,created_at
 FROM business_action_decisions WHERE tenant_id=$1 AND action_id=$2 ORDER BY created_at,id`, tenant, a.ID)
		if queryErr != nil {
			return RunDetail{}, queryErr
		}
		for decisionRows.Next() {
			var item ActionDecision
			if queryErr = decisionRows.Scan(&item.Decision, &item.Reason, &item.FromStatus, &item.ToStatus, &item.CreatedAt); queryErr != nil {
				decisionRows.Close()
				return RunDetail{}, queryErr
			}
			a.Decisions = append(a.Decisions, item)
		}
		queryErr = decisionRows.Err()
		decisionRows.Close()
		if queryErr != nil {
			return RunDetail{}, queryErr
		}
	}
	targets := []string{d.ID}
	for _, action := range d.Actions {
		targets = append(targets, action.ID)
	}
	auditRows, err := tx.Query(ctx, `SELECT operation,target_type,target_id,result,created_at FROM audit_log
 WHERE tenant_id=$1 AND target_id=ANY($2::uuid[]) ORDER BY created_at DESC,id DESC LIMIT 30`, tenant, targets)
	if err != nil {
		return RunDetail{}, err
	}
	for auditRows.Next() {
		var item AuditEntry
		if err = auditRows.Scan(&item.Operation, &item.TargetType, &item.TargetID, &item.Result, &item.CreatedAt); err != nil {
			auditRows.Close()
			return RunDetail{}, err
		}
		d.Audit = append(d.Audit, item)
	}
	err = auditRows.Err()
	auditRows.Close()
	if err != nil {
		return RunDetail{}, err
	}
	return d, tx.Commit(ctx)
}
