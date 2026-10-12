//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/delivery"
	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
	"github.com/redis/go-redis/v9"
)

func startQueuedBusiness(t *testing.T, v *providerWorld) context.CancelFunc {
	return startQueuedBusinessPolicy(t, v, execution.RetryPolicy{})
}
func startQueuedBusinessPolicy(t *testing.T, v *providerWorld, policy execution.RetryPolicy) context.CancelFunc {
	t.Helper()
	stripeHTTP := httptest.NewServer(http.HandlerFunc(v.stripe))
	hubspotHTTP := httptest.NewServer(http.HandlerFunc(v.hubspot))
	slackHTTP := httptest.NewServer(http.HandlerFunc(v.slack))
	ctx, cancel := context.WithCancel(context.Background())
	w, nc, err := delivery.New(ctx, db, natsURL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	w.Business = &execution.Executor{DB: db, Box: box, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Retry:     policy,
		Endpoints: providers.Endpoints{Stripe: stripeHTTP.URL, HubSpot: hubspotHTTP.URL, Slack: slackHTTP.URL, Simulation: true}}
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx); nc.Close() }()
	t.Cleanup(func() { cancel(); <-done; stripeHTTP.Close(); hubspotHTTP.Close(); slackHTTP.Close() })
	return cancel
}

func TestMission04QueuedBusinessAndPointerReplay(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	startQueuedBusiness(t, v)
	id := deliverBusiness(t, f, v, "invoice.paid")
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "succeeded" })
	var outbox, published, claims int
	if err := mig.QueryRow(context.Background(), `SELECT count(*),count(*) FILTER (WHERE published_at IS NOT NULL)
 FROM business_action_outbox WHERE tenant_id=$1`, f.tenantID).Scan(&outbox, &published); err != nil {
		t.Fatal(err)
	}
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_attempts WHERE tenant_id=$1`, f.tenantID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if outbox != 2 || published != 2 || claims != 2 {
		t.Fatalf("outbox=%d published=%d claims=%d", outbox, published, claims)
	}
	var actionID string
	var generation int64
	if err := mig.QueryRow(context.Background(), `SELECT id,dispatch_generation FROM business_actions
 WHERE tenant_id=$1 AND kind='crm_projection'`, f.tenantID).Scan(&actionID, &generation); err != nil {
		t.Fatal(err)
	}
	broker, nc, err := delivery.New(context.Background(), db, natsURL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	consumer, err := broker.JS.Consumer(context.Background(), "RELAYDESK_ACTIONS", "EXECUTOR_V1")
	if err != nil {
		t.Fatal(err)
	}
	before, err := consumer.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	replay, _ := json.Marshal(delivery.ActionEnvelope{Version: 1, TenantID: f.tenantID,
		ActionID: actionID, Generation: generation})
	if _, err = broker.JS.Publish(context.Background(), "relaydesk.action.v1", replay); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		info, infoErr := consumer.Info(context.Background())
		return infoErr == nil && info.AckFloor.Consumer > before.AckFloor.Consumer
	})
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_attempts
 WHERE tenant_id=$1`, f.tenantID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 2 {
		t.Fatalf("stale broker pointer created %d attempts", claims)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.hubspotWrites != 1 || v.slackMessages != 1 {
		t.Fatalf("duplicate effect: HubSpot=%d Slack=%d", v.hubspotWrites, v.slackMessages)
	}
}

func TestMission04LostPointerRepair(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	id := deliverBusiness(t, f, v, "customer.subscription.updated")
	await(t, func() bool {
		var n int
		_ = mig.QueryRow(context.Background(), `SELECT count(*) FROM business_actions WHERE tenant_id=$1`, f.tenantID).Scan(&n)
		return n == 1
	})
	// Start at the queued-pointer boundary this test intends to repair. The
	// fixed CRM collection window and other pending test tenants must not make
	// the first global scheduler batch an implicit part of this fixture.
	if _, err := mig.Exec(context.Background(), `UPDATE business_actions SET created_at=now()-interval '1 hour'
 WHERE tenant_id=$1 AND status='pending'`, f.tenantID); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		if _, queueErr := db.QueueBusiness(context.Background()); queueErr != nil {
			return false
		}
		var generation int64
		_ = mig.QueryRow(context.Background(), `SELECT dispatch_generation FROM business_actions
 WHERE tenant_id=$1`, f.tenantID).Scan(&generation)
		return generation == 1
	})
	var actionID string
	var generation int64
	if err := mig.QueryRow(context.Background(), `SELECT id,dispatch_generation FROM business_actions WHERE tenant_id=$1`, f.tenantID).Scan(&actionID, &generation); err != nil {
		t.Fatal(err)
	}
	if generation != 1 {
		t.Fatalf("initial generation %d", generation)
	}
	if _, err := mig.Exec(context.Background(), `UPDATE business_actions SET queued_at=now()-interval '1 minute' WHERE id=$1`, actionID); err != nil {
		t.Fatal(err)
	}
	if n, err := db.RepairBusiness(context.Background()); err != nil || n < 1 {
		t.Fatalf("repair %d: %v", n, err)
	}
	var current int64
	var superseded int
	if err := mig.QueryRow(context.Background(), `SELECT dispatch_generation FROM business_actions WHERE id=$1`, actionID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_action_outbox WHERE action_id=$1 AND superseded_at IS NOT NULL`, actionID).Scan(&superseded); err != nil {
		t.Fatal(err)
	}
	if current != 2 || superseded != 1 {
		t.Fatalf("repair generation=%d superseded=%d", current, superseded)
	}
	startQueuedBusiness(t, v)
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "succeeded" })
	v.mu.Lock()
	writes := v.hubspotWrites
	v.mu.Unlock()
	if writes != 1 {
		t.Fatalf("repaired pointer caused %d writes", writes)
	}
}

func TestMission04UnknownOperatorDecision(t *testing.T) {
	v := newWorld()
	v.slackLost = true
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	id := deliverBusiness(t, f, v, "invoice.paid")
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "unknown" })
	var actionID string
	var generation int64
	if err := mig.QueryRow(context.Background(), `SELECT id,generation FROM business_actions
	 WHERE tenant_id=$1 AND kind='slack_invoice_paid'`, f.tenantID).Scan(&actionID, &generation); err != nil {
		t.Fatal(err)
	}
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	owner := login(t, app, "recovery-owner-"+randomHex(4), nil)
	ownerID, csrf := me(t, app, owner)
	viewer := login(t, app, "recovery-viewer-"+randomHex(4), nil)
	viewerID, viewerCSRF := me(t, app, viewer)
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner'),($1,$3,'viewer')`, f.tenantID, ownerID, viewerID); err != nil {
		t.Fatal(err)
	}
	path := app.server.URL + "/v1/tenants/" + f.tenantID + "/actions/" + actionID
	body := map[string]any{"decision": "confirm_unknown", "reason": "Observed in destination channel", "expected_generation": generation,
		"external_channel": v.channel, "external_ts": "1700000000.000001", "idempotency_key": "confirm-" + randomHex(4)}
	closeResponse(t, request(t, app.client, "POST", path+"/retry", owner, csrf, body), 400) // ordinary retry cannot accept a resolve decision
	closeResponse(t, request(t, app.client, "POST", path+"/resolve", viewer, viewerCSRF, body), 403)
	other := seed(t, false)
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, other.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	otherPath := app.server.URL + "/v1/tenants/" + other.tenantID + "/actions/" + actionID + "/resolve"
	closeResponse(t, request(t, app.client, "POST", otherPath, owner, csrf, body), 404)
	response := request(t, app.client, "POST", path+"/resolve", owner, csrf, body)
	var first struct {
		DecisionID string `json:"decision_id"`
		Status     string `json:"status"`
	}
	if err := json.NewDecoder(response.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 || first.Status != "succeeded" {
		t.Fatalf("resolution status=%d result=%+v", response.StatusCode, first)
	}
	response = request(t, app.client, "POST", path+"/resolve", owner, csrf, body)
	var repeated struct {
		DecisionID string `json:"decision_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&repeated); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 || repeated.DecisionID != first.DecisionID {
		t.Fatalf("non-idempotent result: %d %+v", response.StatusCode, repeated)
	}
	closeResponse(t, request(t, app.client, "POST", path+"/resolve", owner, csrf, map[string]any{"decision": "risk_retry", "reason": "stale", "expected_generation": generation, "acknowledge_duplicate_risk": true}), 409)
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "succeeded" })
	var source, ts string
	if err := mig.QueryRow(context.Background(), `SELECT confirmation_source,slack_ts FROM business_actions WHERE id=$1`, actionID).Scan(&source, &ts); err != nil {
		t.Fatal(err)
	}
	if source != "operator" || ts != "1700000000.000001" {
		t.Fatalf("confirmation source=%s ts=%s", source, ts)
	}
	v.mu.Lock()
	sends := v.slackMessages
	v.mu.Unlock()
	if sends != 1 {
		t.Fatalf("unknown resolution resent Slack: %d", sends)
	}
}

func TestMission04ManualRetryDoesNotRepeatCRM(t *testing.T) {
	v := newWorld()
	v.slackError = "channel_not_found"
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	id := deliverBusiness(t, f, v, "invoice.paid")
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "needs_attention" })
	var actionID string
	var generation int64
	if err := mig.QueryRow(context.Background(), `SELECT id,generation FROM business_actions WHERE tenant_id=$1 AND kind='slack_invoice_paid'`, f.tenantID).Scan(&actionID, &generation); err != nil {
		t.Fatal(err)
	}
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	owner := login(t, app, "retry-owner-"+randomHex(4), nil)
	ownerID, csrf := me(t, app, owner)
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	v.slackError = ""
	v.mu.Unlock()
	path := app.server.URL + "/v1/tenants/" + f.tenantID + "/actions/" + actionID + "/retry"
	res := request(t, app.client, "POST", path, owner, csrf, map[string]any{"reason": "Channel membership repaired", "expected_generation": generation, "idempotency_key": "retry-" + randomHex(4)})
	closeResponse(t, res, 200)
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "succeeded" })
	v.mu.Lock()
	crm, sends := v.hubspotWrites, v.slackMessages
	v.mu.Unlock()
	if crm != 1 || sends != 1 {
		t.Fatalf("manual retry repeated CRM or Slack: %d/%d", crm, sends)
	}
}

func TestMission04QueuedOutageRetryAndRestart(t *testing.T) {
	v := newWorld()
	v.hubspotUnavailable = true
	f, _ := seedBusiness(t, v)
	cancel := startQueuedBusiness(t, v)
	id := deliverBusiness(t, f, v, "invoice.paid")
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "retry_wait" })
	var next time.Time
	var crmStatus, slackStatus string
	if err := mig.QueryRow(context.Background(), `SELECT c.status,s.status,c.next_attempt_at FROM business_actions c
 JOIN business_actions s ON s.tenant_id=c.tenant_id AND s.depends_on=c.id
 WHERE c.tenant_id=$1 AND c.kind='crm_projection'`, f.tenantID).Scan(&crmStatus, &slackStatus, &next); err != nil {
		t.Fatal(err)
	}
	if crmStatus != "retry_wait" || slackStatus != "pending" || next.IsZero() {
		t.Fatalf("retry state %s/%s due=%v", crmStatus, slackStatus, next)
	}
	cancel()
	v.mu.Lock()
	v.hubspotUnavailable = false
	v.mu.Unlock()
	startQueuedBusiness(t, v)
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "succeeded" })
	v.mu.Lock()
	writes, sends := v.hubspotWrites, v.slackMessages
	v.mu.Unlock()
	if writes != 1 || sends != 1 {
		t.Fatalf("after restart effects %d/%d", writes, sends)
	}
	var attempts int
	if err := mig.QueryRow(context.Background(), `SELECT attempts FROM business_actions WHERE tenant_id=$1 AND kind='crm_projection'`, f.tenantID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts < 2 {
		t.Fatalf("no durable retry after restart: %d", attempts)
	}
}

func TestMission04RetryOutboxSurvivesNATSOutage(t *testing.T) {
	v := newWorld()
	v.hubspotUnavailable = true
	f, _ := seedBusiness(t, v)
	cancel := startQueuedBusinessPolicy(t, v, execution.RetryPolicy{Base: 5 * time.Second,
		Cap: 5 * time.Second, Random: func() float64 { return 0.99 }})
	id := deliverBusiness(t, f, v, "customer.subscription.updated")
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "retry_wait" })
	cancel()
	compose(t, "stop", "nats")
	defer compose(t, "start", "nats")
	if _, err := mig.Exec(context.Background(), `UPDATE business_actions SET next_attempt_at=now()-interval '1 second'
 WHERE tenant_id=$1 AND status='retry_wait'`, f.tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.QueueBusiness(context.Background()); err != nil {
		t.Fatalf("durable retry scheduling during NATS outage: %v", err)
	}
	var queued int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_actions
 WHERE tenant_id=$1 AND status='queued'`, f.tenantID).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("tenant retry scheduled during NATS outage: queued=%d err=%v", queued, err)
	}
	var pending int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_action_outbox
 WHERE tenant_id=$1 AND published_at IS NULL AND superseded_at IS NULL`, f.tenantID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("unpublished retry pointers=%d", pending)
	}
	v.mu.Lock()
	v.hubspotUnavailable = false
	v.mu.Unlock()
	compose(t, "start", "nats")
	startQueuedBusiness(t, v)
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "succeeded" })
	v.mu.Lock()
	writes := v.hubspotWrites
	v.mu.Unlock()
	if writes != 1 {
		t.Fatalf("Contact writes after broker recovery=%d", writes)
	}
}

func TestMission04StaleWorkerCannotCommitLateCRMResult(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	cancelOld := startExecutorWithFault(t, v, func(stage, tenant, _ string) bool {
		if stage != "after_crm_confirmed" || tenant != f.tenantID {
			return false
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return false
	})
	id := deliverBusiness(t, f, v, "customer.subscription.updated")
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("old worker did not reach the late-commit boundary")
	}
	cancelOld()
	var actionID string
	if err := mig.QueryRow(context.Background(), `SELECT id FROM business_actions
 WHERE tenant_id=$1 AND kind='crm_projection'`, f.tenantID).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Exec(context.Background(), `UPDATE business_actions
 SET lease_until=now()-interval '1 second' WHERE id=$1 AND status='running'`, actionID); err != nil {
		t.Fatal(err)
	}
	if n, err := db.RepairBusiness(context.Background()); err != nil || n < 1 {
		t.Fatalf("expired claim was not repaired: %d %v", n, err)
	}
	startQueuedBusiness(t, v)
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "succeeded" })
	releaseOnce.Do(func() { close(release) })
	var status, oldOutcome string
	if err := mig.QueryRow(context.Background(), `SELECT status FROM business_actions WHERE id=$1`, actionID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := mig.QueryRow(context.Background(), `SELECT outcome FROM business_attempts
 WHERE action_id=$1 ORDER BY attempt_number LIMIT 1`, actionID).Scan(&oldOutcome); err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	writes := v.hubspotWrites
	v.mu.Unlock()
	if status != "succeeded" || oldOutcome != "lease_expired" || writes != 1 {
		t.Fatalf("late claim changed result: status=%s old=%s writes=%d", status, oldOutcome, writes)
	}
}

func TestMission04ProviderAttemptBudgetDLQ(t *testing.T) {
	v := newWorld()
	v.hubspotUnavailable = true
	f, _ := seedBusiness(t, v)
	startQueuedBusinessPolicy(t, v, execution.RetryPolicy{Base: 100 * time.Millisecond, Cap: 100 * time.Millisecond,
		MaxAge: time.Hour, MaxProviderCalls: 8, Random: func() float64 { return 0.99 }})
	id := deliverBusiness(t, f, v, "customer.subscription.updated")
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "dead" })
	var status, class, effect string
	var first, last time.Time
	var attempts int
	if err := mig.QueryRow(context.Background(), `SELECT status,error_class,effect_key,first_failed_at,last_failed_at,attempts
 FROM business_actions WHERE tenant_id=$1`, f.tenantID).Scan(&status, &class, &effect, &first, &last, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "dead" || class != "retry_exhausted" || effect == "" || first.IsZero() || last.IsZero() || attempts < 2 {
		t.Fatalf("DLQ entry invalid: %s %s attempts=%d", status, class, attempts)
	}
	var outbox int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_action_outbox WHERE tenant_id=$1`, f.tenantID).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if outbox < 2 {
		t.Fatalf("retry was not redispatched: %d", outbox)
	}
}

func TestMission04QueuedPauseResume(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	id := deliverBusiness(t, f, v, "customer.subscription.updated")
	await(t, func() bool {
		var n int
		_ = mig.QueryRow(context.Background(), `SELECT count(*) FROM business_actions WHERE tenant_id=$1`, f.tenantID).Scan(&n)
		return n == 1
	})
	if _, err := db.QueueBusiness(context.Background()); err != nil {
		t.Fatal(err)
	}
	var actionID string
	if err := mig.QueryRow(context.Background(), `SELECT id FROM business_actions WHERE tenant_id=$1`, f.tenantID).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	// A verified member session is required even for the pause path.
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	owner := login(t, app, "pause-owner-"+randomHex(4), nil)
	ownerID, csrf := me(t, app, owner)
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	path := app.server.URL + "/v1/tenants/" + f.tenantID + "/billing-sync/" + f.connectionID + "/pause"
	closeResponse(t, request(t, app.client, "POST", path, owner, csrf, map[string]bool{"paused": true}), 204)
	var paused string
	if err := mig.QueryRow(context.Background(), `SELECT status FROM business_actions WHERE id=$1`, actionID).Scan(&paused); err != nil {
		t.Fatal(err)
	}
	if paused != "paused" {
		t.Fatalf("queued action was not paused: %s", paused)
	}
	startQueuedBusiness(t, v)
	time.Sleep(1200 * time.Millisecond)
	v.mu.Lock()
	writes := v.hubspotWrites
	v.mu.Unlock()
	if writes != 0 {
		t.Fatalf("paused action wrote %d Contacts", writes)
	}
	closeResponse(t, request(t, app.client, "POST", path, owner, csrf, map[string]bool{"paused": false}), 204)
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "succeeded" })
}

func TestMission04RevokedQueuedConnection(t *testing.T) {
	v := newWorld()
	f, cfg := seedBusiness(t, v)
	id := deliverBusiness(t, f, v, "customer.subscription.updated")
	await(t, func() bool {
		var n int
		_ = mig.QueryRow(context.Background(),
			`SELECT count(*) FROM business_actions WHERE tenant_id=$1`, f.tenantID).Scan(&n)
		return n == 1
	})
	if _, err := mig.Exec(context.Background(), `UPDATE connections SET status='disconnected',generation=generation+1
 WHERE tenant_id=$1 AND id=$2`, f.tenantID, cfg.HubSpotConnectionID); err != nil {
		t.Fatal(err)
	}
	startQueuedBusiness(t, v)
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "blocked_config" })
	v.mu.Lock()
	writes := v.hubspotWrites
	v.mu.Unlock()
	if writes != 0 {
		t.Fatalf("revoked connection wrote Contact: %d", writes)
	}
}

func TestMission04RiskAcknowledgedSlackResend(t *testing.T) {
	v := newWorld()
	v.slackLost = true
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	id := deliverBusiness(t, f, v, "invoice.paid")
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "unknown" })
	var actionID, effect string
	var generation int64
	if err := mig.QueryRow(context.Background(), `SELECT id,effect_key,generation FROM business_actions
 WHERE tenant_id=$1 AND kind='slack_invoice_paid'`, f.tenantID).Scan(&actionID, &effect, &generation); err != nil {
		t.Fatal(err)
	}
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	owner := login(t, app, "risk-owner-"+randomHex(4), nil)
	ownerID, csrf := me(t, app, owner)
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	path := app.server.URL + "/v1/tenants/" + f.tenantID + "/actions/" + actionID + "/resolve"
	base := map[string]any{"decision": "risk_retry", "reason": "Reviewed channel and accept possible duplicate",
		"expected_generation": generation}
	closeResponse(t, request(t, app.client, "POST", path, owner, csrf, base), 400)
	base["acknowledge_duplicate_risk"] = true
	closeResponse(t, request(t, app.client, "POST", path, owner, csrf, base), 200)
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "succeeded" })
	var currentEffect string
	var decisions int
	if err := mig.QueryRow(context.Background(), `SELECT effect_key FROM business_actions WHERE id=$1`, actionID).Scan(&currentEffect); err != nil {
		t.Fatal(err)
	}
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_action_decisions WHERE action_id=$1 AND duplicate_risk_ack`, actionID).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	messages, crm := v.slackMessages, v.hubspotWrites
	v.mu.Unlock()
	if currentEffect != effect || decisions != 1 || messages != 2 || crm != 1 {
		t.Fatalf("risk replay effect=%t decisions=%d Slack=%d CRM=%d", currentEffect == effect, decisions, messages, crm)
	}
}

func TestMission04ConcurrentRecoveryCAS(t *testing.T) {
	v := newWorld()
	v.slackError = "channel_not_found"
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	id := deliverBusiness(t, f, v, "invoice.paid")
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "needs_attention" })
	var actionID string
	var generation int64
	if err := mig.QueryRow(context.Background(), `SELECT id,generation FROM business_actions WHERE tenant_id=$1 AND kind='slack_invoice_paid'`, f.tenantID).Scan(&actionID, &generation); err != nil {
		t.Fatal(err)
	}
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	owner := login(t, app, "concurrent-owner-"+randomHex(4), nil)
	ownerID, csrf := me(t, app, owner)
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	path := app.server.URL + "/v1/tenants/" + f.tenantID + "/actions/" + actionID + "/retry"
	type result struct {
		status int
		err    error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{"reason": "Connection repaired", "expected_generation": generation,
				"idempotency_key": []string{"recovery-a", "recovery-b"}[i]})
			req, err := http.NewRequest("POST", path, bytes.NewReader(body))
			if err != nil {
				results <- result{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-CSRF-Token", csrf)
			req.AddCookie(owner)
			resp, err := app.client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			results <- result{status: resp.StatusCode}
		}(i)
	}
	wg.Wait()
	close(results)
	good, conflict := 0, 0
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.status == 200 {
			good++
		}
		if r.status == 409 {
			conflict++
		}
	}
	if good != 1 || conflict != 1 {
		t.Fatalf("concurrent outcomes success=%d conflict=%d", good, conflict)
	}
	var decisions int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_action_decisions WHERE action_id=$1`, actionID).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 {
		t.Fatalf("concurrent decisions=%d", decisions)
	}
}

func TestMission04RedisWriteQuotaFailClosed(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:56379"})
	defer func() { _ = client.Close() }()
	limiter := &execution.RedisWriteLimiter{Client: client, Limit: 2, Window: 5 * time.Second}
	ctx := context.Background()
	tenant, connection := "quota-test-"+randomHex(8), "connection"
	for i := 0; i < 2; i++ {
		if wait, err := limiter.Allow(ctx, tenant, connection); err != nil || wait != 0 {
			t.Fatalf("allowed call %d: %s %v", i, wait, err)
		}
	}
	if wait, err := limiter.Allow(ctx, tenant, connection); err != nil || wait <= 0 {
		t.Fatalf("quota not enforced: %s %v", wait, err)
	}
	_ = client.Close()
	if _, err := limiter.Allow(ctx, tenant, connection); err == nil {
		t.Fatal("Redis failure was not closed")
	}
}
