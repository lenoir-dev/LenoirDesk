package reconciliation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/crypto"
	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
	"github.com/lenoir-dev/LenoirDesk/internal/telemetry"
	"github.com/stripe/stripe-go/v86"
)

// Reconciler only discovers durable work. CRM and Slack effects remain owned by
// the existing planner/executor; event discovery never authorizes Slack sends.
type Reconciler struct {
	DB        *store.Store
	Box       *crypto.Box
	Endpoints providers.Endpoints
	HTTP      *http.Client
	Quota     *execution.ProviderQuota
	Log       *slog.Logger
	// Fault is an isolated test-only crash boundary. Production leaves it nil.
	Fault func(stage string) bool
}

// The Events API returns the event's creation-time version even when the
// request pins stripe.APIVersion. The named Stage 0 sandbox currently returns
// Endive snapshot events. Discovery parses only the invariant identity fields
// and then re-reads billing state at the pinned adapter version. Keep this
// explicit allowlist separate from signed webhook destination validation.
func supportedDiscoveryVersion(version string) bool {
	return version == stripe.APIVersion || version == "2026-09-30.endive"
}

type page struct {
	Tenant, Connection, Kind, Cursor, Lease, RequestID, Source string
	Start, End                                                 time.Time
	Limit                                                      int
}

func (r *Reconciler) Step(ctx context.Context) (bool, error) {
	return r.step(ctx, "")
}

// StepTenant is used by an authorized bounded operation or an isolated test.
// It claims through tenant RLS; the periodic worker uses the fair global claim.
func (r *Reconciler) StepTenant(ctx context.Context, tenant string) (bool, error) {
	if tenant == "" {
		return false, errors.New("tenant required")
	}
	return r.step(ctx, tenant)
}

// StepHistorical consumes one bounded operator-approved page. It never sends
// invoice notifications, and its cap cannot be enlarged by a provider page.
func (r *Reconciler) StepHistorical(ctx context.Context) (bool, error) {
	return r.stepHistorical(ctx, "")
}

// StepHistoricalTenant lets a bounded operator/test worker claim only its
// authorized tenant's requests without touching another tenant's work.
func (r *Reconciler) StepHistoricalTenant(ctx context.Context, tenant string) (bool, error) {
	if tenant == "" {
		return false, errors.New("tenant required")
	}
	return r.stepHistorical(ctx, tenant)
}

func (r *Reconciler) stepHistorical(ctx context.Context, tenant string) (bool, error) {
	if r == nil || r.DB == nil || r.Box == nil {
		return false, errors.New("reconciliation unavailable")
	}
	if err := r.Endpoints.Validate(); err != nil {
		return false, err
	}
	var p page
	var maxEvents, scanned int
	var err error
	if tenant == "" {
		err = r.DB.Pool.QueryRow(ctx, `SELECT id,tenant_id,stripe_connection_id,from_at,to_at,cursor,max_events,scanned_count,lease_token FROM claim_historical_reconcile()`).
			Scan(&p.RequestID, &p.Tenant, &p.Connection, &p.Start, &p.End, &p.Cursor, &maxEvents, &scanned, &p.Lease)
	} else {
		tx, beginErr := store.BeginTenant(ctx, r.DB.Pool, tenant)
		if beginErr != nil {
			return false, beginErr
		}
		defer func() { _ = tx.Rollback(ctx) }()
		err = tx.QueryRow(ctx, `WITH picked AS (SELECT id FROM reconcile_requests WHERE tenant_id=$1
 AND status='pending' AND next_run_at<=now() AND (lease_until IS NULL OR lease_until<now())
 ORDER BY next_run_at,created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
 UPDATE reconcile_requests q SET lease_token=gen_random_uuid(),lease_until=now()+interval '5 minutes'
 FROM picked WHERE q.tenant_id=$1 AND q.id=picked.id
 RETURNING q.id,q.tenant_id,q.stripe_connection_id,q.from_at,q.to_at,q.cursor,q.max_events,q.scanned_count,q.lease_token`, tenant).
			Scan(&p.RequestID, &p.Tenant, &p.Connection, &p.Start, &p.End, &p.Cursor, &maxEvents, &scanned, &p.Lease)
		if err == nil {
			err = tx.Commit(ctx)
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	p.Kind, p.Source = "events", "historical"
	p.Limit = maxEvents - scanned
	if p.Limit > 25 {
		p.Limit = 25
	}
	if p.Limit < 1 || p.Start.Before(time.Now().Add(-29*24*time.Hour)) {
		return true, r.finishHistorical(ctx, p, p.Cursor, 0, 0, "blocked", "provider_history_gap")
	}
	b, err := r.connection(ctx, p)
	if err != nil {
		return true, r.finishHistorical(ctx, p, p.Cursor, 0, 0, "blocked", "connection_unavailable")
	}
	cursor, seen, discovered, more, err := r.events(ctx, p, b)
	if err != nil {
		var failure *pageFailure
		if errors.As(err, &failure) && failure.Wait > 0 && failure.Gap == "" {
			return true, r.deferHistorical(ctx, p, failure.Wait)
		}
		code := "reconciliation_unavailable"
		if errors.As(err, &failure) && failure.Gap != "" {
			code = failure.Gap
		}
		return true, r.finishHistorical(ctx, p, p.Cursor, 0, 0, "blocked", code)
	}
	status, gap := "pending", ""
	if !more {
		status = "completed"
	} else if int64(scanned)+seen >= int64(maxEvents) {
		status, gap = "limit_reached", "max_count_reached"
	}
	return true, r.finishHistorical(ctx, p, cursor, seen, discovered, status, gap)
}

func (r *Reconciler) step(ctx context.Context, tenant string) (bool, error) {
	if r == nil || r.DB == nil || r.Box == nil {
		return false, errors.New("reconciliation unavailable")
	}
	if err := r.Endpoints.Validate(); err != nil {
		return false, err
	}
	var p page
	var err error
	if tenant == "" {
		err = r.DB.Pool.QueryRow(ctx, `SELECT tenant_id,stripe_connection_id,kind,window_start,window_end,cursor,lease_token FROM claim_reconcile_page()`).
			Scan(&p.Tenant, &p.Connection, &p.Kind, &p.Start, &p.End, &p.Cursor, &p.Lease)
	} else {
		tx, beginErr := store.BeginTenant(ctx, r.DB.Pool, tenant)
		if beginErr != nil {
			return false, beginErr
		}
		defer func() { _ = tx.Rollback(ctx) }()
		err = tx.QueryRow(ctx, `WITH pick AS (SELECT stripe_connection_id,kind FROM reconcile_checkpoints
 WHERE tenant_id=$1 AND next_run_at<=now() AND (lease_until IS NULL OR lease_until<now())
 ORDER BY next_run_at,kind LIMIT 1 FOR UPDATE SKIP LOCKED)
 UPDATE reconcile_checkpoints cp SET lease_token=gen_random_uuid(),lease_until=now()+interval '5 minutes',
 last_attempt_at=now(),updated_at=now() FROM pick
 WHERE cp.tenant_id=$1 AND cp.stripe_connection_id=pick.stripe_connection_id AND cp.kind=pick.kind
 RETURNING cp.tenant_id,cp.stripe_connection_id,cp.kind,cp.window_start,cp.window_end,cp.cursor,cp.lease_token`, tenant).
			Scan(&p.Tenant, &p.Connection, &p.Kind, &p.Start, &p.End, &p.Cursor, &p.Lease)
		if err == nil {
			err = tx.Commit(ctx)
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if p.Kind == "events" && p.Start.Before(time.Now().Add(-29*24*time.Hour)) {
		return true, r.fail(ctx, p, "provider_history_gap", 30*time.Minute)
	}
	connection, err := r.connection(ctx, p)
	if err != nil {
		return true, r.fail(ctx, p, "connection_unavailable", 5*time.Minute)
	}
	var cursor string
	var scanned, discovered int64
	var more bool
	switch p.Kind {
	case "events":
		cursor, scanned, discovered, more, err = r.events(ctx, p, connection)
	case "known":
		cursor, scanned, discovered, more, err = r.known(ctx, p, connection)
	default:
		err = errors.New("invalid reconciliation kind")
	}
	if err != nil {
		var failure *pageFailure
		if errors.As(err, &failure) {
			if failure.Wait > 0 && failure.Gap == "" {
				return true, r.deferPage(ctx, p, failure.Wait)
			}
			return true, r.fail(ctx, p, failure.Gap, 5*time.Minute)
		}
		return true, r.fail(ctx, p, "reconciliation_unavailable", 5*time.Minute)
	}
	if r.Fault != nil && r.Fault("before_checkpoint") {
		return true, errors.New("injected reconciliation stop before checkpoint")
	}
	return true, r.complete(ctx, p, cursor, scanned, discovered, more)
}

type pageFailure struct {
	Gap  string
	Wait time.Duration
}

func (e *pageFailure) Error() string { return e.Gap }

func (r *Reconciler) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func (r *Reconciler) connection(ctx context.Context, p page) (store.Binding, error) {
	tx, err := store.BeginTenant(ctx, r.DB.Pool, p.Tenant)
	if err != nil {
		return store.Binding{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var b store.Binding
	var environment, health string
	err = tx.QueryRow(ctx, `SELECT account_id,mode,environment,health,generation,ciphertext,nonce,key_version FROM business_connection($1,$2,'stripe')`, p.Tenant, p.Connection).
		Scan(&b.AccountID, &b.Mode, &environment, &health, &b.Generation, &b.Ciphertext, &b.Nonce, &b.KeyVersion)
	if err != nil || b.Mode != "test" || health != "verified" ||
		(r.Endpoints.Simulation && environment != "simulation") || (!r.Endpoints.Simulation && environment != "test") {
		return store.Binding{}, errors.New("reconciliation connection unavailable")
	}
	b.TenantID, b.ConnectionID = p.Tenant, p.Connection
	return b, tx.Commit(ctx)
}

func (r *Reconciler) token(b store.Binding) (string, error) {
	plain, err := r.Box.Open(b.Ciphertext, b.Nonce, b.TenantID, b.ConnectionID, "stripe", b.KeyVersion)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (r *Reconciler) quota(ctx context.Context, account string) error {
	// Deterministic simulators may omit Redis, but an explicitly supplied
	// quota is still enforced so the real admission contract can be tested.
	if r.Endpoints.Simulation && r.Quota == nil {
		return nil
	}
	if r.Quota == nil {
		return &pageFailure{Gap: "quota_unavailable"}
	}
	wait, err := r.Quota.Allow(ctx, (execution.QuotaScope{Provider: "stripe", Account: account, Category: "reconciliation_read"}).Budgets())
	if err != nil {
		return &pageFailure{Gap: "quota_unavailable"}
	}
	if wait > 0 {
		telemetry.Wait(ctx, "stripe", wait)
		return &pageFailure{Wait: wait}
	}
	return nil
}

func (r *Reconciler) events(ctx context.Context, p page, b store.Binding) (string, int64, int64, bool, error) {
	if err := r.quota(ctx, b.AccountID); err != nil {
		return "", 0, 0, false, err
	}
	token, err := r.token(b)
	if err != nil {
		return "", 0, 0, false, &pageFailure{Gap: "credential_unavailable"}
	}
	q := url.Values{}
	q.Set("created[gte]", strconv.FormatInt(p.Start.Unix(), 10))
	q.Set("created[lte]", strconv.FormatInt(p.End.Unix(), 10))
	limit := p.Limit
	if limit < 1 || limit > 25 {
		limit = 25
	}
	q.Set("limit", strconv.Itoa(limit))
	if p.Cursor != "" {
		q.Set("starting_after", p.Cursor)
	}
	for _, eventType := range []string{"invoice.paid", "invoice.payment_failed", "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted"} {
		q.Add("types[]", eventType)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Endpoints.Stripe+"/v1/events?"+q.Encode(), nil)
	if err != nil {
		return "", 0, 0, false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Stripe-Version", stripe.APIVersion)
	started := time.Now()
	response, err := r.client().Do(req)
	if err != nil {
		telemetry.ProviderRequest(ctx, "stripe", 0, time.Since(started))
		return "", 0, 0, false, &pageFailure{Gap: "provider_unavailable"}
	}
	telemetry.ProviderRequest(ctx, "stripe", response.StatusCode, time.Since(started))
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "", 0, 0, false, &pageFailure{Gap: "events_permission_or_auth"}
		case http.StatusTooManyRequests:
			telemetry.Event(ctx, "http_429", "stripe")
			return "", 0, 0, false, &pageFailure{Wait: time.Minute}
		default:
			return "", 0, 0, false, &pageFailure{Gap: "events_api_failure"}
		}
	}
	var listed struct {
		HasMore bool              `json:"has_more"`
		Data    []json.RawMessage `json:"data"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&listed); err != nil || len(listed.Data) > limit || (listed.HasMore && len(listed.Data) == 0) {
		return "", 0, 0, false, &pageFailure{Gap: "events_page_incomplete"}
	}
	var created int64
	cursor := p.Cursor
	for _, raw := range listed.Data {
		var e struct {
			ID         string `json:"id"`
			Type       string `json:"type"`
			APIVersion string `json:"api_version"`
			Account    string `json:"account"`
			Created    int64  `json:"created"`
			Livemode   *bool  `json:"livemode"`
			Data       struct {
				Object json.RawMessage `json:"object"`
			} `json:"data"`
		}
		if len(raw) > 256<<10 || json.Unmarshal(raw, &e) != nil || !strings.HasPrefix(e.ID, "evt_") ||
			e.Livemode == nil || *e.Livemode != (b.Mode == "live") || e.Account != "" && e.Account != b.AccountID ||
			e.Created < p.Start.Unix() || e.Created > p.End.Unix() || !supportedDiscoveryVersion(e.APIVersion) {
			return "", 0, 0, false, &pageFailure{Gap: "events_identity_or_version"}
		}
		var object struct{ ID, Object string }
		if json.Unmarshal(e.Data.Object, &object) != nil || object.ID == "" ||
			(strings.HasPrefix(e.Type, "invoice.") && object.Object != "invoice") ||
			(strings.HasPrefix(e.Type, "customer.subscription.") && object.Object != "subscription") {
			return "", 0, 0, false, &pageFailure{Gap: "events_payload_unsupported"}
		}
		if _, parseErr := business.ParseSignal(raw); parseErr != nil {
			return "", 0, 0, false, &pageFailure{Gap: "events_payload_unsupported"}
		}
		source := p.Source
		if source == "" {
			source = "event_discovery"
		}
		accepted, acceptErr := r.DB.Accept(ctx, store.Incoming{Binding: b, EventID: e.ID, EventType: e.Type,
			APIVersion: e.APIVersion, CorrelationID: "reconcile-events", Classification: "supported", Source: source, Raw: raw})
		if acceptErr != nil {
			return "", 0, 0, false, acceptErr
		}
		if accepted {
			created++
		}
		cursor = e.ID
	}
	return cursor, int64(len(listed.Data)), created, listed.HasMore, nil
}

func (r *Reconciler) finishHistorical(ctx context.Context, p page, cursor string, scanned, discovered int64, status, gap string) error {
	tx, err := store.BeginTenant(ctx, r.DB.Pool, p.Tenant)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var finished *time.Time
	if status != "pending" {
		now := time.Now().UTC()
		finished = &now
	}
	tag, err := tx.Exec(ctx, `UPDATE reconcile_requests SET cursor=$3,scanned_count=scanned_count+$4,
 discovered_count=discovered_count+$5,status=$6,gap_code=nullif($7,''),finished_at=$8,
 lease_token=NULL,lease_until=NULL,next_run_at=now()+interval '5 seconds'
 WHERE tenant_id=$1 AND id=$2 AND lease_token=$9`, p.Tenant, p.RequestID, cursor, scanned, discovered, status, gap, finished, p.Lease)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("historical reconciliation lease lost")
	}
	return tx.Commit(ctx)
}

func (r *Reconciler) deferHistorical(ctx context.Context, p page, wait time.Duration) error {
	if wait < time.Second {
		wait = time.Second
	}
	if wait > time.Hour {
		wait = time.Hour
	}
	tx, err := store.BeginTenant(ctx, r.DB.Pool, p.Tenant)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE reconcile_requests SET lease_token=NULL,lease_until=NULL,
 next_run_at=now()+$3::interval WHERE tenant_id=$1 AND id=$2 AND lease_token=$4`,
		p.Tenant, p.RequestID, fmt.Sprintf("%f seconds", wait.Seconds()), p.Lease)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("historical reconciliation lease lost")
	}
	return tx.Commit(ctx)
}

func (r *Reconciler) known(ctx context.Context, p page, b store.Binding) (string, int64, int64, bool, error) {
	tx, err := store.BeginTenant(ctx, r.DB.Pool, p.Tenant)
	if err != nil {
		return "", 0, 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT config FROM automation_revisions WHERE tenant_id=$1 AND connection_id=$2 AND status='active'`, p.Tenant, p.Connection).Scan(&raw)
	if err != nil {
		return "", 0, 0, false, err
	}
	var config business.Config
	if json.Unmarshal(raw, &config) != nil || config.Validate() != nil {
		return "", 0, 0, false, &pageFailure{Gap: "automation_config_invalid"}
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT customer_id FROM business_actions WHERE tenant_id=$1 AND stripe_connection_id=$2 AND customer_id>$3 ORDER BY customer_id LIMIT 8`, p.Tenant, p.Connection, p.Cursor)
	if err != nil {
		return "", 0, 0, false, err
	}
	var customers []string
	for rows.Next() {
		var customer string
		if err = rows.Scan(&customer); err != nil {
			rows.Close()
			return "", 0, 0, false, err
		}
		customers = append(customers, customer)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", 0, 0, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", 0, 0, false, err
	}
	token, err := r.token(b)
	if err != nil {
		return "", 0, 0, false, &pageFailure{Gap: "credential_unavailable"}
	}
	cursor := p.Cursor
	var created int64
	for _, customer := range customers {
		if err = r.quota(ctx, b.AccountID); err != nil {
			return "", 0, 0, false, err
		}
		projection, readErr := (providers.Stripe{BaseURL: r.Endpoints.Stripe, HTTP: r.client()}).Snapshot(ctx, token, b.AccountID, b.Mode, customer, config.TrackedPriceIDs)
		if readErr != nil {
			return "", 0, 0, false, &pageFailure{Gap: "known_state_unavailable"}
		}
		// A scan epoch is part of the synthetic event identity. A crash rescan
		// deduplicates; a later epoch checks HubSpot again for drift.
		digest := sha256.Sum256([]byte(b.AccountID + "|" + customer + "|" + p.End.UTC().Format(time.RFC3339Nano) + "|" + projection.Hash()))
		id := "evt_rd_known_" + hex.EncodeToString(digest[:16])
		event := map[string]any{"id": id, "object": "event", "type": "customer.subscription.updated",
			"api_version": stripe.APIVersion, "created": time.Now().Unix(), "livemode": false,
			"data": map[string]any{"object": map[string]any{"id": projection.SubscriptionID, "object": "subscription", "customer": customer}}}
		eventRaw, _ := json.Marshal(event)
		accepted, acceptErr := r.DB.Accept(ctx, store.Incoming{Binding: b, EventID: id, EventType: "customer.subscription.updated",
			APIVersion: stripe.APIVersion, CorrelationID: "reconcile-known", Classification: "supported", Source: "known_state", Raw: eventRaw})
		if acceptErr != nil {
			return "", 0, 0, false, acceptErr
		}
		if accepted {
			created++
		}
		cursor = customer
	}
	return cursor, int64(len(customers)), created, len(customers) == 8, nil
}

func (r *Reconciler) complete(ctx context.Context, p page, cursor string, scanned, discovered int64, more bool) error {
	tx, err := store.BeginTenant(ctx, r.DB.Pool, p.Tenant)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	interval := 5 * time.Minute
	if p.Kind == "known" {
		interval = 15 * time.Minute
	}
	next := time.Now().Add(5 * time.Second)
	start, end := p.Start, p.End
	var completed *time.Time
	if !more {
		completed = &p.End
		next = time.Now().Add(interval)
		start = p.End.Add(-10 * time.Minute)
		end = time.Now().Add(-2 * time.Minute)
		if !start.Before(end) {
			start = end.Add(-time.Minute)
		}
		cursor = ""
	}
	tag, err := tx.Exec(ctx, `UPDATE reconcile_checkpoints SET cursor=$4,window_start=$5,window_end=$6,
 last_complete_at=coalesce($7,last_complete_at),next_run_at=$8,lease_token=NULL,lease_until=NULL,
 gap_code=NULL,gap_at=NULL,scanned_count=scanned_count+$9,discovered_count=discovered_count+$10,updated_at=now()
 WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind=$3 AND lease_token=$11`, p.Tenant, p.Connection, p.Kind,
		cursor, start, end, completed, next, scanned, discovered, p.Lease)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("reconciliation lease lost")
	}
	return tx.Commit(ctx)
}

func (r *Reconciler) fail(ctx context.Context, p page, code string, wait time.Duration) error {
	if code == "" {
		code = "reconciliation_unavailable"
	}
	tx, err := store.BeginTenant(ctx, r.DB.Pool, p.Tenant)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE reconcile_checkpoints SET gap_code=$4,gap_at=coalesce(gap_at,now()),
 lease_token=NULL,lease_until=NULL,next_run_at=now()+$5::interval,updated_at=now()
 WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind=$3 AND lease_token=$6`, p.Tenant, p.Connection, p.Kind, code, fmt.Sprintf("%f seconds", wait.Seconds()), p.Lease)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("reconciliation lease lost")
	}
	return tx.Commit(ctx)
}

func (r *Reconciler) deferPage(ctx context.Context, p page, wait time.Duration) error {
	if wait < time.Second {
		wait = time.Second
	}
	if wait > time.Hour {
		wait = time.Hour
	}
	tx, err := store.BeginTenant(ctx, r.DB.Pool, p.Tenant)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE reconcile_checkpoints SET lease_token=NULL,lease_until=NULL,
 next_run_at=now()+$4::interval,updated_at=now() WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind=$3 AND lease_token=$5`,
		p.Tenant, p.Connection, p.Kind, fmt.Sprintf("%f seconds", wait.Seconds()), p.Lease)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("reconciliation lease lost")
	}
	return tx.Commit(ctx)
}
