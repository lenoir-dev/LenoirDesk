package execution

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/crypto"
	"github.com/lenoir-dev/LenoirDesk/internal/providerconn"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
	"github.com/lenoir-dev/LenoirDesk/internal/recoverywitness"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
	"github.com/lenoir-dev/LenoirDesk/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type Executor struct {
	DB             *store.Store
	Box            *crypto.Box
	Endpoints      providers.Endpoints
	Log            *slog.Logger
	HTTP           *http.Client
	Retry          RetryPolicy
	httpOnce       sync.Once
	Limiter        WriteLimiter
	RequireLimiter bool
	Quota          *ProviderQuota
	Breaker        *ProviderBreaker
	Refresher      *providerconn.Refresher
	HubSpotAppID   string
	SlackAppID     string
	Witness        *recoverywitness.Witness
	// Fault is a test-only crash boundary. Production leaves it nil.
	Fault func(stage, tenantID, actionID string) bool
}

func (x *Executor) stopAt(stage string, c claim) bool {
	return x.Fault != nil && x.Fault(stage, c.Tenant, c.Action)
}

type claim struct {
	Tenant, Action, Token, Attempt string
	Generation                     int64
}
type action struct {
	Kind, Revision, Effect, StripeConnection, Customer, Invoice string
	Config                                                      business.Config
}
type connection struct {
	Account, Mode, Environment, Health, Token string
	Generation                                int64
}

func (x *Executor) Run(ctx context.Context) {
	if x == nil {
		return
	}
	if err := x.Endpoints.Validate(); err != nil {
		x.Log.Error("provider execution disabled", "error", err)
		return
	}
	x.httpOnce.Do(func() {
		if x.HTTP == nil {
			x.HTTP = &http.Client{Timeout: 5 * time.Second}
		}
	})
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticker.C:
		}
		for len(slots) < cap(slots) {
			c, err := x.claim(ctx)
			if err != nil {
				x.Log.Warn("business claim failed", "error", err)
				break
			}
			if c.Action == "" {
				break
			}
			slots <- struct{}{}
			wg.Add(1)
			go func(c claim) { defer wg.Done(); defer func() { <-slots }(); x.execute(ctx, c) }(c)
		}
	}
}

// ExecutePointer is the broker-driven production path. The legacy Run poller
// remains for isolated executor tests; both use the same execution and finish
// protocol. A pointer is ACKable only after its claim is durably resolved or
// shown stale. A live same-generation claim is left for lease repair.
func (x *Executor) ExecutePointer(ctx context.Context, tenantID, actionID string, dispatchGeneration int64) (bool, error) {
	allowed, gateErr := x.outboundAllowed(ctx)
	if gateErr != nil || !allowed {
		return false, gateErr
	}
	if err := x.Endpoints.Validate(); err != nil {
		return false, err
	}
	x.httpOnce.Do(func() {
		if x.HTTP == nil {
			x.HTTP = &http.Client{Timeout: 5 * time.Second}
		}
	})
	var c claim
	err := x.DB.Pool.QueryRow(ctx, `SELECT tenant_id,action_id,fence_token,fence_generation,attempt_id
 FROM claim_business_action_pointer($1,$2,$3)`, tenantID, actionID, dispatchGeneration).
		Scan(&c.Tenant, &c.Action, &c.Token, &c.Generation, &c.Attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		tx, txErr := store.BeginTenant(ctx, x.DB.Pool, tenantID)
		if txErr != nil {
			return false, txErr
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var live bool
		txErr = tx.QueryRow(ctx, `SELECT status='queued' AND dispatch_generation=$3
 FROM business_actions WHERE tenant_id=$1 AND id=$2`, tenantID, actionID, dispatchGeneration).Scan(&live)
		if errors.Is(txErr, pgx.ErrNoRows) {
			return true, nil
		}
		if txErr != nil {
			return false, txErr
		}
		if txErr = tx.Commit(ctx); txErr != nil {
			return false, txErr
		}
		return !live, nil
	}
	if err != nil {
		return false, err
	}
	x.execute(ctx, c)
	live, err := x.DB.PointerState(ctx, c.Tenant, c.Action, c.Token, c.Generation)
	return !live, err
}
func (x *Executor) claim(ctx context.Context) (claim, error) {
	allowed, gateErr := x.outboundAllowed(ctx)
	if gateErr != nil || !allowed {
		return claim{}, gateErr
	}
	var c claim
	err := x.DB.Pool.QueryRow(ctx, `SELECT tenant_id,action_id,fence_token,fence_generation,attempt_id FROM claim_business_action()`).Scan(&c.Tenant, &c.Action, &c.Token, &c.Generation, &c.Attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return claim{}, nil
	}
	return c, err
}

func (x *Executor) outboundAllowed(ctx context.Context) (bool, error) {
	var enabled bool
	err := x.DB.Pool.QueryRow(ctx, `SELECT outbound_enabled FROM restore_safety_gate WHERE singleton=true`).Scan(&enabled)
	if err == nil && enabled && x.Witness != nil {
		err = x.Witness.Check(ctx)
	}
	return enabled, err
}
func (x *Executor) load(ctx context.Context, c claim) (action, error) {
	var a action
	var raw []byte
	tx, err := store.BeginTenant(ctx, x.DB.Pool, c.Tenant)
	if err != nil {
		return a, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = tx.QueryRow(ctx, `SELECT kind,automation_revision_id,effect_key,stripe_connection_id,customer_id,invoice_id,r.config
 FROM business_actions b JOIN automation_revisions r ON r.tenant_id=b.tenant_id AND r.id=b.automation_revision_id
 WHERE b.tenant_id=$1 AND b.id=$2 AND b.lease_token=$3 AND b.generation=$4`, c.Tenant, c.Action, c.Token, c.Generation).Scan(&a.Kind, &a.Revision, &a.Effect, &a.StripeConnection, &a.Customer, &a.Invoice, &raw)
	if err != nil {
		return a, err
	}
	if json.Unmarshal(raw, &a.Config) != nil || a.Config.Validate() != nil || a.Config.StripeConnectionID != a.StripeConnection {
		return a, errStoredConfig
	}
	return a, tx.Commit(ctx)
}

var errStoredConfig = errors.New("invalid stored business config")

func (x *Executor) credential(ctx context.Context, c claim, id, provider string) (connection, error) {
	if !x.Endpoints.Simulation && provider != "stripe" {
		if x.Refresher == nil {
			return connection{}, &providers.Failure{Class: "refresh_unavailable", Outcome: providers.BlockedAuth}
		}
		if err := x.Refresher.Ensure(ctx, c.Tenant, id); err != nil {
			if errors.Is(err, providerconn.ErrRefreshBusy) {
				return connection{}, &providers.Failure{Class: "refresh_in_progress", Outcome: providers.SafeRetryable, RetryAfter: 5 * time.Second}
			}
			return connection{}, &providers.Failure{Class: "credential_refresh_blocked", Outcome: providers.BlockedAuth}
		}
	}
	var out connection
	var cipher, nonce []byte
	var version int
	err := x.DB.Pool.QueryRow(ctx, `SELECT account_id,mode,environment,health,generation,ciphertext,nonce,key_version FROM business_connection($1,$2,$3)`, c.Tenant, id, provider).Scan(&out.Account, &out.Mode, &out.Environment, &out.Health, &out.Generation, &cipher, &nonce, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, &providers.Failure{Class: "connection_missing_or_revoked", Outcome: providers.BlockedAuth}
	}
	if err != nil {
		return out, err
	}
	expectedEnv := "test"
	if x.Endpoints.Simulation {
		expectedEnv = "simulation"
	}
	if out.Health != "verified" || out.Environment != expectedEnv || out.Mode != "test" {
		return out, &providers.Failure{Class: "connection_unverified_or_mode_blocked", Outcome: providers.BlockedConfig}
	}
	secret, err := x.Box.Open(cipher, nonce, c.Tenant, id, provider, version)
	if err != nil {
		return out, &providers.Failure{Class: "credential_decryption_failed", Outcome: providers.BlockedAuth}
	}
	if x.Endpoints.Simulation || provider == "stripe" {
		out.Token = string(secret)
	} else {
		var issued providerconn.Tokens
		if json.Unmarshal(secret, &issued) != nil || issued.AccessToken == "" ||
			(issued.Rotation == "rotating" && issued.ExpiresAt.Before(time.Now().Add(30*time.Second))) {
			return out, &providers.Failure{Class: "credential_invalid", Outcome: providers.BlockedAuth}
		}
		out.Token = issued.AccessToken
	}
	return out, nil
}
func (x *Executor) execute(parent context.Context, c claim) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	ctx, span := otel.Tracer("relaydesk/business").Start(ctx, "business.action.execute")
	defer span.End()
	stopped := make(chan struct{})
	go x.heartbeat(ctx, cancel, c, stopped)
	defer func() { cancel(); <-stopped }()
	a, err := x.load(ctx, c)
	if err != nil {
		if errors.Is(err, errStoredConfig) {
			x.finishSafe(c, "blocked_config", "stored_config_invalid", 0, "", "", "")
		}
		return
	}
	span.SetAttributes(attribute.String("relaydesk.action.kind", a.Kind))
	if a.Kind == "crm_projection" {
		defer func() { telemetry.CRMElapsed(parent, time.Since(started)) }()
	}
	if x.stopAt("before_outbound", c) {
		return
	}
	allowed, gateErr := x.outboundAllowed(ctx)
	if gateErr != nil || !allowed {
		return // watchdog preserves the obligation; no provider call is made
	}
	if x.Witness != nil && x.Witness.Prepare(ctx, c.Tenant, c.Action) != nil {
		return // lease recovery keeps the obligation; outbound was not attempted
	}
	if x.Witness != nil {
		defer func() {
			completeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if x.Witness.Complete(completeCtx, c.Tenant, c.Action) != nil {
				x.Log.Warn("external restore witness remains closed; operator review required")
			}
		}()
	}
	stripeConn, err := x.credential(ctx, c, a.StripeConnection, "stripe")
	if err != nil {
		x.finishFailure(c, err)
		return
	}
	switch a.Kind {
	case "crm_projection":
		x.crm(ctx, c, a, stripeConn)
	case "slack_invoice_paid":
		x.slack(ctx, c, a, stripeConn)
	default:
		x.finishSafe(c, "blocked_config", "invalid_action_kind", 0, "", "", "")
	}
}

// A heartbeat is fenced by both the token and generation. Losing the claim
// cancels the HTTP context, but cannot retract a request already transmitted.
func (x *Executor) heartbeat(ctx context.Context, cancel context.CancelFunc, c claim, stopped chan<- struct{}) {
	defer close(stopped)
	ticker := time.NewTicker(8 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pulse, done := context.WithTimeout(ctx, 3*time.Second)
		tx, err := store.BeginTenant(pulse, x.DB.Pool, c.Tenant)
		if err == nil {
			var kind string
			err = tx.QueryRow(pulse, `UPDATE business_actions SET lease_until=now()+interval '30 seconds'
 WHERE tenant_id=$1 AND id=$2 AND status='running' AND lease_token=$3 AND generation=$4
 AND lease_until>now() RETURNING kind`, c.Tenant, c.Action, c.Token, c.Generation).Scan(&kind)
			if err == nil && kind == "crm_projection" {
				var tag pgconn.CommandTag
				tag, err = tx.Exec(pulse, `UPDATE crm_aggregate_leases SET lease_until=now()+interval '30 seconds'
 WHERE tenant_id=$1 AND action_id=$2 AND lease_token=$3 AND lease_until>now()`, c.Tenant, c.Action, c.Token)
				if err == nil && tag.RowsAffected() != 1 {
					err = errors.New("aggregate lease lost")
				}
			}
			if err == nil {
				err = tx.Commit(pulse)
			} else {
				_ = tx.Rollback(pulse)
			}
		}
		done()
		if err != nil {
			x.Log.Warn("business heartbeat lost", "action_id", c.Action, "error", err)
			cancel()
			return
		}
	}
}
func (x *Executor) crm(ctx context.Context, c claim, a action, sc connection) {
	customerKey, err := business.CustomerKey(sc.Account, sc.Mode, a.Customer)
	if err != nil {
		x.finishSafe(c, "blocked_config", "stripe_customer_identity_invalid", 0, "", "", "")
		return
	}
	hs, err := x.credential(ctx, c, a.Config.HubSpotConnectionID, "hubspot")
	if err != nil {
		x.finishFailure(c, err)
		return
	}
	linkID, err := x.contactLink(ctx, c, customerKey, a.Config.HubSpotConnectionID)
	if err != nil {
		x.finishFailure(c, err)
		return
	}
	if linkID == "" {
		// New Contacts need a write. Check the current quota before making
		// three Stripe reads and a HubSpot read that would be discarded when
		// the write is already known to be unavailable. Existing links may
		// need only a no-op comparison, so they do not wait here.
		if err = x.preflightWriteQuota(ctx, c, a.Config.HubSpotConnectionID,
			QuotaScope{Provider: "hubspot", App: x.HubSpotAppID, Account: hs.Account, Method: "contact.write"}); err != nil {
			x.writeGateFailure(c, err)
			return
		}
	}
	stripeClient := x.recordingClient(c, QuotaScope{Provider: "stripe", Account: sc.Account})
	projection, err := (providers.Stripe{BaseURL: x.Endpoints.Stripe, HTTP: stripeClient}).Snapshot(ctx, sc.Token, sc.Account, sc.Mode, a.Customer, a.Config.TrackedPriceIDs)
	if err != nil {
		x.finishFailure(c, err)
		return
	}
	if projection.CustomerKey != customerKey {
		x.finishSafe(c, "needs_attention", "stripe_customer_identity_conflict", 0, "", "", "")
		return
	}
	adapter := providers.HubSpot{BaseURL: x.Endpoints.HubSpot, Token: hs.Token,
		HTTP: x.recordingClient(c, QuotaScope{Provider: "hubspot", App: x.HubSpotAppID, Account: hs.Account})}
	var contact providers.Contact
	var found bool
	if linkID != "" {
		contact, found, err = adapter.Read(ctx, linkID, false)
	} else {
		contact, found, err = adapter.Read(ctx, projection.CustomerKey, true)
	}
	if err != nil {
		x.finishFailure(c, err)
		return
	}
	if linkID != "" && !found {
		x.finishSafe(c, "needs_attention", "mapped_contact_missing", 0, "", "", "")
		return
	}
	if found && ((linkID == "" && contact.Properties["relaydesk_customer_key"] != projection.CustomerKey) ||
		(contact.Properties["relaydesk_customer_key"] != "" && contact.Properties["relaydesk_customer_key"] != projection.CustomerKey)) {
		x.finishSafe(c, "needs_attention", "contact_identity_conflict", 0, "", "", "")
		return
	}
	if linkID != "" && found && contact.ID != linkID {
		// HubSpot can return a new canonical ID when a mapped Contact was merged.
		// Rebind only when both lookup paths independently identify that same
		// Contact and its managed identity key is still present. Never treat an
		// arbitrary redirect (or an archived Contact) as permission to remap.
		if contact.Properties["relaydesk_customer_key"] != projection.CustomerKey {
			x.finishSafe(c, "needs_attention", "contact_identity_conflict", 0, "", "", "")
			return
		}
		byKey, ok, readErr := adapter.Read(ctx, projection.CustomerKey, true)
		if readErr != nil {
			x.finishFailure(c, readErr)
			return
		}
		if !ok || byKey.ID != contact.ID || byKey.Properties["relaydesk_customer_key"] != projection.CustomerKey {
			x.finishSafe(c, "needs_attention", "contact_identity_conflict", 0, "", "", "")
			return
		}
		linked, relinkErr := x.relinkMergedContact(ctx, c, projection.CustomerKey, a.Config.HubSpotConnectionID, linkID, contact.ID)
		if relinkErr != nil {
			x.finishFailure(c, relinkErr)
			return
		}
		if !linked {
			x.finishSafe(c, "needs_attention", "contact_identity_conflict", 0, "", "", "")
			return
		}
	}
	desired := projection.Managed()
	if found && sameManaged(contact.Properties, desired) {
		if err = x.saveLink(ctx, c, projection.CustomerKey, a.Config.HubSpotConnectionID, contact.ID, "unique_key"); err != nil {
			x.finishFailure(c, err)
			return
		}
		x.finishSafe(c, "succeeded", "", 0, projection.SubscriptionID, contact.ID, "")
		return
	}
	desired["relaydesk_synced_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if !found {
		if a.Config.CreationPolicy != "unique_upsert" {
			x.finishSafe(c, "needs_attention", "explicit_contact_link_required", 0, "", "", "")
			return
		}
		if err = x.markWriteStarted(ctx, c, a.Config.HubSpotConnectionID,
			QuotaScope{Provider: "hubspot", App: x.HubSpotAppID, Account: hs.Account, Method: "contact.write"}); err != nil {
			x.writeGateFailure(c, err)
			return
		}
		if x.stopAt("after_request_started", c) {
			return
		}
		contact, err = adapter.Upsert(ctx, projection.CustomerKey, desired)
	} else {
		if err = x.markWriteStarted(ctx, c, a.Config.HubSpotConnectionID,
			QuotaScope{Provider: "hubspot", App: x.HubSpotAppID, Account: hs.Account, Method: "contact.write"}); err != nil {
			x.writeGateFailure(c, err)
			return
		}
		if x.stopAt("after_request_started", c) {
			return
		}
		contact, err = adapter.Update(ctx, contact.ID, desired)
	}
	if err != nil {
		f := providers.SafeClass(err)
		if f.Ambiguous {
			observed, ok, readErr := adapter.Read(ctx, projection.CustomerKey, true)
			if readErr == nil && ok && sameManaged(observed.Properties, desired) {
				contact = observed
				err = nil
			}
		}
	}
	if err != nil {
		x.finishFailure(c, err)
		return
	}
	verified, ok, err := adapter.Read(ctx, projection.CustomerKey, true)
	if err != nil || !ok || !sameManaged(verified.Properties, desired) {
		x.finishSafe(c, "retry_wait", "hubspot_confirmation_pending", time.Second, "", "", "")
		return
	}
	if contact.ID != "" && verified.ID != contact.ID {
		x.finishSafe(c, "needs_attention", "contact_identity_conflict", 0, "", "", "")
		return
	}
	if err = x.saveLink(ctx, c, projection.CustomerKey, a.Config.HubSpotConnectionID, verified.ID, "upsert"); err != nil {
		x.finishFailure(c, err)
		return
	}
	if x.stopAt("after_crm_confirmed", c) {
		return
	}
	x.finishSafe(c, "succeeded", "", 0, projection.SubscriptionID, verified.ID, "")
}
func sameManaged(observed, desired map[string]string) bool {
	for key, value := range desired {
		if key == "relaydesk_synced_at" {
			continue
		}
		if observed[key] != value {
			return false
		}
	}
	return true
}
func (x *Executor) contactLink(ctx context.Context, c claim, key, hubspot string) (string, error) {
	tx, err := store.BeginTenant(ctx, x.DB.Pool, c.Tenant)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	err = tx.QueryRow(ctx, `SELECT contact_id FROM contact_links WHERE tenant_id=$1 AND customer_key=$2 AND hubspot_connection_id=$3`, c.Tenant, key, hubspot).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}
func (x *Executor) saveLink(ctx context.Context, c claim, key, hubspot, id, source string) error {
	tx, err := store.BeginTenant(ctx, x.DB.Pool, c.Tenant)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `INSERT INTO contact_links(tenant_id,customer_key,hubspot_connection_id,contact_id,source)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT(tenant_id,customer_key,hubspot_connection_id)
 DO UPDATE SET contact_id=excluded.contact_id WHERE contact_links.contact_id=excluded.contact_id`, c.Tenant, key, hubspot, id, source)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("contact mapping conflict")
	}
	return tx.Commit(ctx)
}
func (x *Executor) relinkMergedContact(ctx context.Context, c claim, key, hubspot, priorID, canonicalID string) (bool, error) {
	tx, err := store.BeginTenant(ctx, x.DB.Pool, c.Tenant)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE contact_links SET contact_id=$5
 WHERE tenant_id=$1 AND customer_key=$2 AND hubspot_connection_id=$3 AND contact_id=$4`,
		c.Tenant, key, hubspot, priorID, canonicalID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return false, nil
		}
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	return true, tx.Commit(ctx)
}
func (x *Executor) slack(ctx context.Context, c claim, a action, sc connection) {
	slackConn, err := x.credential(ctx, c, a.Config.SlackConnectionID, "slack")
	if err != nil {
		x.finishFailure(c, err)
		return
	}
	expected := business.InvoiceEffectKey(c.Tenant, sc.Account, sc.Mode, a.Invoice, slackConn.Account, a.Config.SlackChannelID)
	if a.Effect != expected {
		x.finishSafe(c, "blocked_config", "slack_destination_changed", 0, "", "", "")
		return
	}
	if err = x.preflightWriteQuota(ctx, c, a.Config.SlackConnectionID,
		QuotaScope{Provider: "slack", App: x.SlackAppID, Account: slackConn.Account,
			Method: "chat.postMessage", Channel: a.Config.SlackChannelID}); err != nil {
		x.writeGateFailure(c, err)
		return
	}
	stripeClient := x.recordingClient(c, QuotaScope{Provider: "stripe", Account: sc.Account})
	projection, err := (providers.Stripe{BaseURL: x.Endpoints.Stripe, HTTP: stripeClient}).Snapshot(ctx, sc.Token, sc.Account, sc.Mode, a.Customer, a.Config.TrackedPriceIDs)
	if err != nil {
		x.finishFailure(c, err)
		return
	}
	err = (providers.Stripe{BaseURL: x.Endpoints.Stripe, HTTP: stripeClient}).PaidInvoice(ctx, sc.Token, a.Invoice, a.Customer, projection.SubscriptionID, sc.Mode)
	if err != nil {
		x.finishFailure(c, err)
		return
	}
	if err = x.markWriteStarted(ctx, c, a.Config.SlackConnectionID,
		QuotaScope{Provider: "slack", App: x.SlackAppID, Account: slackConn.Account,
			Method: "chat.postMessage", Channel: a.Config.SlackChannelID}); err != nil {
		x.writeGateFailure(c, err)
		return
	}
	if x.stopAt("after_request_started", c) {
		return
	}
	text := "Invoice paid; CRM synchronized. Invoice: " + a.Invoice
	channel, ts, err := (providers.Slack{BaseURL: x.Endpoints.Slack, Token: slackConn.Token,
		HTTP: x.recordingClient(c, QuotaScope{Provider: "slack", App: x.SlackAppID, Account: slackConn.Account,
			Method: "chat.postMessage", Channel: a.Config.SlackChannelID})}).Post(ctx, a.Config.SlackChannelID, text)
	if err != nil {
		x.finishFailure(c, err)
		return
	}
	if x.stopAt("after_slack_response", c) {
		return
	}
	x.finishSafe(c, "succeeded", "", 0, "", channel, ts)
}
func (x *Executor) preflightWriteQuota(ctx context.Context, c claim, targetConnection string, scope QuotaScope) error {
	var wait time.Duration
	var err error
	if !x.Endpoints.Simulation {
		if x.Quota == nil {
			return errQuotaUnavailable
		}
		wait, err = x.Quota.Peek(ctx, scope.Budgets())
	} else if x.Limiter != nil {
		// Custom limiters without a non-consuming preview continue to use the
		// mandatory final Allow gate; no preview can authorize a write.
		if preview, ok := x.Limiter.(interface {
			Peek(context.Context, string, string) (time.Duration, error)
		}); ok {
			wait, err = preview.Peek(ctx, c.Tenant, targetConnection)
		}
	} else if x.RequireLimiter {
		return errQuotaUnavailable
	}
	if err != nil {
		telemetry.Event(ctx, "redis_outage", scope.Provider)
		return errQuotaUnavailable
	}
	if wait > 0 {
		telemetry.Wait(ctx, scope.Provider, wait)
		return &providerCooldown{Delay: wait}
	}
	return nil
}

func (x *Executor) markWriteStarted(ctx context.Context, c claim, targetConnection string, scope QuotaScope) error {
	if !x.Endpoints.Simulation {
		if x.Quota == nil {
			return errQuotaUnavailable
		}
		wait, err := x.Quota.Allow(ctx, scope.Budgets())
		if err != nil {
			telemetry.Event(ctx, "redis_outage", scope.Provider)
			return errQuotaUnavailable
		}
		if wait > 0 {
			telemetry.Wait(ctx, scope.Provider, wait)
			return &providerCooldown{Delay: wait}
		}
	} else if x.Limiter != nil {
		wait, err := x.Limiter.Allow(ctx, c.Tenant, targetConnection)
		if err != nil {
			return errQuotaUnavailable
		}
		if wait > 0 {
			return &providerCooldown{Delay: wait}
		}
	} else if x.RequireLimiter {
		return errQuotaUnavailable
	}
	tx, err := store.BeginTenant(ctx, x.DB.Pool, c.Tenant)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	expectedEnv := "test"
	if x.Endpoints.Simulation {
		expectedEnv = "simulation"
	}
	tag, err := tx.Exec(ctx, `UPDATE business_attempts ba SET request_started_at=now()
    FROM business_actions a WHERE ba.tenant_id=$1 AND ba.id=$2 AND ba.lease_token=$3
    AND ba.fence_generation=$4 AND ba.request_started_at IS NULL
    AND a.tenant_id=ba.tenant_id AND a.id=ba.action_id AND a.status='running'
    AND a.lease_until>now()
    AND EXISTS (SELECT 1 FROM connections target WHERE target.tenant_id=a.tenant_id
      AND target.id=$5 AND target.status='active' AND target.health_status='verified'
      AND target.execution_environment=$6 AND target.generation=ba.connection_generation)
    AND NOT EXISTS (SELECT 1 FROM business_provider_cooldowns cd WHERE cd.tenant_id=a.tenant_id
      AND cd.connection_id IN (a.stripe_connection_id,$5::uuid) AND cd.cooldown_until>now())
    AND NOT EXISTS (SELECT 1 FROM automation_gates g WHERE g.tenant_id=a.tenant_id
      AND g.stripe_connection_id=a.stripe_connection_id AND g.paused)`, c.Tenant, c.Attempt, c.Token, c.Generation, targetConnection, expectedEnv)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		var paused bool
		if err = tx.QueryRow(ctx, `SELECT coalesce((SELECT g.paused FROM automation_gates g JOIN business_actions a
      ON a.tenant_id=g.tenant_id AND a.stripe_connection_id=g.stripe_connection_id
      WHERE a.tenant_id=$1 AND a.id=$2),false)`, c.Tenant, c.Action).Scan(&paused); err != nil {
			return err
		}
		if paused {
			return errWriteGateClosed
		}
		var remaining float64
		if err = tx.QueryRow(ctx, `SELECT coalesce(max(extract(epoch from cd.cooldown_until-now())),0)
 FROM business_provider_cooldowns cd JOIN business_actions a ON a.tenant_id=cd.tenant_id
 WHERE a.tenant_id=$1 AND a.id=$2 AND cd.connection_id IN (a.stripe_connection_id,$3::uuid)
 AND cd.cooldown_until>now()`, c.Tenant, c.Action, targetConnection).Scan(&remaining); err != nil {
			return err
		}
		if remaining > 0 {
			return &providerCooldown{Delay: time.Duration(remaining * float64(time.Second))}
		}
		return errConnectionChanged
	}
	return tx.Commit(ctx)
}

var errWriteGateClosed = errors.New("business write gate closed")
var errConnectionChanged = errors.New("business connection generation changed")
var errQuotaUnavailable = errors.New("write quota guard unavailable")

type providerCooldown struct{ Delay time.Duration }

func (c *providerCooldown) Error() string { return "provider cooldown active" }

func (x *Executor) writeGateFailure(c claim, err error) {
	if errors.Is(err, errQuotaUnavailable) {
		x.finishSafe(c, "retry_wait", "quota_guard_unavailable", 5*time.Second, "", "", "")
		return
	}
	var cooldown *providerCooldown
	if errors.As(err, &cooldown) {
		x.finishSafe(c, "retry_wait", "provider_cooldown", cooldown.Delay, "", "", "")
		return
	}
	if errors.Is(err, errWriteGateClosed) {
		x.finishSafe(c, "paused", "automation_paused", 0, "", "", "")
		return
	}
	if errors.Is(err, errConnectionChanged) {
		x.finishSafe(c, "blocked_auth", "connection_changed_before_write", 0, "", "", "")
		return
	}
	x.finishFailure(c, err)
}

type recordingTransport struct {
	x     *Executor
	c     claim
	base  http.RoundTripper
	scope QuotaScope
}

func (x *Executor) recordingClient(c claim, scope QuotaScope) *http.Client {
	client := *x.HTTP
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = &recordingTransport{x: x, c: c, base: base, scope: scope}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}
func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	provider := ""
	switch {
	case strings.HasPrefix(req.URL.Path, "/v1/"):
		provider = "stripe"
	case strings.HasPrefix(req.URL.Path, "/crm/"):
		provider = "hubspot"
	case req.URL.Path == "/api/chat.postMessage":
		provider = "slack"
	}
	if provider == "" || !t.trustedEndpoint(req.URL, provider) {
		return nil, errors.New("untrusted provider request")
	}
	traceCtx, span := otel.Tracer("relaydesk/provider").Start(req.Context(), "provider.http")
	defer span.End()
	span.SetAttributes(attribute.String("relaydesk.provider", provider), attribute.String("http.request.method", req.Method))
	req = req.WithContext(traceCtx)
	probeToken := ""
	if !t.x.Endpoints.Simulation {
		if t.scope.Provider != provider || t.x.Quota == nil || t.x.Breaker == nil {
			return nil, &providers.Failure{Class: "quota_unavailable", Outcome: providers.SafeRetryable, RetryAfter: 5 * time.Second}
		}
		wait, token, breakerErr := t.x.Breaker.Before(req.Context(), provider, t.scope.Account)
		probeToken = token
		if breakerErr != nil {
			telemetry.Event(req.Context(), "redis_outage", provider)
			return nil, &providers.Failure{Class: "breaker_unavailable", Outcome: providers.SafeRetryable, RetryAfter: 5 * time.Second}
		}
		if wait > 0 {
			telemetry.Wait(req.Context(), provider, wait)
			telemetry.Event(req.Context(), "breaker_open", provider)
			return nil, &providers.Failure{Class: "provider_circuit_open", Outcome: providers.SafeRetryable, RetryAfter: wait}
		}
		if probeToken != "" {
			telemetry.Event(req.Context(), "breaker_half_open", provider)
		}
		defer func() {
			// A rejected quota request must not consume a half-open probe.
			if probeToken != "" {
				// Response handling below clears the token after it records the outcome.
				updateCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = t.x.Breaker.After(updateCtx, provider, t.scope.Account, probeToken, 429, false)
			}
		}()
		if req.Method == http.MethodGet {
			scope := t.scope
			if provider == "stripe" {
				scope.Category = strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/"), "/")[0]
			}
			wait, quotaErr := t.x.Quota.Allow(req.Context(), scope.Budgets())
			if quotaErr != nil {
				telemetry.Event(req.Context(), "redis_outage", provider)
				return nil, &providers.Failure{Class: "quota_unavailable", Outcome: providers.SafeRetryable, RetryAfter: 5 * time.Second}
			}
			if wait > 0 {
				telemetry.Wait(req.Context(), provider, wait)
				return nil, &providers.Failure{Class: "provider_quota_wait", Outcome: providers.SafeRetryable, RetryAfter: wait}
			}
		}
	}
	tx, err := store.BeginTenant(req.Context(), t.x.DB.Pool, t.c.Tenant)
	if err != nil {
		return nil, err
	}
	var callID string
	err = tx.QueryRow(req.Context(), `INSERT INTO business_provider_calls(tenant_id,attempt_id,provider,method,request_path)
    VALUES($1,$2,$3,$4,$5) RETURNING id`, t.c.Tenant, t.c.Attempt, provider, req.Method, req.URL.Path).Scan(&callID)
	if err != nil {
		_ = tx.Rollback(req.Context())
		return nil, err
	}
	if err = tx.Commit(req.Context()); err != nil {
		return nil, err
	}
	requestStart := time.Now()
	resp, callErr := t.base.RoundTrip(req)
	responseStatus := 0
	if resp != nil {
		responseStatus = resp.StatusCode
	}
	if responseStatus != 0 {
		span.SetAttributes(attribute.Int("http.response.status_code", responseStatus))
	}
	if callErr != nil {
		span.SetStatus(codes.Error, "provider_transport_failed")
	} else if responseStatus >= http.StatusInternalServerError {
		span.SetStatus(codes.Error, "provider_unavailable")
	}
	telemetry.ProviderRequest(req.Context(), provider, responseStatus, time.Since(requestStart))
	if !t.x.Endpoints.Simulation {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			if status == http.StatusTooManyRequests {
				telemetry.Event(req.Context(), "http_429", provider)
				delay := retryAfterDelay(resp.Header.Get("Retry-After"))
				if delay > 0 {
					scope := t.scope
					if provider == "stripe" {
						scope.Category = strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/"), "/")[0]
					}
					for _, budget := range scope.Budgets() {
						coolCtx, stop := context.WithTimeout(context.Background(), time.Second)
						if err := t.x.Quota.Cooldown(coolCtx, budget.Key, delay); err != nil {
							t.x.Log.Warn("provider quota cooldown unavailable", "provider", provider)
						}
						stop()
					}
				}
			}
		}
		breakerCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := t.x.Breaker.After(breakerCtx, provider, t.scope.Account, probeToken, status, callErr != nil); err != nil {
			t.x.Log.Warn("provider breaker update unavailable", "provider", provider)
		}
		cancel()
		probeToken = ""
	}
	updateCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finishTx, err := store.BeginTenant(updateCtx, t.x.DB.Pool, t.c.Tenant)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	status := 0
	outcome := "transport_unknown"
	requestID := ""
	if resp != nil {
		status = resp.StatusCode
		outcome = "response_received"
		requestID = resp.Header.Get("Request-Id")
		if provider == "hubspot" {
			requestID = resp.Header.Get("X-HubSpot-Correlation-Id")
		}
		if len(requestID) > 128 {
			requestID = ""
		}
	}
	_, err = finishTx.Exec(updateCtx, `UPDATE business_provider_calls SET response_status=nullif($3,0),outcome=$4,
      provider_request_id=nullif($5,''),finished_at=now() WHERE tenant_id=$1 AND id=$2`, t.c.Tenant, callID, status, outcome, requestID)
	if err != nil {
		_ = finishTx.Rollback(updateCtx)
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	if err = finishTx.Commit(updateCtx); err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	return resp, callErr
}

func retryAfterDelay(value string) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds > 0 {
		if seconds > 3600 {
			seconds = 3600
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		wait := time.Until(when)
		if wait > time.Hour {
			wait = time.Hour
		}
		if wait > 0 {
			return wait
		}
	}
	return 0
}
func (t *recordingTransport) trustedEndpoint(u *url.URL, provider string) bool {
	base := ""
	switch provider {
	case "stripe":
		base = t.x.Endpoints.Stripe
	case "hubspot":
		base = t.x.Endpoints.HubSpot
	case "slack":
		base = t.x.Endpoints.Slack
	}
	v, err := url.Parse(base)
	return err == nil && u.Scheme == v.Scheme && u.Host == v.Host
}
func (x *Executor) finishFailure(c claim, err error) {
	f := providers.SafeClass(err)
	var status string
	delay := f.RetryAfter
	switch f.Outcome {
	case providers.Ambiguous:
		if strings.HasPrefix(f.Class, "slack") {
			status = "unknown"
		} else {
			status = "retry_wait"
		}
	case providers.SafeRetryable:
		status = "retry_wait"
	case providers.BlockedAuth:
		status = "blocked_auth"
	case providers.BlockedConfig:
		status = "blocked_config"
	case providers.Permanent:
		status = "needs_attention"
	default:
		status = "unknown"
	}
	x.finishSafe(c, status, f.Class, delay, "", "", "")
}
func (x *Executor) finishSafe(c claim, status, class string, delay time.Duration, providerRef, contactOrChannel, slackTS string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := x.finish(ctx, c, status, class, delay, providerRef, contactOrChannel, slackTS); err != nil {
		x.Log.Warn("business result not committed; lease recovery required", "action_id", c.Action, "error", err)
	}
}
func (x *Executor) finish(ctx context.Context, c claim, status, class string, delay time.Duration, providerRef, contactOrChannel, slackTS string) error {
	if !business.CanTransition(business.ActionRunning, business.ActionStatus(status), business.TransitionFinish) {
		return errors.New("invalid action result transition")
	}
	tx, err := store.BeginTenant(ctx, x.DB.Pool, c.Tenant)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var kind string
	var customer string
	var stripeID string
	var retryFailures int
	var budgetStarted time.Time
	err = tx.QueryRow(ctx, `SELECT kind,customer_id,stripe_connection_id,retry_failures,budget_started_at FROM business_actions
 WHERE tenant_id=$1 AND id=$2 AND lease_token=$3 AND generation=$4 AND status='running' FOR UPDATE`, c.Tenant, c.Action, c.Token, c.Generation).Scan(&kind, &customer, &stripeID, &retryFailures, &budgetStarted)
	if err != nil {
		return err
	}
	if strings.HasSuffix(class, "_rate_limited") && delay > 0 {
		var connectionID string
		switch {
		case strings.HasPrefix(class, "stripe_"):
			connectionID = stripeID
		case strings.HasPrefix(class, "hubspot_") && kind == "crm_projection":
			err = tx.QueryRow(ctx, `SELECT config->>'hubspot_connection_id' FROM automation_revisions
 WHERE tenant_id=$1 AND id=(SELECT automation_revision_id FROM business_actions WHERE tenant_id=$1 AND id=$2)`, c.Tenant, c.Action).Scan(&connectionID)
		case strings.HasPrefix(class, "slack_") && kind == "slack_invoice_paid":
			err = tx.QueryRow(ctx, `SELECT config->>'slack_connection_id' FROM automation_revisions
 WHERE tenant_id=$1 AND id=(SELECT automation_revision_id FROM business_actions WHERE tenant_id=$1 AND id=$2)`, c.Tenant, c.Action).Scan(&connectionID)
		}
		if err != nil {
			return err
		}
		if connectionID != "" {
			_, err = tx.Exec(ctx, `INSERT INTO business_provider_cooldowns(tenant_id,connection_id,cooldown_until,error_class)
 VALUES($1,$2::uuid,now()+($3::bigint*interval '1 microsecond'),$4)
 ON CONFLICT(tenant_id,connection_id) DO UPDATE SET cooldown_until=greatest(business_provider_cooldowns.cooldown_until,excluded.cooldown_until),
 error_class=excluded.error_class,updated_at=now()`, c.Tenant, connectionID, delay.Microseconds(), class)
			if err != nil {
				return err
			}
		}
	}
	var nextMicros any
	if status == "retry_wait" {
		var now time.Time
		if class == "provider_cooldown" {
			// Redis admission is scheduling, not a failed provider call.
			// Preserve the 24-hour action horizon without exponential retry
			// growth or burning the provider-call budget on a known wait.
			err = tx.QueryRow(ctx, `SELECT now()`).Scan(&now)
			if err != nil {
				return err
			}
			policy := x.Retry
			if policy.MaxAge <= 0 {
				policy.MaxAge = DefaultRetryPolicy().MaxAge
			}
			if !now.Before(budgetStarted.Add(policy.MaxAge)) {
				status, class = "dead", "retry_exhausted"
			} else {
				if delay < 100*time.Millisecond {
					delay = 100 * time.Millisecond
				}
				nextMicros = delay.Microseconds()
			}
		} else {
			var providerCalls int
			err = tx.QueryRow(ctx, `SELECT now(),
 (SELECT count(*) FROM business_provider_calls pc JOIN business_attempts ba ON ba.tenant_id=pc.tenant_id AND ba.id=pc.attempt_id
	 WHERE ba.tenant_id=$1 AND ba.action_id=$2 AND pc.request_started_at >= $3)`, c.Tenant, c.Action, budgetStarted).Scan(&now, &providerCalls)
			if err != nil {
				return err
			}
			policy := x.Retry
			if policy.Base == 0 {
				policy = DefaultRetryPolicy()
			}
			if policy.exhausted(now, budgetStarted, providerCalls) {
				status = "dead"
				class = "retry_exhausted"
			} else {
				nextMicros = policy.next(retryFailures+1, delay).Microseconds()
			}
		}
	}
	contactID, channel, ts := "", "", ""
	if kind == "crm_projection" {
		contactID = contactOrChannel
	} else {
		channel = contactOrChannel
		ts = slackTS
	}
	_, err = tx.Exec(ctx, `UPDATE business_actions SET status=$5,error_class=nullif($6,''),
 next_attempt_at=CASE WHEN $7::bigint IS NULL THEN NULL ELSE now()+($7::bigint*interval '1 microsecond') END,
 lease_token=NULL,lease_until=NULL,updated_at=now(),provider_ref=coalesce(nullif($8,''),provider_ref),
 contact_id=coalesce(nullif($9,''),contact_id),slack_channel=coalesce(nullif($10,''),slack_channel),
 slack_ts=coalesce(nullif($11,''),slack_ts),
 retry_failures=retry_failures+CASE WHEN $5 IN ('retry_wait','dead') AND $6<>'provider_cooldown' THEN 1 ELSE 0 END,
 first_failed_at=CASE WHEN $5='succeeded' THEN first_failed_at ELSE coalesce(first_failed_at,now()) END,
 last_failed_at=CASE WHEN $5='succeeded' THEN last_failed_at ELSE now() END,
 confirmation_source=CASE WHEN $5='succeeded' THEN 'provider' ELSE confirmation_source END
 WHERE tenant_id=$1 AND id=$2 AND lease_token=$3 AND generation=$4`, c.Tenant, c.Action, c.Token, c.Generation, status, class, nextMicros, providerRef, contactID, channel, ts)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE business_attempts SET outcome=$3,finished_at=now() WHERE tenant_id=$1 AND id=$2`, c.Tenant, c.Attempt, status)
	if err != nil {
		return err
	}
	if kind == "crm_projection" {
		var aggregate string
		err = tx.QueryRow(ctx, `SELECT provider_account_id||'|'||mode||'|'||$2 FROM connections WHERE tenant_id=$1 AND id=$3`, c.Tenant, customer, stripeID).Scan(&aggregate)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM crm_aggregate_leases WHERE tenant_id=$1 AND customer_key=$2 AND action_id=$3 AND lease_token=$4`, c.Tenant, aggregate, c.Action, c.Token)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE runs r SET status=summary.status,finished_at=CASE WHEN summary.status='succeeded' THEN now() ELSE NULL END
 FROM (SELECT l.run_id,CASE
  WHEN bool_or(a.status='unknown') THEN 'unknown'
  WHEN bool_or(a.status='needs_attention') THEN 'needs_attention'
  WHEN bool_or(a.status='dead') THEN 'dead'
  WHEN bool_or(a.status='blocked_auth') THEN 'blocked_auth'
  WHEN bool_or(a.status='blocked_config') THEN 'blocked_config'
  WHEN bool_or(a.status='paused') THEN 'paused'
  WHEN bool_and(a.status='succeeded') THEN 'succeeded'
  WHEN bool_or(a.status='retry_wait') THEN 'retry_wait'
  ELSE 'running' END AS status
  FROM business_run_actions l JOIN business_actions a ON a.tenant_id=l.tenant_id AND a.id=l.action_id
  WHERE l.tenant_id=$1 AND l.run_id IN (SELECT run_id FROM business_run_actions WHERE tenant_id=$1 AND action_id=$2)
  GROUP BY l.run_id) summary WHERE r.tenant_id=$1 AND r.id=summary.run_id`, c.Tenant, c.Action)
	if err != nil {
		return err
	}
	err = tx.Commit(ctx)
	if err == nil {
		telemetry.ActionOutcome(ctx, kind, status, class)
		if status == "blocked_auth" {
			provider := "hubspot"
			switch {
			case strings.HasPrefix(class, "stripe"):
				provider = "stripe"
			case strings.HasPrefix(class, "slack"), kind == "slack_invoice_paid":
				provider = "slack"
			}
			telemetry.Event(ctx, "blocked_auth", provider)
		}
	}
	return err
}
