//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
	"github.com/lenoir-dev/LenoirDesk/internal/reconciliation"
)

func ownerPage(t *testing.T, app *testApp, cookie *http.Cookie, path string, markers ...string) string {
	t.Helper()
	resp := request(t, app.client, http.MethodGet, app.server.URL+path, cookie, "", nil)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("owner page %s: status=%d err=%v", path, resp.StatusCode, err)
	}
	for _, marker := range markers {
		if !strings.Contains(string(body), marker) {
			t.Fatalf("owner page %s missing %q", path, marker)
		}
	}
	return string(body)
}

func TestMission081OwnerJourney(t *testing.T) {
	v := newWorld()
	f, cfg := seedBusiness(t, v)
	app := newTestApp(t, newFakeOIDC(t))
	owner := login(t, app, "mission081-owner-"+randomHex(4), nil)
	ownerID, csrf := me(t, app, owner)
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	ownerPage(t, app, owner, "/", f.tenantID)
	ownerPage(t, app, owner, "/ui/tenants/"+f.tenantID, "Activate billing automation")
	ownerPage(t, app, owner, "/ui/tenants/"+f.tenantID+"/connections", f.account, "Local simulator fixture")
	form := url.Values{"csrf_token": {csrf}, "stripe": {cfg.StripeConnectionID},
		"hubspot": {cfg.HubSpotConnectionID}, "slack": {cfg.SlackConnectionID},
		"channel": {cfg.SlackChannelID}, "prices": {v.price}, "policy": {cfg.CreationPolicy}}
	preview := postUIForm(t, app, owner, "/ui/tenants/"+f.tenantID+"/billing-sync/preview", form)
	previewBody, _ := io.ReadAll(preview.Body)
	_ = preview.Body.Close()
	if preview.StatusCode != http.StatusOK || !strings.Contains(string(previewBody), f.account) {
		t.Fatalf("automation review status=%d", preview.StatusCode)
	}
	activated := postUIForm(t, app, owner, "/ui/tenants/"+f.tenantID+"/billing-sync", form)
	_ = activated.Body.Close()
	if activated.StatusCode != http.StatusSeeOther {
		t.Fatalf("automation activation status=%d", activated.StatusCode)
	}
	ownerPage(t, app, owner, "/ui/tenants/"+f.tenantID+"/billing-sync?notice=activated", "Automation revision activated")
	var activation time.Time
	if err := mig.QueryRow(context.Background(), `SELECT activated_at FROM automation_revisions
	 WHERE tenant_id=$1 AND connection_id=$2 AND status='active' AND template_key=$3`,
		f.tenantID, cfg.StripeConnectionID, business.Template).Scan(&activation); err != nil {
		t.Fatal(err)
	}
	// Webhook admission selects only revisions whose activation time has
	// reached PostgreSQL's clock. Wait for that contract before testing the
	// Stripe second-precision boundary, otherwise the event can be ignored
	// before a run is planned.
	await(t, func() bool {
		var active bool
		if err := mig.QueryRow(context.Background(), `SELECT EXISTS(
		 SELECT 1 FROM automation_revisions
		 WHERE tenant_id=$1 AND connection_id=$2 AND status='active'
		 AND template_key=$3 AND activated_at<=now())`,
			f.tenantID, cfg.StripeConnectionID, business.Template).Scan(&active); err != nil {
			t.Fatal(err)
		}
		return active
	})
	startExecutor(t, v)
	// Stripe's created timestamp has second precision. The ambiguous
	// activation second is deliberately skipped so an earlier invoice in that
	// same second cannot send a historical Slack notification.
	boundaryID := "evt_" + randomHex(8)
	raw := billingPayload(boundaryID, "invoice.paid", v.customer, v.invoice, v.subscription)
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	event["created"] = activation.Unix()
	raw, _ = json.Marshal(event)
	if got := post(t, f, raw, time.Now(), f.secret); got != http.StatusAccepted {
		t.Fatalf("boundary event acceptance status=%d", got)
	}
	waitBusinessStatus(t, f.tenantID, boundaryID, "skipped")
	if pause := time.Until(time.Unix(activation.Unix()+1, 0)); pause > 0 {
		time.Sleep(pause)
	}
	eventID := deliverBusiness(t, f, v, "invoice.paid")
	waitBusinessStatus(t, f.tenantID, eventID, "succeeded")
	if writes, messages := externalCounts(v); writes != 1 || messages != 1 {
		t.Fatalf("paid event effects: Contact=%d Slack=%d", writes, messages)
	}
	var paidRun string
	if err := mig.QueryRow(context.Background(), `SELECT r.id FROM runs r
	 JOIN planning_jobs j ON j.tenant_id=r.tenant_id AND j.id=r.planning_job_id
	 JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
	 WHERE r.tenant_id=$1 AND e.event_id=$2`, f.tenantID, eventID).Scan(&paidRun); err != nil {
		t.Fatal(err)
	}
	ownerPage(t, app, owner, "/ui/tenants/"+f.tenantID+"/runs", eventID)
	ownerPage(t, app, owner, "/ui/tenants/"+f.tenantID+"/runs/"+paidRun, "HubSpot Contact", "Slack notification")

	key, err := business.CustomerKey(f.account, "test", v.customer)
	if err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	v.contacts[key]["relaydesk_customer_key"] = "stripe:acct_other:test:cus_other"
	v.mu.Unlock()
	failureID := deliverBusiness(t, f, v, "customer.subscription.updated")
	waitBusinessStatus(t, f.tenantID, failureID, "needs_attention")
	ownerPage(t, app, owner, "/ui/tenants/"+f.tenantID+"/runs?status=needs_attention", failureID)
	var failedRun, actionID string
	var generation int64
	if err := mig.QueryRow(context.Background(), `SELECT r.id,a.id,a.generation FROM runs r
	 JOIN planning_jobs j ON j.tenant_id=r.tenant_id AND j.id=r.planning_job_id
	 JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
	 JOIN business_run_actions ra ON ra.tenant_id=r.tenant_id AND ra.run_id=r.id AND ra.kind='crm_projection'
	 JOIN business_actions a ON a.tenant_id=ra.tenant_id AND a.id=ra.action_id
	 WHERE r.tenant_id=$1 AND e.event_id=$2`, f.tenantID, failureID).Scan(&failedRun, &actionID, &generation); err != nil {
		t.Fatal(err)
	}
	ownerPage(t, app, owner, "/ui/tenants/"+f.tenantID+"/runs/"+failedRun, "Retry")
	v.mu.Lock()
	v.contacts[key]["relaydesk_customer_key"] = key
	v.mu.Unlock()
	retry := postUIForm(t, app, owner, "/ui/tenants/"+f.tenantID+"/runs/"+failedRun+"/actions/"+actionID+"/recover",
		url.Values{"csrf_token": {csrf}, "generation": {strconv.FormatInt(generation, 10)},
			"decision": {"manual_retry"}, "reason": {"Verified local Contact identity after simulated conflict"}})
	_ = retry.Body.Close()
	if retry.StatusCode != http.StatusSeeOther {
		t.Fatalf("operator recovery status=%d", retry.StatusCode)
	}
	waitBusinessStatus(t, f.tenantID, failureID, "succeeded")
	if writes, messages := externalCounts(v); writes != 1 || messages != 1 {
		t.Fatalf("recovery duplicated external effects: Contact=%d Slack=%d", writes, messages)
	}

	from := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	to := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	historicalID := "evt_" + randomHex(8)
	historical := billingPayload(historicalID, "customer.subscription.updated", v.customer, v.invoice, v.subscription)
	if err := json.Unmarshal(historical, &event); err != nil {
		t.Fatal(err)
	}
	event["created"] = from.Add(3 * time.Minute).Unix()
	historical, _ = json.Marshal(event)
	stripeHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			jsonResponse(w, map[string]any{"object": "list", "has_more": false, "data": []json.RawMessage{historical}})
			return
		}
		v.stripe(w, r)
	}))
	defer stripeHTTP.Close()
	reconcileForm := url.Values{"csrf_token": {csrf}, "connection_id": {cfg.StripeConnectionID},
		"from_at": {from.Format(time.RFC3339)}, "to_at": {to.Format(time.RFC3339)},
		"max_events": {"10"}, "reason": {"Controlled owner journey coverage scan"}}
	reconcilePath := "/ui/tenants/" + f.tenantID + "/reconciliation"
	ownerPage(t, app, owner, reconcilePath, "Outbox age", "Coverage checkpoints")
	review := postUIForm(t, app, owner, reconcilePath+"/preview", reconcileForm)
	_ = review.Body.Close()
	if review.StatusCode != http.StatusOK {
		t.Fatalf("reconciliation review status=%d", review.StatusCode)
	}
	submit := postUIForm(t, app, owner, reconcilePath, reconcileForm)
	_ = submit.Body.Close()
	if submit.StatusCode != http.StatusSeeOther {
		t.Fatalf("reconciliation request status=%d", submit.StatusCode)
	}
	r := &reconciliation.Reconciler{DB: db, Box: box, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Endpoints: providers.Endpoints{Stripe: stripeHTTP.URL, HubSpot: stripeHTTP.URL, Slack: stripeHTTP.URL, Simulation: true}}
	worked, err := r.StepHistoricalTenant(context.Background(), f.tenantID)
	if err != nil || !worked {
		t.Fatalf("bounded historical scan worked=%t err=%v", worked, err)
	}
	ownerPage(t, app, owner, reconcilePath+"?notice=reconciliation_requested", "completed", "Outbox age")
	if writes, messages := externalCounts(v); writes != 1 || messages != 1 {
		t.Fatalf("historical scan emitted effects: Contact=%d Slack=%d", writes, messages)
	}
}
