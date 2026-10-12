//go:build integration

package integration

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
)

func startDemoExhaustedExecutor(t *testing.T, v *providerWorld) func() {
	t.Helper()
	stripeHTTP := httptest.NewServer(http.HandlerFunc(v.stripe))
	hubspotHTTP := httptest.NewServer(http.HandlerFunc(v.hubspot))
	slackHTTP := httptest.NewServer(http.HandlerFunc(v.slack))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	x := &execution.Executor{DB: db, Box: box, Log: slog.New(slog.NewTextHandler(os.Stdout, nil)),
		Endpoints: providers.Endpoints{Stripe: stripeHTTP.URL, HubSpot: hubspotHTTP.URL, Slack: slackHTTP.URL, Simulation: true},
		Retry: execution.RetryPolicy{Base: 100 * time.Millisecond, Cap: 100 * time.Millisecond,
			MaxAge: time.Hour, MaxProviderCalls: 4, Random: func() float64 { return 0 }}}
	go func() { defer close(done); x.Run(ctx) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() { cancel(); <-done; stripeHTTP.Close(); hubspotHTTP.Close(); slackHTTP.Close() })
	}
	t.Cleanup(stop)
	return stop
}

// This opt-in demonstration keeps only local HTTP provider simulators alive.
// It intentionally uses the integration stack and never contacts real accounts.
func TestMission06InteractiveDemo(t *testing.T) {
	if os.Getenv("RELAYDESK_INTERACTIVE_DEMO") != "1" {
		t.Skip("run make demo-local to start the browser demonstration")
	}
	v := newWorld()
	f, cfg := seedBusiness(t, v)
	if _, err := mig.Exec(context.Background(), `UPDATE connections SET verified_at=now(),last_checked_at=now()
	 WHERE tenant_id=$1 AND execution_environment='simulation' AND health_status='verified'`, f.tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Exec(context.Background(), `UPDATE tenants SET name='LenoirDesk Demo — Northstar Studio (synthetic)' WHERE id=$1`, f.tenantID); err != nil {
		t.Fatal(err)
	}
	// The shared integration fixture has a legacy placeholder revision #1.
	// Replace that unreferenced placeholder only in this disposable demo DB with
	// a readable historical config; the production revision API remains immutable.
	if _, err := mig.Exec(context.Background(), `DELETE FROM automation_revisions WHERE tenant_id=$1 AND connection_id=$2
	 AND revision=1 AND template_key='legacy_planning'`, f.tenantID, f.connectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Exec(context.Background(), `INSERT INTO automation_revisions
	 (tenant_id,connection_id,revision,status,template_key,activated_at,config)
	 SELECT tenant_id,connection_id,1,'inactive',template_key,now()-interval '7 days',config
	 FROM automation_revisions WHERE id=$1`, f.revisionID); err != nil {
		t.Fatal(err)
	}
	subject := "relaydesk-local-demo-" + randomHex(5)
	idp := newFakeOIDC(t)
	idp.setBrowserSubject(subject)
	hub := httptest.NewServer(http.HandlerFunc(v.hubspot))
	defer hub.Close()
	app := newTestAppHubSpot(t, idp, hub.URL)
	owner := login(t, app, subject, nil)
	userID, csrf := me(t, app, owner)
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f.tenantID, userID); err != nil {
		t.Fatal(err)
	}
	connectionsPage := ownerPage(t, app, owner, "/ui/tenants/"+f.tenantID+"/connections", "Synthetic demo connections are fixed")
	if strings.Contains(connectionsPage, "Reconnect same account") || strings.Contains(connectionsPage, "Set up Stripe") {
		t.Fatal("synthetic demo exposed provider onboarding controls")
	}
	closeResponse(t, request(t, app.client, http.MethodPost, app.server.URL+"/v1/tenants/"+f.tenantID+"/connections/stripe-test", owner, csrf, map[string]string{}), http.StatusForbidden)
	closeResponse(t, request(t, app.client, http.MethodPost, app.server.URL+"/v1/tenants/"+f.tenantID+"/connections/oauth/hubspot", owner, csrf, map[string]string{}), http.StatusForbidden)
	cancelExecutor := startExecutor(t, v)
	first := deliverBusiness(t, f, v, "invoice.paid")
	waitBusinessStatus(t, f.tenantID, first, "succeeded")
	noWrite := deliverBusiness(t, f, v, "customer.subscription.updated")
	waitBusinessStatus(t, f.tenantID, noWrite, "succeeded")
	// One unknown notification is confirmed through the real owner recovery API,
	// leaving an audited operator decision. A second remains open for inspection.
	for i := 0; i < 2; i++ {
		v.mu.Lock()
		v.invoice = "in_test_" + randomHex(8)
		v.slackLost = true
		v.mu.Unlock()
		event := deliverBusiness(t, f, v, "invoice.paid")
		waitBusinessStatus(t, f.tenantID, event, "unknown")
		if i == 0 {
			var actionID string
			var generation int64
			if err := mig.QueryRow(context.Background(), `SELECT a.id,a.generation FROM business_actions a
			 JOIN business_run_actions ra ON ra.tenant_id=a.tenant_id AND ra.action_id=a.id
			 JOIN runs r ON r.tenant_id=ra.tenant_id AND r.id=ra.run_id
			 JOIN planning_jobs j ON j.tenant_id=r.tenant_id AND j.id=r.planning_job_id
			 JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
			 WHERE a.tenant_id=$1 AND e.event_id=$2 AND a.kind='slack_invoice_paid'`, f.tenantID, event).Scan(&actionID, &generation); err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"decision": "confirm_unknown", "reason": "Observed in synthetic destination channel", "expected_generation": generation,
				"external_channel": v.channel, "external_ts": "1700000000.000001", "idempotency_key": "demo-confirm-" + randomHex(4)}
			closeResponse(t, request(t, app.client, http.MethodPost, app.server.URL+"/v1/tenants/"+f.tenantID+"/actions/"+actionID+"/resolve", owner, csrf, body), http.StatusOK)
			waitBusinessStatus(t, f.tenantID, event, "succeeded")
		}
	}
	v.mu.Lock()
	v.slackLost = false
	v.hubspotUnavailable = true
	v.subscriptionStatus = "past_due"
	v.mu.Unlock()
	cancelExecutor()
	stopExhausted := startDemoExhaustedExecutor(t, v)
	dead := deliverBusiness(t, f, v, "customer.subscription.updated")
	waitBusinessStatus(t, f.tenantID, dead, "dead")
	stopExhausted()
	v.mu.Lock()
	v.hubspotUnavailable = false
	v.subscriptionStatus = "active"
	v.mu.Unlock()
	pending := deliverBusiness(t, f, v, "customer.subscription.updated")
	// The run is in progress while its persisted CRM action waits unclaimed.
	waitBusinessStatus(t, f.tenantID, pending, "running")
	await(t, func() bool {
		var actionStatus string
		err := mig.QueryRow(context.Background(), `SELECT a.status FROM business_actions a
		 JOIN business_run_actions ra ON ra.tenant_id=a.tenant_id AND ra.action_id=a.id
		 JOIN runs r ON r.tenant_id=ra.tenant_id AND r.id=ra.run_id
		 JOIN planning_jobs j ON j.tenant_id=r.tenant_id AND j.id=r.planning_job_id
		 JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
		 WHERE a.tenant_id=$1 AND e.event_id=$2 AND a.kind='crm_projection'`, f.tenantID, pending).Scan(&actionStatus)
		return err == nil && actionStatus == "pending"
	})
	if _, err := mig.Exec(context.Background(), `INSERT INTO reconcile_checkpoints
	 (tenant_id,stripe_connection_id,kind,window_start,window_end,last_complete_at,gap_code,gap_at,scanned_count,discovered_count)
	 VALUES($1,$2,'events',now()-interval '2 hours',now()-interval '1 hour',now()-interval '1 hour','events_permission',now(),24,2),
	 ($1,$2,'known',now()-interval '1 hour',now()-interval '2 minutes',now()-interval '2 minutes',NULL,NULL,18,0)`, f.tenantID, cfg.StripeConnectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Exec(context.Background(), `INSERT INTO reconcile_requests
	 (tenant_id,stripe_connection_id,actor_user_id,from_at,to_at,max_events,reason,status,scanned_count,discovered_count,created_at,finished_at)
	 VALUES($1,$2,$3,now()-interval '3 hours',now()-interval '2 hours',50,'Synthetic historical billing review','completed',31,2,now()-interval '1 hour',now()-interval '1 hour')`, f.tenantID, cfg.StripeConnectionID, userID); err != nil {
		t.Fatal(err)
	}
	steps := []struct{ value, label string }{{"start_pending", "Complete the pending billing event"}, {"outage", "Simulate a temporary CRM outage"}, {"restore_outage", "Restore local CRM service"},
		{"failure", "Inject Contact identity conflict"}, {"restore", "Restore local Contact identity"}, {"finish", "Finish demo after CRM retry"}}
	controlCode := randomHex(16)
	auto := os.Getenv("RELAYDESK_DEMO_AUTORUN") == "1"
	advance := make(chan string, 1)
	var mu sync.Mutex
	stage := 0
	controls := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			if r.ParseForm() != nil || r.FormValue("code") != controlCode || stage >= len(steps) || r.FormValue("step") != steps[stage].value {
				http.Error(w, "invalid demo step", 409)
				return
			}
			if steps[stage].value == "finish" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte("<!doctype html><html lang=\"en\"><meta charset=\"utf-8\"><title>LenoirDesk demo complete</title><h1>Demo complete</h1><p>The local CRM and Slack effects are being verified in the test terminal. You can close this tab.</p></html>"))
				if flush, ok := w.(http.Flusher); ok {
					flush.Flush()
				}
				advance <- steps[stage].value
				stage++
				return
			}
			advance <- steps[stage].value
			stage++
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		label, value := "Demo complete", ""
		if stage < len(steps) {
			label, value = steps[stage].label, steps[stage].value
		}
		page := template.Must(template.New("control").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><title>LenoirDesk local demo control</title><style>body{font:18px system-ui;max-width:40rem;margin:3rem auto;padding:1rem}button{font:inherit;padding:.7rem 1rem}</style><h1>Local simulation control</h1><p>Only local HTTP simulators are affected. Follow the LenoirDesk instructions in the other tab.</p>{{if .Value}}<form method="post"><input type="hidden" name="code" value="{{.Code}}"><input type="hidden" name="step" value="{{.Value}}"><button>{{.Label}}</button></form>{{else}}<p>Demo complete. You can close this tab.</p>{{end}}</html>`))
		_ = page.Execute(w, struct{ Label, Value, Code string }{label, value, controlCode})
	}))
	defer controls.Close()
	waitStep := func(want string) {
		t.Helper()
		if auto {
			response, err := http.PostForm(controls.URL+"/", url.Values{"code": {controlCode}, "step": {want}})
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("automatic local control %s status=%d", want, response.StatusCode)
			}
		}
		select {
		case got := <-advance:
			if got != want {
				t.Fatalf("demo step %q, expected %q", got, want)
			}
		}
	}
	fmt.Printf("\nLOCAL DEMO: open %s/ in a browser, then Sign in.\n", app.server.URL)
	fmt.Printf("Simulation controls: %s/ (open in a second tab).\n", controls.URL)
	fmt.Printf("Workspace: LenoirDesk Demo — Northstar Studio (synthetic). Connected providers and active revision are isolated simulations.\n")
	fmt.Printf("First successful run: %s/ui/tenants/%s/runs\n", app.server.URL, f.tenantID)
	fmt.Print("Inspect Connections, Automation, Runs, pending and stopped retries (DLQ), unknown Slack outcome, operator-confirmed message and Reconciliation. Then use the control tab to complete the pending billing event.\n")
	waitStep("start_pending")
	cancelExecutor = startExecutor(t, v)
	waitBusinessStatus(t, f.tenantID, pending, "succeeded")
	fmt.Print("Pending billing execution completed. Now simulate a CRM outage.\n")
	waitStep("outage")
	v.mu.Lock()
	v.hubspotUnavailable = true
	v.subscriptionStatus = "past_due"
	v.mu.Unlock()
	outage := deliverBusiness(t, f, v, "customer.subscription.updated")
	waitBusinessStatus(t, f.tenantID, outage, "retry_wait")
	// Freeze this local executor while the owner inspects the scheduled retry.
	// Otherwise an arbitrarily long browser review can exhaust its retry budget.
	cancelExecutor()
	fmt.Printf("Scheduled retry is visible: %s/ui/tenants/%s/runs?status=retry_wait\n", app.server.URL, f.tenantID)
	waitStep("restore_outage")
	v.mu.Lock()
	v.hubspotUnavailable = false
	v.mu.Unlock()
	startExecutor(t, v)
	waitBusinessStatus(t, f.tenantID, outage, "succeeded")
	fmt.Print("CRM recovered through the normal retry path; no duplicate Contact write. Now inject a Contact conflict.\n")
	waitStep("failure")
	key, err := business.CustomerKey(f.account, "test", v.customer)
	if err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	v.contacts[key]["relaydesk_customer_key"] = "stripe:acct_other:test:cus_other"
	v.subscriptionStatus = "canceled"
	v.mu.Unlock()
	failed := deliverBusiness(t, f, v, "customer.subscription.updated")
	waitBusinessStatus(t, f.tenantID, failed, "needs_attention")
	fmt.Printf("Contact identity conflict is visible in workspace run history: %s/ui/tenants/%s/runs?status=needs_attention\n", app.server.URL, f.tenantID)
	fmt.Print("Inspect the failed CRM action. Use the control tab to restore the local provider Contact identity.\n")
	waitStep("restore")
	v.mu.Lock()
	v.contacts[key]["relaydesk_customer_key"] = key
	v.mu.Unlock()
	fmt.Print("Provider identity restored. In LenoirDesk, enter a recovery reason and Retry this CRM action. Use the control tab to finish after the run succeeds.\n")
	if auto {
		var actionID string
		var generation int64
		if err := mig.QueryRow(context.Background(), `SELECT a.id,a.generation FROM business_actions a
		 JOIN business_run_actions ra ON ra.tenant_id=a.tenant_id AND ra.action_id=a.id
		 JOIN runs r ON r.tenant_id=ra.tenant_id AND r.id=ra.run_id
		 JOIN planning_jobs j ON j.tenant_id=r.tenant_id AND j.id=r.planning_job_id
		 JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
		 WHERE a.tenant_id=$1 AND e.event_id=$2 AND a.kind='crm_projection'`, f.tenantID, failed).Scan(&actionID, &generation); err != nil {
			t.Fatal(err)
		}
		body := map[string]any{"reason": "Exact synthetic managed Contact key verified and restored", "expected_generation": generation,
			"idempotency_key": "demo-retry-" + randomHex(4)}
		closeResponse(t, request(t, app.client, http.MethodPost, app.server.URL+"/v1/tenants/"+f.tenantID+"/actions/"+actionID+"/retry", owner, csrf, body), http.StatusOK)
	}
	waitStep("finish")
	waitBusinessStatus(t, f.tenantID, failed, "succeeded")
	writes, messages := externalCounts(v)
	if writes != 5 || messages != 3 {
		t.Fatalf("unexpected local provider effects: CRM=%d Slack=%d", writes, messages)
	}
	var duplicatedWrites, noWriteActions int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE writes>1),count(*) FILTER (WHERE writes=0)
	 FROM (SELECT a.id,count(*) FILTER (WHERE pc.provider='hubspot' AND pc.method IN ('POST','PATCH')) AS writes
	 FROM business_actions a LEFT JOIN business_attempts bt ON bt.tenant_id=a.tenant_id AND bt.action_id=a.id
	 LEFT JOIN business_provider_calls pc ON pc.tenant_id=bt.tenant_id AND pc.attempt_id=bt.id
	 WHERE a.tenant_id=$1 AND a.kind='crm_projection' GROUP BY a.id) x`, f.tenantID).Scan(&duplicatedWrites, &noWriteActions); err != nil {
		t.Fatal(err)
	}
	if duplicatedWrites != 0 || noWriteActions < 1 {
		t.Fatalf("CRM replay/no-write contract failed: duplicate actions=%d no-write actions=%d", duplicatedWrites, noWriteActions)
	}
	fmt.Printf("Demo complete: CRM writes=%s, Slack sends=%s. Close this terminal to stop the local browser server.\n", strconv.Itoa(writes), strconv.Itoa(messages))
}
