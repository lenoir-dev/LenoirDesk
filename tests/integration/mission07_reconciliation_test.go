//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
	"github.com/lenoir-dev/LenoirDesk/internal/reconciliation"
)

func TestMission07EventDiscoveryKnownDriftAndCoverageGap(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	eventID := "evt_" + randomHex(10)
	eventRaw := billingPayload(eventID, "invoice.paid", v.customer, v.invoice, v.subscription)
	stripeHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/events" {
			if req.Method != http.MethodGet || req.URL.Query().Get("limit") != "25" || req.URL.Query().Get("created[gte]") == "" ||
				len(req.URL.Query()["types[]"]) != 5 || !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
				http.Error(w, "invalid discovery query", http.StatusBadRequest)
				return
			}
			jsonResponse(w, map[string]any{"data": []json.RawMessage{eventRaw}, "has_more": false})
			return
		}
		v.stripe(w, req)
	}))
	defer stripeHTTP.Close()
	r := &reconciliation.Reconciler{DB: db, Box: box, Endpoints: providers.Endpoints{
		Stripe: stripeHTTP.URL, HubSpot: stripeHTTP.URL, Slack: stripeHTTP.URL, Simulation: true}}
	ctx := context.Background()
	_, err := mig.Exec(ctx, `INSERT INTO reconcile_checkpoints(tenant_id,stripe_connection_id,kind,window_start,window_end,next_run_at)
	VALUES($1,$2,'events',now()-interval '10 minutes',now()+interval '1 minute',now()-interval '1 minute'),
	($1,$2,'known',now()-interval '10 minutes',now()+interval '1 minute',now())`, f.tenantID, f.connectionID)
	if err != nil {
		t.Fatal(err)
	}
	worked, err := r.StepTenant(ctx, f.tenantID)
	if err != nil || !worked {
		t.Fatalf("event discovery: claimed=%t err=%v", worked, err)
	}
	waitBusinessStatus(t, f.tenantID, eventID, "succeeded")
	var source, checkpoint string
	if err = mig.QueryRow(ctx, `SELECT source FROM webhook_events WHERE tenant_id=$1 AND event_id=$2`, f.tenantID, eventID).Scan(&source); err != nil || source != "event_discovery" {
		t.Fatalf("discovered source=%q err=%v", source, err)
	}
	if err = mig.QueryRow(ctx, `SELECT coalesce(last_complete_at::text,'') FROM reconcile_checkpoints
 WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind='events'`, f.tenantID, f.connectionID).Scan(&checkpoint); err != nil || checkpoint == "" {
		t.Fatalf("event checkpoint missing: %v", err)
	}
	writes, messages := externalCounts(v)
	if writes != 1 || messages != 0 {
		t.Fatalf("historical discovery effects CRM=%d Slack=%d", writes, messages)
	}
	// ADR 0003: a signed duplicate cannot promote the suppressed effect.
	if status := post(t, f, eventRaw, time.Now(), f.secret); status != http.StatusOK {
		t.Fatalf("signed duplicate after discovery status=%d", status)
	}
	if writes, messages := externalCounts(v); writes != 1 || messages != 0 {
		t.Fatalf("signed duplicate promoted suppressed effect CRM=%d Slack=%d", writes, messages)
	}
	// The next known-entity pass uses the ordinary executor to compare current
	// Stripe state with HubSpot managed fields and repairs deliberate drift.
	v.mu.Lock()
	for _, contact := range v.contacts {
		contact["relaydesk_projection_hash"] = "stale"
	}
	v.mu.Unlock()
	worked, err = r.StepTenant(ctx, f.tenantID)
	if err != nil || !worked {
		t.Fatalf("known-state scan: claimed=%t err=%v", worked, err)
	}
	var knownID string
	if err = mig.QueryRow(ctx, `SELECT event_id FROM webhook_events WHERE tenant_id=$1 AND source='known_state' ORDER BY received_at DESC LIMIT 1`, f.tenantID).Scan(&knownID); err != nil {
		var gap string
		_ = mig.QueryRow(ctx, `SELECT coalesce(gap_code,'') FROM reconcile_checkpoints WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind='known'`, f.tenantID, f.connectionID).Scan(&gap)
		t.Fatalf("known-state work missing: %v; gap=%s", err, gap)
	}
	waitBusinessStatus(t, f.tenantID, knownID, "succeeded")
	writes, messages = externalCounts(v)
	if writes != 2 || messages != 0 {
		t.Fatalf("drift repair effects CRM=%d Slack=%d", writes, messages)
	}
	// Re-reading the same Stripe event must not create another effect.
	_, err = mig.Exec(ctx, `UPDATE reconcile_checkpoints SET window_start=now()-interval '10 minutes',
 window_end=now()+interval '1 minute',cursor='',next_run_at=now()-interval '1 minute'
 WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind='events'`, f.tenantID, f.connectionID)
	if err != nil {
		t.Fatal(err)
	}
	worked, err = r.StepTenant(ctx, f.tenantID)
	if err != nil || !worked {
		t.Fatalf("overlap scan: claimed=%t err=%v", worked, err)
	}
	var eventRows int
	if err = mig.QueryRow(ctx, `SELECT count(*) FROM webhook_events WHERE tenant_id=$1 AND event_id=$2`, f.tenantID, eventID).Scan(&eventRows); err != nil || eventRows != 1 {
		t.Fatalf("duplicate event rows=%d err=%v", eventRows, err)
	}
	_, err = mig.Exec(ctx, `UPDATE reconcile_checkpoints SET window_start=now()-interval '31 days',
 window_end=now()-interval '30 days',cursor='',next_run_at=now()-interval '1 minute'
 WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind='events'`, f.tenantID, f.connectionID)
	if err != nil {
		t.Fatal(err)
	}
	worked, err = r.StepTenant(ctx, f.tenantID)
	if err != nil || !worked {
		t.Fatalf("gap detection: claimed=%t err=%v", worked, err)
	}
	var gap string
	if err = mig.QueryRow(ctx, `SELECT coalesce(gap_code,'') FROM reconcile_checkpoints WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind='events'`, f.tenantID, f.connectionID).Scan(&gap); err != nil || gap != "provider_history_gap" {
		t.Fatalf("coverage gap=%q err=%v", gap, err)
	}
}

func TestMission08EndiveEventDiscoveryPreservesWebhookVersionBoundary(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	eventID := "evt_" + randomHex(10)
	var event map[string]any
	if err := json.Unmarshal(billingPayload(eventID, "invoice.paid", v.customer, v.invoice, v.subscription), &event); err != nil {
		t.Fatal(err)
	}
	event["api_version"] = "2026-09-30.endive"
	raw, _ := json.Marshal(event)
	stripeHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/events" {
			jsonResponse(w, map[string]any{"object": "list", "data": []json.RawMessage{raw}, "has_more": false})
			return
		}
		v.stripe(w, req)
	}))
	defer stripeHTTP.Close()
	r := &reconciliation.Reconciler{DB: db, Box: box, Endpoints: providers.Endpoints{
		Stripe: stripeHTTP.URL, HubSpot: stripeHTTP.URL, Slack: stripeHTTP.URL, Simulation: true}}
	ctx := context.Background()
	if _, err := mig.Exec(ctx, `INSERT INTO reconcile_checkpoints(tenant_id,stripe_connection_id,kind,window_start,window_end,next_run_at)
 VALUES($1,$2,'events',now()-interval '10 minutes',now()+interval '1 minute',now())`, f.tenantID, f.connectionID); err != nil {
		t.Fatal(err)
	}
	if worked, err := r.StepTenant(ctx, f.tenantID); err != nil || !worked {
		t.Fatalf("Endive discovery worked=%t err=%v", worked, err)
	}
	waitBusinessStatus(t, f.tenantID, eventID, "succeeded")
	var version string
	if err := mig.QueryRow(ctx, `SELECT event_api_version FROM webhook_events WHERE tenant_id=$1 AND event_id=$2`,
		f.tenantID, eventID).Scan(&version); err != nil || version != "2026-09-30.endive" {
		t.Fatalf("discovered version=%q err=%v", version, err)
	}
	event["id"] = "evt_" + randomHex(10)
	signedRaw, _ := json.Marshal(event)
	if status := post(t, f, signedRaw, time.Now(), f.secret); status != http.StatusAccepted {
		t.Fatalf("signed Endive webhook acceptance=%d", status)
	}
	var classification string
	if err := mig.QueryRow(ctx, `SELECT classification FROM webhook_events WHERE tenant_id=$1 AND event_id=$2`,
		f.tenantID, event["id"]).Scan(&classification); err != nil || classification != "ignored" {
		t.Fatalf("signed Endive webhook classification=%q err=%v", classification, err)
	}
	if writes, messages := externalCounts(v); writes != 1 || messages != 0 {
		t.Fatalf("discovered event duplicated effects: CRM=%d Slack=%d", writes, messages)
	}
}

func TestMission07CheckpointCrashRescanAndConcurrentClaim(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	eventID := "evt_" + randomHex(10)
	raw := billingPayload(eventID, "invoice.paid", v.customer, v.invoice, v.subscription)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	stripeHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/events" {
			once.Do(func() { close(entered) })
			<-release
			jsonResponse(w, map[string]any{"data": []json.RawMessage{raw}, "has_more": false})
			return
		}
		v.stripe(w, req)
	}))
	defer stripeHTTP.Close()
	ctx := context.Background()
	if _, err := mig.Exec(ctx, `INSERT INTO reconcile_checkpoints(tenant_id,stripe_connection_id,kind,window_start,window_end,next_run_at)
	 VALUES($1,$2,'events',now()-interval '10 minutes',now()+interval '1 minute',now())`, f.tenantID, f.connectionID); err != nil {
		t.Fatal(err)
	}
	endpoints := providers.Endpoints{Stripe: stripeHTTP.URL, HubSpot: stripeHTTP.URL, Slack: stripeHTTP.URL, Simulation: true}
	first := &reconciliation.Reconciler{DB: db, Box: box, Endpoints: endpoints,
		Fault: func(stage string) bool { return stage == "before_checkpoint" }}
	result := make(chan error, 1)
	go func() { _, err := first.StepTenant(ctx, f.tenantID); result <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first claim never reached provider")
	}
	second := &reconciliation.Reconciler{DB: db, Box: box, Endpoints: endpoints}
	secondCtx, secondCancel := context.WithTimeout(ctx, 2*time.Second)
	worked, err := second.StepTenant(secondCtx, f.tenantID)
	secondCancel()
	if err != nil || worked {
		close(release)
		t.Fatalf("concurrent claim duplicated work: worked=%t err=%v", worked, err)
	}
	close(release)
	if err = <-result; err == nil {
		t.Fatal("crash boundary did not fire")
	}
	var cursor string
	var lastComplete *time.Time
	if err = mig.QueryRow(ctx, `SELECT cursor,last_complete_at FROM reconcile_checkpoints
	 WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind='events'`, f.tenantID, f.connectionID).
		Scan(&cursor, &lastComplete); err != nil || cursor != "" || lastComplete != nil {
		t.Fatalf("checkpoint advanced before commit: cursor=%q complete=%v err=%v", cursor, lastComplete, err)
	}
	if _, err = mig.Exec(ctx, `UPDATE reconcile_checkpoints SET lease_until=now()-interval '1 second'
	 WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind='events'`, f.tenantID, f.connectionID); err != nil {
		t.Fatal(err)
	}
	worked, err = second.StepTenant(ctx, f.tenantID)
	if err != nil || !worked {
		t.Fatalf("restart rescan failed: worked=%t err=%v", worked, err)
	}
	var count int
	if err = mig.QueryRow(ctx, `SELECT count(*) FROM webhook_events WHERE tenant_id=$1 AND event_id=$2`, f.tenantID, eventID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rescan duplicated event count=%d err=%v", count, err)
	}
	writes, messages := externalCounts(v)
	if writes > 1 || messages != 0 {
		t.Fatalf("rescan duplicated external effects CRM=%d Slack=%d", writes, messages)
	}
}

func TestMission07OldSignedInvoiceCannotReplaySlack(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	eventID := "evt_" + randomHex(10)
	var event map[string]any
	if err := json.Unmarshal(billingPayload(eventID, "invoice.paid", v.customer, v.invoice, v.subscription), &event); err != nil {
		t.Fatal(err)
	}
	event["created"] = time.Now().Add(-91 * 24 * time.Hour).Unix()
	raw, _ := json.Marshal(event)
	if status := post(t, f, raw, time.Now(), f.secret); status != http.StatusAccepted {
		t.Fatalf("signed old delivery status=%d", status)
	}
	var source string
	if err := mig.QueryRow(context.Background(), `SELECT source FROM webhook_events WHERE tenant_id=$1 AND event_id=$2`, f.tenantID, eventID).Scan(&source); err != nil || source != "historical" {
		t.Fatalf("old event source=%q err=%v", source, err)
	}
	// Even if planning is retried after archival, a historical fact cannot
	// acquire a fresh Slack effect identity.
	await(t, func() bool {
		var n int
		_ = mig.QueryRow(context.Background(), `SELECT count(*) FROM planning_jobs WHERE tenant_id=$1
		 AND event_row_id=(SELECT id FROM webhook_events WHERE tenant_id=$1 AND event_id=$2)
		 AND status='planned'`, f.tenantID, eventID).Scan(&n)
		return n == 1
	})
	var slack int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_actions WHERE tenant_id=$1 AND kind='slack_invoice_paid'`, f.tenantID).Scan(&slack); err != nil || slack != 0 {
		t.Fatalf("old event created Slack action=%d err=%v", slack, err)
	}
}

func TestMission07HistoricalBoundedPageAndRestart(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	eventID := "evt_" + randomHex(10)
	var event map[string]any
	if err := json.Unmarshal(billingPayload(eventID, "invoice.paid", v.customer, v.invoice, v.subscription), &event); err != nil {
		t.Fatal(err)
	}
	event["created"] = time.Now().Add(-10 * time.Minute).Unix()
	raw, _ := json.Marshal(event)
	stripeHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/events" {
			v.stripe(w, req)
			return
		}
		if req.URL.Query().Get("limit") != "1" {
			http.Error(w, "unbounded request", http.StatusBadRequest)
			return
		}
		jsonResponse(w, map[string]any{"data": []json.RawMessage{raw}, "has_more": true})
	}))
	defer stripeHTTP.Close()
	ctx := context.Background()
	var userID, requestID string
	if err := mig.QueryRow(ctx, `INSERT INTO users(oidc_issuer,oidc_subject,email) VALUES('test','history-'||gen_random_uuid()::text,'history@example.invalid') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := mig.QueryRow(ctx, `INSERT INTO reconcile_requests(tenant_id,stripe_connection_id,actor_user_id,from_at,to_at,max_events,reason)
	 VALUES($1,$2,$3,now()-interval '20 minutes',now()-interval '3 minutes',1,'Synthetic bounded history test') RETURNING id`,
		f.tenantID, f.connectionID, userID).Scan(&requestID); err != nil {
		t.Fatal(err)
	}
	r := &reconciliation.Reconciler{DB: db, Box: box, Endpoints: providers.Endpoints{
		Stripe: stripeHTTP.URL, HubSpot: stripeHTTP.URL, Slack: stripeHTTP.URL, Simulation: true}}
	worked, err := r.StepHistoricalTenant(ctx, f.tenantID)
	if err != nil || !worked {
		t.Fatalf("historical page: worked=%t err=%v", worked, err)
	}
	var status, gap, source string
	var scanned int
	if err := mig.QueryRow(ctx, `SELECT status,coalesce(gap_code,''),scanned_count FROM reconcile_requests WHERE id=$1`, requestID).
		Scan(&status, &gap, &scanned); err != nil || status != "limit_reached" || gap != "max_count_reached" || scanned != 1 {
		t.Fatalf("request status=%s gap=%s scanned=%d err=%v", status, gap, scanned, err)
	}
	if err := mig.QueryRow(ctx, `SELECT source FROM webhook_events WHERE tenant_id=$1 AND event_id=$2`, f.tenantID, eventID).Scan(&source); err != nil || source != "historical" {
		t.Fatalf("persisted source=%s err=%v", source, err)
	}
	// A new worker sees the terminal request and must not re-run it.
	worked, err = (&reconciliation.Reconciler{DB: db, Box: box, Endpoints: r.Endpoints}).StepHistoricalTenant(ctx, f.tenantID)
	if err != nil || worked {
		t.Fatalf("terminal request reclaimed: worked=%t err=%v", worked, err)
	}
	await(t, func() bool {
		var n int
		_ = mig.QueryRow(ctx, `SELECT count(*) FROM planning_jobs j JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
		 WHERE j.tenant_id=$1 AND e.event_id=$2 AND j.status='planned'`, f.tenantID, eventID).Scan(&n)
		return n == 1
	})
	_, messages := externalCounts(v)
	if messages != 0 {
		t.Fatalf("historical request sent %d Slack messages", messages)
	}
}

func TestMission07RedisQuotaDefersOnlyItsConnection(t *testing.T) {
	ctx := context.Background()
	quota := &execution.ProviderQuota{Client: testRedis(t)}
	type candidate struct {
		fixture    fixture
		reconciler *reconciliation.Reconciler
	}
	var candidates []candidate
	for range 2 {
		v := newWorld()
		f, _ := seedBusiness(t, v)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/v1/events" {
				jsonResponse(w, map[string]any{"data": []any{}, "has_more": false})
				return
			}
			v.stripe(w, req)
		}))
		t.Cleanup(server.Close)
		if _, err := mig.Exec(ctx, `INSERT INTO reconcile_checkpoints(tenant_id,stripe_connection_id,kind,window_start,window_end,next_run_at)
 VALUES($1,$2,'events',now()-interval '10 minutes',now()-interval '3 minutes',now())`, f.tenantID, f.connectionID); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate{f, &reconciliation.Reconciler{DB: db, Box: box, Quota: quota,
			Endpoints: providers.Endpoints{Stripe: server.URL, HubSpot: server.URL, Slack: server.URL, Simulation: true}}})
	}
	for _, item := range candidates {
		worked, err := item.reconciler.StepTenant(ctx, item.fixture.tenantID)
		if err != nil || !worked {
			t.Fatalf("quota deferral tenant=%s worked=%t err=%v", item.fixture.tenantID, worked, err)
		}
		var completed *time.Time
		if err := mig.QueryRow(ctx, `SELECT last_complete_at FROM reconcile_checkpoints
 WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind='events'`, item.fixture.tenantID, item.fixture.connectionID).Scan(&completed); err != nil || completed != nil {
			t.Fatalf("cold quota advanced checkpoint: complete=%v err=%v", completed, err)
		}
	}
	// Each account has a separate Redis budget. Either tenant may finish once
	// the cold-start window expires; neither needs the other's checkpoint.
	time.Sleep(1200 * time.Millisecond)
	for _, item := range []candidate{candidates[1], candidates[0]} {
		worked, err := item.reconciler.StepTenant(ctx, item.fixture.tenantID)
		if err != nil || !worked {
			t.Fatalf("quota recovery tenant=%s worked=%t err=%v", item.fixture.tenantID, worked, err)
		}
		var completed *time.Time
		if err := mig.QueryRow(ctx, `SELECT last_complete_at FROM reconcile_checkpoints
 WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind='events'`, item.fixture.tenantID, item.fixture.connectionID).Scan(&completed); err != nil || completed == nil {
			t.Fatalf("tenant did not complete after quota: complete=%v err=%v", completed, err)
		}
	}
}
