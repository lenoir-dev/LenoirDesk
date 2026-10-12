//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

type providerWorld struct {
	mu                 sync.Mutex
	hubspotWrites      int
	contacts           map[string]map[string]string
	slackMessages      int
	slackLost          bool
	hubspotLost        bool
	invoiceStatus      string
	subscriptionStatus string
	price              string
	customer           string
	subscription       string
	invoice            string
	channel            string
	multiple           bool
	cancelAtPeriodEnd  bool
	endedAt            int64
	schemaMissing      bool
	emailCollision     bool
	slack429           int
	hubspot429         int
	hubspotPartial     string
	archivedContact    bool
	slackError         string
	hubspotUnavailable bool
	mergedContactID    string
}

func newWorld() *providerWorld {
	// Fresh resource IDs prevent stale pointers retained by a previous local
	// integration run from mutating the current run's HTTP simulator state.
	suffix := randomHex(8)
	return &providerWorld{contacts: map[string]map[string]string{}, invoiceStatus: "paid", subscriptionStatus: "active",
		price: "price_test_" + suffix, customer: "cus_test_" + suffix, subscription: "sub_test_" + suffix,
		invoice: "in_test_" + suffix, channel: "C" + strings.ToUpper(suffix)}
}
func jsonResponse(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func (v *providerWorld) stripe(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "unauthorized", 401)
		return
	}
	switch {
	case r.URL.Path == "/v1/customers/"+v.customer:
		jsonResponse(w, map[string]any{"id": v.customer, "object": "customer", "livemode": false})
	case r.URL.Path == "/v1/subscriptions":
		items := []any{map[string]any{"id": v.subscription, "object": "subscription", "customer": v.customer, "status": v.subscriptionStatus, "livemode": false, "cancel_at_period_end": v.cancelAtPeriodEnd, "ended_at": v.endedAt, "items": map[string]any{"object": "list", "data": []any{map[string]any{"id": "si_test", "price": map[string]any{"id": v.price}}}}, "latest_invoice": v.invoice}}
		if v.multiple {
			items = append(items, map[string]any{"id": "sub_other", "object": "subscription", "customer": v.customer, "status": "active", "livemode": false, "items": map[string]any{"object": "list", "data": []any{map[string]any{"id": "si_other", "price": map[string]any{"id": v.price}}}}})
		}
		jsonResponse(w, map[string]any{"object": "list", "data": items, "has_more": false})
	case r.URL.Path == "/v1/invoices/"+v.invoice:
		jsonResponse(w, map[string]any{"id": v.invoice, "object": "invoice", "customer": v.customer, "livemode": false, "status": v.invoiceStatus, "attempted": true, "currency": "usd", "amount_paid": 1500, "parent": map[string]any{"type": "subscription_details", "subscription_details": map[string]any{"subscription": v.subscription}}})
	default:
		http.NotFound(w, r)
	}
}
func (v *providerWorld) hubspot(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer sim_hubspot" {
		http.Error(w, "unauthorized", 401)
		return
	}
	if v.hubspotUnavailable {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/crm/v3/objects/contacts/") && r.Method == http.MethodGet {
		key := strings.TrimPrefix(path, "/crm/v3/objects/contacts/")
		if r.URL.Query().Get("idProperty") == "" {
			if key != "101" && (v.mergedContactID == "" || key != v.mergedContactID) {
				http.NotFound(w, r)
				return
			}
			for _, properties := range v.contacts {
				id := "101"
				if v.mergedContactID != "" {
					id = v.mergedContactID
				}
				jsonResponse(w, map[string]any{"id": id, "properties": properties, "archived": v.archivedContact})
				return
			}
		}
		properties, ok := v.contacts[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		id := "101"
		if v.mergedContactID != "" {
			id = v.mergedContactID
		}
		jsonResponse(w, map[string]any{"id": id, "properties": properties, "archived": v.archivedContact})
		return
	}
	if path == "/crm/v3/objects/contacts/batch/upsert" && r.Method == http.MethodPost {
		if v.hubspot429 > 0 {
			v.hubspot429--
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", 429)
			return
		}
		if v.schemaMissing {
			http.Error(w, `{"status":"error","category":"VALIDATION_ERROR"}`, 400)
			return
		}
		if v.emailCollision {
			http.Error(w, `{"status":"error","category":"CONFLICT"}`, 409)
			return
		}
		if v.hubspotPartial != "" {
			w.WriteHeader(http.StatusMultiStatus)
			jsonResponse(w, map[string]any{"status": "PARTIAL", "errors": []any{map[string]string{"category": v.hubspotPartial}}})
			return
		}
		var req struct {
			Inputs []struct {
				ID         string            `json:"id"`
				IDProperty string            `json:"idProperty"`
				Properties map[string]string `json:"properties"`
			} `json:"inputs"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Inputs) != 1 || req.Inputs[0].IDProperty != "relaydesk_customer_key" {
			http.Error(w, "invalid", 400)
			return
		}
		v.contacts[req.Inputs[0].ID] = req.Inputs[0].Properties
		v.hubspotWrites++
		if v.hubspotLost {
			v.hubspotLost = false
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{")
			return
		}
		jsonResponse(w, map[string]any{"status": "COMPLETE", "results": []any{map[string]any{"id": "101", "properties": req.Inputs[0].Properties, "archived": false}}})
		return
	}
	if (path == "/crm/v3/objects/contacts/101" && v.mergedContactID == "" ||
		v.mergedContactID != "" && path == "/crm/v3/objects/contacts/"+v.mergedContactID) && r.Method == http.MethodPatch {
		var req struct {
			Properties map[string]string `json:"properties"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		key := req.Properties["relaydesk_customer_key"]
		v.contacts[key] = req.Properties
		v.hubspotWrites++
		id := "101"
		if v.mergedContactID != "" {
			id = v.mergedContactID
		}
		jsonResponse(w, map[string]any{"id": id, "properties": req.Properties, "archived": false})
		return
	}
	http.NotFound(w, r)
}
func (v *providerWorld) slack(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if r.URL.Path != "/api/chat.postMessage" {
		http.NotFound(w, r)
		return
	}
	if v.slack429 > 0 {
		v.slack429--
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limited", 429)
		return
	}
	if r.Header.Get("Authorization") != "Bearer sim_slack" {
		http.Error(w, "unauthorized", 401)
		return
	}
	if v.slackError != "" {
		jsonResponse(w, map[string]any{"ok": false, "error": v.slackError})
		return
	}
	var req struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Channel != v.channel {
		jsonResponse(w, map[string]any{"ok": false, "error": "channel_not_found"})
		return
	}
	v.slackMessages++
	if v.slackLost {
		v.slackLost = false
		_, _ = io.WriteString(w, "{")
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "channel": req.Channel, "ts": fmt.Sprintf("1000.%06d", v.slackMessages)})
}
func seedBusiness(t *testing.T, v *providerWorld) (fixture, business.Config) {
	t.Helper()
	f := seed(t, false)
	ctx := context.Background()
	_, err := mig.Exec(ctx, `UPDATE connections SET health_status='verified',execution_environment='simulation' WHERE id=$1`, f.connectionID)
	if err != nil {
		t.Fatal(err)
	}
	addToken := func(id, provider, secret string) {
		t.Helper()
		cipher, nonce, e := box.Seal([]byte(secret), f.tenantID, id, provider, 1)
		if e != nil {
			t.Fatal(e)
		}
		_, e = mig.Exec(ctx, `INSERT INTO connection_credentials(tenant_id,connection_id,generation,secret_kind,ciphertext,nonce,key_version) VALUES($1,$2,1,'api_token',$3,$4,1)`, f.tenantID, id, cipher, nonce)
		if e != nil {
			t.Fatal(e)
		}
	}
	addToken(f.connectionID, "stripe", "sk_test_simulated")
	addConnection := func(provider, account, token string) string {
		t.Helper()
		var id string
		if e := mig.QueryRow(ctx, `INSERT INTO connections(tenant_id,provider,provider_account_id,mode,status,health_status,execution_environment) VALUES($1,$2,$3,'test','active','verified','simulation') RETURNING id`, f.tenantID, provider, account).Scan(&id); e != nil {
			t.Fatal(e)
		}
		addToken(id, provider, token)
		return id
	}
	hubspot := addConnection("hubspot", "portal_"+randomHex(4), "sim_hubspot")
	slack := addConnection("slack", "workspace_"+randomHex(4), "sim_slack")
	cfg := business.Config{Template: business.Template, StripeConnectionID: f.connectionID, HubSpotConnectionID: hubspot, SlackConnectionID: slack, SlackChannelID: v.channel, TrackedPriceIDs: []string{v.price}, CreationPolicy: "unique_upsert", ActivationTime: time.Now().Add(-time.Second).UTC()}
	raw, _ := json.Marshal(cfg)
	if err = mig.QueryRow(ctx, `INSERT INTO automation_revisions(tenant_id,connection_id,revision,status,template_key,activated_at,config) VALUES($1,$2,2,'active',$3,$4,$5) RETURNING id`, f.tenantID, f.connectionID, business.Template, cfg.ActivationTime, raw).Scan(&f.revisionID); err != nil {
		t.Fatal(err)
	}
	return f, cfg
}
func billingPayload(id, kind, customer, invoice, subscription string) []byte {
	obj := map[string]any{"id": invoice, "object": "invoice", "customer": customer, "parent": map[string]any{"subscription_details": map[string]any{"subscription": subscription}}}
	if strings.HasPrefix(kind, "customer.subscription.") {
		obj = map[string]any{"id": subscription, "object": "subscription", "customer": customer}
	}
	raw, _ := json.Marshal(map[string]any{"id": id, "object": "event", "type": kind, "api_version": stripe.APIVersion, "livemode": false, "created": time.Now().Unix(), "data": map[string]any{"object": obj}})
	return raw
}
func startExecutor(t *testing.T, v *providerWorld) context.CancelFunc {
	return startExecutorWithFault(t, v, nil)
}

func startExecutorWithFault(t *testing.T, v *providerWorld, fault func(string, string, string) bool) context.CancelFunc {
	t.Helper()
	stripeHTTP := httptest.NewServer(http.HandlerFunc(v.stripe))
	hubspotHTTP := httptest.NewServer(http.HandlerFunc(v.hubspot))
	slackHTTP := httptest.NewServer(http.HandlerFunc(v.slack))
	ctx, cancel := context.WithCancel(context.Background())
	x := &execution.Executor{DB: db, Box: box, Log: slog.New(slog.NewTextHandler(os.Stdout, nil)), Endpoints: providers.Endpoints{Stripe: stripeHTTP.URL, HubSpot: hubspotHTTP.URL, Slack: slackHTTP.URL, Simulation: true}, Fault: fault}
	done := make(chan struct{})
	go func() { defer close(done); x.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; stripeHTTP.Close(); hubspotHTTP.Close(); slackHTTP.Close() })
	return cancel
}
func TestBusinessPaidInvoice(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	raw := billingPayload("evt_"+randomHex(8), "invoice.paid", v.customer, v.invoice, v.subscription)
	if got := post(t, f, raw, time.Now(), f.secret); got != 202 {
		t.Fatalf("webhook status %d", got)
	}
	await(t, func() bool {
		var status string
		err := mig.QueryRow(context.Background(), `SELECT status FROM runs WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 1`, f.tenantID).Scan(&status)
		return err == nil && status == "succeeded"
	})
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.hubspotWrites != 1 || v.slackMessages != 1 {
		t.Fatalf("external writes HubSpot=%d Slack=%d", v.hubspotWrites, v.slackMessages)
	}
	for _, properties := range v.contacts {
		if properties["relaydesk_latest_invoice_id"] != v.invoice || properties["relaydesk_payment_state"] != "paid" {
			t.Fatalf("incorrect CRM projection: %v", properties)
		}
	}
	var actions, attempts int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_actions WHERE tenant_id=$1`, f.tenantID).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_attempts WHERE tenant_id=$1`, f.tenantID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if actions != 2 || attempts != 2 {
		t.Fatalf("actions=%d attempts=%d", actions, attempts)
	}
	for _, provider := range []string{"stripe", "hubspot", "slack"} {
		var calls, unfinished int
		if err := mig.QueryRow(context.Background(), `SELECT count(*),count(*) FILTER (WHERE outcome IS NULL) FROM business_provider_calls WHERE tenant_id=$1 AND provider=$2`, f.tenantID, provider).Scan(&calls, &unfinished); err != nil {
			t.Fatal(err)
		}
		if calls == 0 || unfinished != 0 {
			t.Fatalf("provider %s calls=%d unfinished=%d", provider, calls, unfinished)
		}
	}
	var runID, channel, ts, contactID string
	if err := mig.QueryRow(context.Background(), `SELECT r.id,s.slack_channel,s.slack_ts,c.contact_id
 FROM runs r JOIN business_run_actions sl ON sl.tenant_id=r.tenant_id AND sl.run_id=r.id AND sl.kind='slack_invoice_paid'
 JOIN business_actions s ON s.tenant_id=sl.tenant_id AND s.id=sl.action_id
 JOIN business_run_actions cr ON cr.tenant_id=r.tenant_id AND cr.run_id=r.id AND cr.kind='crm_projection'
 JOIN business_actions c ON c.tenant_id=cr.tenant_id AND c.id=cr.action_id
 WHERE r.tenant_id=$1`, f.tenantID).Scan(&runID, &channel, &ts, &contactID); err != nil {
		t.Fatal(err)
	}
	if channel != v.channel || ts == "" || contactID == "" {
		t.Fatalf("missing confirmed provider references: contact=%q channel=%q ts=%q", contactID, channel, ts)
	}
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	owner := login(t, app, "business-history-"+randomHex(4), nil)
	ownerID, _ := me(t, app, owner)
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	detailResponse := request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+f.tenantID+"/runs/"+runID, owner, "", nil)
	var detail struct {
		Status  string `json:"status"`
		Actions []struct {
			Kind      string `json:"kind"`
			Status    string `json:"status"`
			ContactID string `json:"contact_id"`
			SlackTS   string `json:"slack_ts"`
		} `json:"actions"`
	}
	if err := json.NewDecoder(detailResponse.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	_ = detailResponse.Body.Close()
	if detailResponse.StatusCode != 200 || detail.Status != "succeeded" || len(detail.Actions) != 2 {
		t.Fatalf("run detail status=%d payload=%+v", detailResponse.StatusCode, detail)
	}
	ui := request(t, app.client, "GET", app.server.URL+"/ui/tenants/"+f.tenantID+"/runs/"+runID, owner, "", nil)
	uiBody, err := io.ReadAll(ui.Body)
	_ = ui.Body.Close()
	if err != nil || ui.StatusCode != 200 || !strings.Contains(string(uiBody), ts) || !strings.Contains(string(uiBody), contactID) {
		t.Fatalf("run UI missing provider refs status=%d err=%v", ui.StatusCode, err)
	}
	t.Logf("simulated Contact writes=%d Contact=%s Slack messages=%d channel=%s ts=%s run=%s", v.hubspotWrites, contactID, v.slackMessages, channel, ts, runID)
}

func eventRunStatus(t *testing.T, tenant, eventID string) string {
	t.Helper()
	var status string
	_ = mig.QueryRow(context.Background(), `SELECT r.status FROM runs r JOIN planning_jobs j ON j.tenant_id=r.tenant_id AND j.id=r.planning_job_id JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id WHERE r.tenant_id=$1 AND e.event_id=$2`, tenant, eventID).Scan(&status)
	return status
}
func deliverBusiness(t *testing.T, f fixture, v *providerWorld, kind string) string {
	t.Helper()
	id := "evt_" + randomHex(8)
	raw := billingPayload(id, kind, v.customer, v.invoice, v.subscription)
	if got := post(t, f, raw, time.Now(), f.secret); got != 202 {
		t.Fatalf("webhook status %d", got)
	}
	return id
}
func waitBusinessStatus(t *testing.T, tenant, eventID, status string) {
	t.Helper()
	await(t, func() bool { return eventRunStatus(t, tenant, eventID) == status })
}
func actionCount(t *testing.T, tenant, kind string) int {
	t.Helper()
	var n int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_actions WHERE tenant_id=$1 AND kind=$2`, tenant, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func externalCounts(v *providerWorld) (int, int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.hubspotWrites, v.slackMessages
}
func managedState(v *providerWorld) map[string]string {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, p := range v.contacts {
		copy := map[string]string{}
		for k, value := range p {
			copy[k] = value
		}
		return copy
	}
	return nil
}

func TestBusinessDuplicateDeliveryAndInvoiceDedup(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	id := "evt_" + randomHex(8)
	raw := billingPayload(id, "invoice.paid", v.customer, v.invoice, v.subscription)
	var wg sync.WaitGroup
	errs := make(chan int, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sig := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: raw, Secret: f.secret, Timestamp: time.Now()})
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/webhooks/stripe/"+f.ref, strings.NewReader(string(raw)))
			req.Header.Set("Stripe-Signature", sig.Header)
			resp, e := http.DefaultClient.Do(req)
			if e != nil {
				errs <- 0
				return
			}
			_ = resp.Body.Close()
			errs <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(errs)
	for code := range errs {
		if code != 200 && code != 202 {
			t.Fatalf("duplicate delivery status %d", code)
		}
	}
	waitBusinessStatus(t, f.tenantID, id, "succeeded")
	if actionCount(t, f.tenantID, "crm_projection") != 1 || actionCount(t, f.tenantID, "slack_invoice_paid") != 1 {
		t.Fatal("duplicate event created actions")
	}
	id2 := deliverBusiness(t, f, v, "invoice.paid")
	waitBusinessStatus(t, f.tenantID, id2, "succeeded")
	if actionCount(t, f.tenantID, "crm_projection") != 2 || actionCount(t, f.tenantID, "slack_invoice_paid") != 1 {
		t.Fatal("invoice effect key did not deduplicate across event IDs")
	}
	writes, messages := externalCounts(v)
	if writes != 1 || messages != 1 {
		t.Fatalf("external effects: contact writes=%d messages=%d", writes, messages)
	}
}
func TestBusinessCurrentStateAndSubscriptionOnly(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	id := deliverBusiness(t, f, v, "invoice.payment_failed")
	waitBusinessStatus(t, f.tenantID, id, "succeeded")
	firstSyncedAt := managedState(v)["relaydesk_synced_at"]
	id = deliverBusiness(t, f, v, "customer.subscription.updated")
	waitBusinessStatus(t, f.tenantID, id, "succeeded")
	p := managedState(v)
	if p["relaydesk_payment_state"] != "paid" {
		t.Fatalf("old webhook overwrote paid state: %v", p)
	}
	if p["relaydesk_synced_at"] != firstSyncedAt {
		t.Fatal("CRM no-op modified sync timestamp")
	}
	if actionCount(t, f.tenantID, "slack_invoice_paid") != 0 {
		t.Fatal("non-paid events created Slack action")
	}
	writes, messages := externalCounts(v)
	if writes != 1 || messages != 0 {
		t.Fatalf("writes=%d messages=%d", writes, messages)
	}
}

func TestBusinessOutOfOrderConvergence(t *testing.T) {
	orders := [][]string{
		{"invoice.payment_failed", "invoice.paid", "customer.subscription.updated"},
		{"customer.subscription.updated", "invoice.paid", "invoice.payment_failed"},
		{"invoice.paid", "invoice.payment_failed", "customer.subscription.updated"},
	}
	for i, order := range orders {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			v := newWorld()
			f, _ := seedBusiness(t, v)
			startExecutor(t, v)
			for _, kind := range order {
				id := deliverBusiness(t, f, v, kind)
				waitBusinessStatus(t, f.tenantID, id, "succeeded")
			}
			state := managedState(v)
			if state["relaydesk_payment_state"] != "paid" || state["relaydesk_latest_invoice_id"] != v.invoice {
				t.Fatalf("CRM state followed event order: %v", state)
			}
			crm, slack := externalCounts(v)
			if crm != 1 || slack != 1 {
				t.Fatalf("out-of-order effects CRM=%d Slack=%d", crm, slack)
			}
		})
	}
}

func TestBusinessTenantIsolation(t *testing.T) {
	v := newWorld()
	f1, _ := seedBusiness(t, v)
	f2, _ := seedBusiness(t, v)
	startExecutor(t, v)
	id1 := deliverBusiness(t, f1, v, "invoice.paid")
	id2 := deliverBusiness(t, f2, v, "invoice.paid")
	waitBusinessStatus(t, f1.tenantID, id1, "succeeded")
	waitBusinessStatus(t, f2.tenantID, id2, "succeeded")
	crm, slack := externalCounts(v)
	if crm != 2 || slack != 2 {
		t.Fatalf("tenant effects CRM=%d Slack=%d", crm, slack)
	}
	v.mu.Lock()
	if len(v.contacts) != 2 {
		v.mu.Unlock()
		t.Fatal("tenant Contact keys collided")
	}
	v.mu.Unlock()
	for _, tenant := range []string{f1.tenantID, f2.tenantID} {
		tx, err := store.BeginTenant(context.Background(), db.Pool, tenant)
		if err != nil {
			t.Fatal(err)
		}
		var actions, links int
		err = tx.QueryRow(context.Background(), `SELECT count(*) FROM business_actions`).Scan(&actions)
		if err == nil {
			err = tx.QueryRow(context.Background(), `SELECT count(*) FROM contact_links`).Scan(&links)
		}
		_ = tx.Rollback(context.Background())
		if err != nil || actions != 2 || links != 1 {
			t.Fatalf("tenant %s sees actions=%d links=%d err=%v", tenant, actions, links, err)
		}
	}
}

func TestBusinessConfigurationRevisionAndAuthorization(t *testing.T) {
	v := newWorld()
	f, cfg := seedBusiness(t, v)
	other, _ := seedBusiness(t, newWorld())
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	owner := login(t, app, "billing-owner-"+randomHex(4), nil)
	ownerID, csrf := me(t, app, owner)
	viewer := login(t, app, "billing-viewer-"+randomHex(4), nil)
	viewerID, viewerCSRF := me(t, app, viewer)
	_, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner'),($1,$3,'viewer')`, f.tenantID, ownerID, viewerID)
	if err != nil {
		t.Fatal(err)
	}
	startExecutor(t, v)
	firstID := deliverBusiness(t, f, v, "customer.subscription.updated")
	waitBusinessStatus(t, f.tenantID, firstID, "succeeded")
	endpoint := app.server.URL + "/v1/tenants/" + f.tenantID + "/billing-sync"
	closeResponse(t, request(t, app.client, "POST", endpoint, viewer, viewerCSRF, cfg), 403)
	invalid := cfg
	invalid.HubSpotConnectionID = other.connectionID
	closeResponse(t, request(t, app.client, "POST", endpoint, owner, csrf, invalid), 400)
	cfg.ActivationTime = time.Now().Add(-5 * time.Second).UTC()
	response := request(t, app.client, "POST", endpoint, owner, csrf, cfg)
	var created struct {
		RevisionID string `json:"revision_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 201 || created.RevisionID == "" || created.RevisionID == f.revisionID {
		t.Fatalf("revision response %d %#v", response.StatusCode, created)
	}
	closeResponse(t, request(t, app.client, "GET", endpoint+"/"+f.connectionID, viewer, "", nil), 200)
	_, err = mig.Exec(context.Background(), `UPDATE automation_revisions SET config='{}'::jsonb WHERE id=$1`, f.revisionID)
	if err == nil {
		t.Fatal("activated revision was mutable")
	}
	repeated := billingPayload(firstID, "customer.subscription.updated", v.customer, v.invoice, v.subscription)
	if code := post(t, f, repeated, time.Now(), f.secret); code != 200 {
		t.Fatalf("duplicate event response %d", code)
	}
	secondID := deliverBusiness(t, f, v, "customer.subscription.updated")
	waitBusinessStatus(t, f.tenantID, secondID, "succeeded")
	for _, tc := range []struct{ eventID, revisionID string }{{firstID, f.revisionID}, {secondID, created.RevisionID}} {
		var actual string
		if err := mig.QueryRow(context.Background(), `SELECT automation_revision_id FROM webhook_events WHERE tenant_id=$1 AND event_id=$2`, f.tenantID, tc.eventID).Scan(&actual); err != nil || actual != tc.revisionID {
			t.Fatalf("event %s revision=%s want=%s err=%v", tc.eventID, actual, tc.revisionID, err)
		}
	}
	var changed int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND operation='automation.configure'`, f.tenantID).Scan(&changed); err != nil || changed != 1 {
		t.Fatalf("configuration audit entries=%d err=%v", changed, err)
	}
}
func TestBusinessTrialAndCancellation(t *testing.T) {
	cases := []struct {
		name, status, invoice     string
		scheduled                 bool
		ended                     int64
		wantPayment, wantCanceled string
	}{
		{"trial", "trialing", "open", false, 0, "unknown", "false"},
		{"unpaid", "unpaid", "open", false, 0, "payment_failed", "false"},
		{"scheduled", "active", "paid", true, 0, "paid", "false"},
		{"completed", "canceled", "paid", true, 1700000000, "paid", "true"},
		{"zero_amount_paid", "active", "paid", false, 0, "paid", "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newWorld()
			v.subscriptionStatus = tc.status
			v.invoiceStatus = tc.invoice
			v.cancelAtPeriodEnd = tc.scheduled
			v.endedAt = tc.ended
			f, _ := seedBusiness(t, v)
			startExecutor(t, v)
			id := deliverBusiness(t, f, v, "customer.subscription.updated")
			waitBusinessStatus(t, f.tenantID, id, "succeeded")
			p := managedState(v)
			if p["relaydesk_subscription_status"] != tc.status || p["relaydesk_payment_state"] != tc.wantPayment || p["relaydesk_effectively_canceled"] != tc.wantCanceled || p["relaydesk_cancel_at_period_end"] != fmt.Sprint(tc.scheduled) {
				t.Fatalf("projection mismatch: %v", p)
			}
			_, messages := externalCounts(v)
			if messages != 0 {
				t.Fatal("subscription update sent Slack message")
			}
		})
	}
}
func TestBusinessHubSpotLostResponse(t *testing.T) {
	v := newWorld()
	v.hubspotLost = true
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	id := deliverBusiness(t, f, v, "invoice.paid")
	waitBusinessStatus(t, f.tenantID, id, "succeeded")
	writes, messages := externalCounts(v)
	if writes != 1 || messages != 1 {
		t.Fatalf("ambiguous CRM recovery duplicated or lost effects: %d/%d", writes, messages)
	}
}
func TestBusinessHubSpotOutageThenRecovery(t *testing.T) {
	v := newWorld()
	v.hubspotUnavailable = true
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	id := deliverBusiness(t, f, v, "invoice.paid")
	waitBusinessStatus(t, f.tenantID, id, "retry_wait")
	crm, slack := externalCounts(v)
	if crm != 0 || slack != 0 {
		t.Fatalf("outage produced effects CRM=%d Slack=%d", crm, slack)
	}
	v.mu.Lock()
	v.hubspotUnavailable = false
	v.mu.Unlock()
	waitBusinessStatus(t, f.tenantID, id, "succeeded")
	crm, slack = externalCounts(v)
	if crm != 1 || slack != 1 {
		t.Fatalf("recovery duplicated or lost effects CRM=%d Slack=%d", crm, slack)
	}
	var attempts int
	if err := mig.QueryRow(context.Background(), `SELECT attempts FROM business_actions WHERE tenant_id=$1 AND kind='crm_projection'`, f.tenantID).Scan(&attempts); err != nil || attempts < 2 {
		t.Fatalf("CRM retry attempts=%d err=%v", attempts, err)
	}
	t.Logf("simulated HubSpot outage recovered: CRM attempts=%d, Contact writes=%d, Slack messages=%d", attempts, crm, slack)
}
func TestBusinessSlackLostResponse(t *testing.T) {
	v := newWorld()
	v.slackLost = true
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	id := deliverBusiness(t, f, v, "invoice.paid")
	waitBusinessStatus(t, f.tenantID, id, "unknown")
	_, messages := externalCounts(v)
	if messages != 1 {
		t.Fatalf("external messages=%d", messages)
	}
	time.Sleep(1500 * time.Millisecond)
	_, messages = externalCounts(v)
	if messages != 1 {
		t.Fatal("unknown Slack action retried automatically")
	}
	var crm, slack string
	err := mig.QueryRow(context.Background(), `SELECT max(CASE WHEN kind='crm_projection' THEN status END),max(CASE WHEN kind='slack_invoice_paid' THEN status END) FROM business_actions WHERE tenant_id=$1`, f.tenantID).Scan(&crm, &slack)
	if err != nil || crm != "succeeded" || slack != "unknown" {
		t.Fatalf("crm=%s slack=%s err=%v", crm, slack, err)
	}
}
func TestBusinessMultipleSubscriptionsNeedsAttention(t *testing.T) {
	v := newWorld()
	v.multiple = true
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	id := deliverBusiness(t, f, v, "customer.subscription.updated")
	waitBusinessStatus(t, f.tenantID, id, "needs_attention")
	writes, messages := externalCounts(v)
	if writes != 0 || messages != 0 {
		t.Fatalf("unsafe effects with ambiguous subscriptions %d/%d", writes, messages)
	}
}
func TestBusinessPauseResume(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	_, err := mig.Exec(context.Background(), `INSERT INTO automation_gates(tenant_id,stripe_connection_id,paused) VALUES($1,$2,true)`, f.tenantID, f.connectionID)
	if err != nil {
		t.Fatal(err)
	}
	startExecutor(t, v)
	id := deliverBusiness(t, f, v, "invoice.paid")
	await(t, func() bool { return actionCount(t, f.tenantID, "crm_projection") == 1 })
	time.Sleep(500 * time.Millisecond)
	writes, messages := externalCounts(v)
	if writes != 0 || messages != 0 {
		t.Fatal("paused automation sent effects")
	}
	_, err = mig.Exec(context.Background(), `UPDATE automation_gates SET paused=false WHERE tenant_id=$1 AND stripe_connection_id=$2`, f.tenantID, f.connectionID)
	if err != nil {
		t.Fatal(err)
	}
	waitBusinessStatus(t, f.tenantID, id, "succeeded")
}
func TestBusinessTwoExecutors(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	startExecutor(t, v)
	id := deliverBusiness(t, f, v, "invoice.paid")
	waitBusinessStatus(t, f.tenantID, id, "succeeded")
	writes, messages := externalCounts(v)
	if writes != 1 || messages != 1 {
		t.Fatalf("two workers duplicated effects %d/%d", writes, messages)
	}
}

func TestBusinessMappingConflicts(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		schema, collision, missingMapped, explicit bool
		want                                       string
	}{
		{name: "explicit_contact", explicit: true, want: "succeeded"},
		{name: "missing_mapped_contact", missingMapped: true, want: "needs_attention"},
		{name: "missing_managed_property", schema: true, want: "blocked_config"},
		{name: "email_identity_collision", collision: true, want: "needs_attention"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newWorld()
			v.schemaMissing = tc.schema
			v.emailCollision = tc.collision
			f, cfg := seedBusiness(t, v)
			key, err := business.CustomerKey(f.account, "test", v.customer)
			if err != nil {
				t.Fatal(err)
			}
			if tc.explicit || tc.missingMapped {
				_, err = mig.Exec(context.Background(), `INSERT INTO contact_links(tenant_id,customer_key,hubspot_connection_id,contact_id,source) VALUES($1,$2,$3,'101','explicit')`, f.tenantID, key, cfg.HubSpotConnectionID)
				if err != nil {
					t.Fatal(err)
				}
				if tc.explicit {
					v.contacts["unlinked"] = map[string]string{}
				}
			}
			startExecutor(t, v)
			id := deliverBusiness(t, f, v, "customer.subscription.updated")
			waitBusinessStatus(t, f.tenantID, id, tc.want)
			writes, messages := externalCounts(v)
			if messages != 0 {
				t.Fatal("unexpected Slack message")
			}
			if tc.explicit && writes != 1 {
				t.Fatalf("explicit Contact write count=%d", writes)
			}
			if tc.want == "needs_attention" && writes != 0 {
				t.Fatalf("conflict wrote Contact %d times", writes)
			}
		})
	}
}

func TestBusinessMergedContactRebind(t *testing.T) {
	for _, tc := range []struct {
		name, managedKey   string
		changeProjection   bool
		wantStatus, wantID string
		wantWrites         int
	}{
		{name: "same_projection", wantStatus: "succeeded", wantID: "202", wantWrites: 1},
		{name: "changed_projection", changeProjection: true, wantStatus: "succeeded", wantID: "202", wantWrites: 2},
		{name: "missing_identity_key", managedKey: "", wantStatus: "needs_attention", wantID: "101", wantWrites: 1},
		{name: "wrong_identity_key", managedKey: "other", wantStatus: "needs_attention", wantID: "101", wantWrites: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newWorld()
			f, cfg := seedBusiness(t, v)
			startExecutor(t, v)
			first := deliverBusiness(t, f, v, "customer.subscription.updated")
			waitBusinessStatus(t, f.tenantID, first, "succeeded")
			key, err := business.CustomerKey(f.account, "test", v.customer)
			if err != nil {
				t.Fatal(err)
			}
			v.mu.Lock()
			v.mergedContactID = "202"
			if tc.changeProjection {
				v.cancelAtPeriodEnd = true
			}
			if tc.name == "missing_identity_key" || tc.name == "wrong_identity_key" {
				v.contacts[key]["relaydesk_customer_key"] = tc.managedKey
			}
			v.mu.Unlock()
			second := deliverBusiness(t, f, v, "customer.subscription.updated")
			waitBusinessStatus(t, f.tenantID, second, tc.wantStatus)
			var linkedID string
			if err := mig.QueryRow(context.Background(), `SELECT contact_id FROM contact_links WHERE tenant_id=$1 AND customer_key=$2 AND hubspot_connection_id=$3`,
				f.tenantID, key, cfg.HubSpotConnectionID).Scan(&linkedID); err != nil {
				t.Fatal(err)
			}
			if linkedID != tc.wantID {
				t.Fatalf("linked Contact=%s want=%s", linkedID, tc.wantID)
			}
			writes, messages := externalCounts(v)
			if writes != tc.wantWrites || messages != 0 {
				t.Fatalf("effects Contact=%d Slack=%d, want Contact=%d Slack=0", writes, messages, tc.wantWrites)
			}
		})
	}
}

func TestBusinessProviderContractFailures(t *testing.T) {
	for _, tc := range []struct {
		name, partial, slackError string
		archived                  bool
		want                      string
	}{
		{"hubspot_partial_validation", "VALIDATION_ERROR", "", false, "blocked_config"},
		{"hubspot_partial_conflict", "CONFLICT", "", false, "needs_attention"},
		{"slack_ok_false", "", "channel_not_found", false, "needs_attention"},
		{"archived_contact", "", "", true, "needs_attention"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newWorld()
			v.hubspotPartial = tc.partial
			v.slackError = tc.slackError
			v.archivedContact = tc.archived
			f, _ := seedBusiness(t, v)
			if tc.archived {
				key, err := business.CustomerKey(f.account, "test", v.customer)
				if err != nil {
					t.Fatal(err)
				}
				v.contacts[key] = map[string]string{"relaydesk_customer_key": key}
			}
			startExecutor(t, v)
			id := deliverBusiness(t, f, v, "invoice.paid")
			waitBusinessStatus(t, f.tenantID, id, tc.want)
			crm, slack := externalCounts(v)
			if slack != 0 || (tc.slackError == "" && crm != 0) || (tc.slackError != "" && crm != 1) {
				t.Fatalf("unsafe external effects CRM=%d Slack=%d", crm, slack)
			}
		})
	}
}

func TestBusinessDurable429Retry(t *testing.T) {
	for _, provider := range []string{"hubspot", "slack"} {
		t.Run(provider, func(t *testing.T) {
			v := newWorld()
			if provider == "hubspot" {
				v.hubspot429 = 1
			} else {
				v.slack429 = 1
			}
			f, _ := seedBusiness(t, v)
			startExecutor(t, v)
			id := deliverBusiness(t, f, v, "invoice.paid")
			waitBusinessStatus(t, f.tenantID, id, "succeeded")
			writes, messages := externalCounts(v)
			if writes != 1 || messages != 1 {
				t.Fatalf("effects after retry %d/%d", writes, messages)
			}
			var attempts int
			err := mig.QueryRow(context.Background(), `SELECT attempts FROM business_actions WHERE tenant_id=$1 AND kind=$2`, f.tenantID, map[bool]string{true: "crm_projection", false: "slack_invoice_paid"}[provider == "hubspot"]).Scan(&attempts)
			if err != nil || attempts != 2 {
				t.Fatalf("provider %s attempts=%d err=%v", provider, attempts, err)
			}
			var rateLimited int
			err = mig.QueryRow(context.Background(), `SELECT count(*) FROM business_provider_calls WHERE tenant_id=$1 AND response_status=429`, f.tenantID).Scan(&rateLimited)
			if err != nil || rateLimited != 1 {
				t.Fatalf("429 call records=%d err=%v", rateLimited, err)
			}
			var limitedAt, nextWriteAt time.Time
			err = mig.QueryRow(context.Background(), `SELECT
 max(finished_at) FILTER (WHERE response_status=429),
 min(request_started_at) FILTER (WHERE response_status=200 AND method IN ('POST','PATCH'))
 FROM business_provider_calls WHERE tenant_id=$1 AND provider=$2`, f.tenantID, provider).Scan(&limitedAt, &nextWriteAt)
			if err != nil || nextWriteAt.Sub(limitedAt) < time.Second {
				t.Fatalf("provider %s wrote before Retry-After: delta=%s err=%v", provider, nextWriteAt.Sub(limitedAt), err)
			}
		})
	}
}

func TestBusinessCrashBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, event, kind, stage, wantStatus string
		wantCRM, wantSlack                   int
	}{
		{"crm_before_outbound", "customer.subscription.updated", "crm_projection", "before_outbound", "succeeded", 1, 0},
		{"crm_after_request_mark", "customer.subscription.updated", "crm_projection", "after_request_started", "succeeded", 1, 0},
		{"crm_after_confirmation", "customer.subscription.updated", "crm_projection", "after_crm_confirmed", "succeeded", 1, 0},
		{"slack_after_request_mark", "invoice.paid", "slack_invoice_paid", "after_request_started", "unknown", 1, 0},
		{"slack_after_response", "invoice.paid", "slack_invoice_paid", "after_slack_response", "unknown", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newWorld()
			f, _ := seedBusiness(t, v)
			stopped := make(chan struct{}, 1)
			var once sync.Once
			cancel := startExecutorWithFault(t, v, func(stage, tenant, actionID string) bool {
				if stage != tc.stage || tenant != f.tenantID {
					return false
				}
				var kind string
				if err := mig.QueryRow(context.Background(), `SELECT kind FROM business_actions WHERE tenant_id=$1 AND id=$2`, tenant, actionID).Scan(&kind); err != nil || kind != tc.kind {
					return false
				}
				once.Do(func() { stopped <- struct{}{} })
				return true
			})
			id := deliverBusiness(t, f, v, tc.event)
			select {
			case <-stopped:
			case <-time.After(15 * time.Second):
				t.Fatal("fault boundary not reached")
			}
			cancel()
			_, err := mig.Exec(context.Background(), `UPDATE business_actions SET lease_until=now()-interval '1 second' WHERE tenant_id=$1 AND kind=$2 AND status='running'`, f.tenantID, tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			_, err = mig.Exec(context.Background(), `UPDATE crm_aggregate_leases SET lease_until=now()-interval '1 second' WHERE tenant_id=$1`, f.tenantID)
			if err != nil {
				t.Fatal(err)
			}
			startExecutor(t, v)
			waitBusinessStatus(t, f.tenantID, id, tc.wantStatus)
			crm, slack := externalCounts(v)
			if crm != tc.wantCRM || slack != tc.wantSlack {
				t.Fatalf("effects after recovery CRM=%d Slack=%d, want %d/%d", crm, slack, tc.wantCRM, tc.wantSlack)
			}
			var actions int
			if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM business_actions WHERE tenant_id=$1 AND kind=$2 AND status=$3`, f.tenantID, tc.kind, tc.wantStatus).Scan(&actions); err != nil || actions != 1 {
				t.Fatalf("recovered action count=%d err=%v", actions, err)
			}
		})
	}
}
