//go:build integration

package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/lenoir-dev/LenoirDesk/internal/business"

	"github.com/stripe/stripe-go/v86"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMission06BrowserOnboardingAndIsolation(t *testing.T) {
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	owner := login(t, app, "mission06-owner", nil)
	ownerID, csrf := me(t, app, owner)
	_ = ownerID
	tenant := createWorkspace(t, app, owner, csrf, "<script>private</script>")
	for _, path := range []string{"", "/connections", "/billing-sync", "/runs"} {
		resp := request(t, app.client, "GET", app.server.URL+"/ui/tenants/"+tenant+path, owner, "", nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s status=%d", path, resp.StatusCode)
		}
		if strings.Contains(string(body), "<script>private</script>") {
			t.Fatal("workspace name was not escaped")
		}
	}
	setup := request(t, app.client, "GET", app.server.URL+"/ui/tenants/"+tenant+"/connections/stripe/new", owner, "", nil)
	setupBody, _ := io.ReadAll(setup.Body)
	setup.Body.Close()
	if setup.StatusCode != 200 {
		t.Fatalf("setup status=%d", setup.StatusCode)
	}
	match := regexp.MustCompile(`name="webhook_ref" value="([0-9a-f]{64})"`).FindSubmatch(setupBody)
	if len(match) != 2 {
		t.Fatal("missing opaque webhook reference")
	}
	key := "rk_test_" + strings.Repeat("a", 30)
	secret := "whsec_" + strings.Repeat("b", 30)
	account := "acct_" + randomHex(8)
	reads := 0
	app.service.ProviderHTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.stripe.com" || req.Method != "GET" || req.Header.Get("Authorization") != "Bearer "+key || req.Header.Get("Stripe-Version") != stripe.APIVersion {
			t.Errorf("unsafe Stripe verification request")
		}
		reads++
		body := `{"data":[]}`
		if req.URL.Path == "/v1/account" {
			body = `{"id":"` + account + `"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	form := url.Values{"csrf_token": {csrf}, "expected_account_id": {account}, "restricted_key": {key}, "webhook_signing_secret": {secret}, "webhook_ref": {string(match[1])}}
	req, err := http.NewRequest("POST", app.server.URL+"/ui/tenants/"+tenant+"/connections/stripe", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	connected, err := app.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	connected.Body.Close()
	if connected.StatusCode != 303 || reads != 4 {
		t.Fatalf("connect status=%d reads=%d", connected.StatusCode, reads)
	}
	connectionList := request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+tenant+"/connections", owner, "", nil)
	listBody, _ := io.ReadAll(connectionList.Body)
	connectionList.Body.Close()
	if connectionList.StatusCode != 200 || !strings.Contains(string(listBody), account) || strings.Contains(string(listBody), key) || strings.Contains(string(listBody), secret) {
		t.Fatal("connection metadata or secret disclosure invalid")
	}
	viewer := login(t, app, "mission06-viewer", nil)
	viewerID, viewerCSRF := me(t, app, viewer)
	grant := request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+tenant+"/members", owner, csrf, map[string]string{"user_id": viewerID, "role": "viewer"})
	closeResponse(t, grant, 204)
	for _, path := range []string{"", "/connections", "/billing-sync", "/runs"} {
		resp := request(t, app.client, "GET", app.server.URL+"/ui/tenants/"+tenant+path, viewer, "", nil)
		closeResponse(t, resp, 200)
	}
	forbidden := request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+tenant+"/connections/stripe-test", viewer, viewerCSRF, map[string]string{"expected_account_id": account, "restricted_key": key, "webhook_signing_secret": secret, "webhook_ref": string(match[1])})
	if forbidden.StatusCode != 403 {
		t.Fatalf("viewer Stripe write status=%d", forbidden.StatusCode)
	}
	forbidden.Body.Close()
	other := createWorkspace(t, app, owner, csrf, "Other")
	cross := request(t, app.client, "GET", app.server.URL+"/ui/tenants/"+other, viewer, "", nil)
	if cross.StatusCode != 403 {
		t.Fatalf("cross-tenant overview status=%d", cross.StatusCode)
	}
	cross.Body.Close()
	noCSRF := request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+tenant+"/billing-sync", owner, "", map[string]string{})
	if noCSRF.StatusCode != 403 {
		t.Fatalf("missing CSRF status=%d", noCSRF.StatusCode)
	}
	noCSRF.Body.Close()
}

func postUIForm(t *testing.T, app *testApp, cookie *http.Cookie, path string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, app.server.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := app.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMission06MappingReviewBindAndCRMRetry(t *testing.T) {
	v := newWorld()
	f, cfg := seedBusiness(t, v)
	key, err := business.CustomerKey(f.account, "test", v.customer)
	if err != nil {
		t.Fatal(err)
	}
	v.contacts[key] = map[string]string{"relaydesk_customer_key": "stripe:acct_other:test:cus_other"}
	hub := httptest.NewServer(http.HandlerFunc(v.hubspot))
	defer hub.Close()
	app := newTestAppHubSpot(t, newFakeOIDC(t), hub.URL)
	owner := login(t, app, "mission06-map-"+randomHex(5), nil)
	ownerID, csrf := me(t, app, owner)
	if _, err = mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	connections := request(t, app.client, "GET", app.server.URL+"/ui/tenants/"+f.tenantID+"/connections", owner, "", nil)
	connectionPage, _ := io.ReadAll(connections.Body)
	connections.Body.Close()
	if connections.StatusCode != 200 || strings.Count(string(connectionPage), "Local simulator fixture") != 3 {
		t.Fatal("simulation credentials must be identified as local fixtures")
	}
	var hubspotAccount, slackAccount string
	if err = mig.QueryRow(context.Background(), `SELECT provider_account_id FROM connections WHERE tenant_id=$1 AND id=$2`, f.tenantID, cfg.HubSpotConnectionID).Scan(&hubspotAccount); err != nil {
		t.Fatal(err)
	}
	if err = mig.QueryRow(context.Background(), `SELECT provider_account_id FROM connections WHERE tenant_id=$1 AND id=$2`, f.tenantID, cfg.SlackConnectionID).Scan(&slackAccount); err != nil {
		t.Fatal(err)
	}
	previewForm := url.Values{"csrf_token": {csrf}, "stripe": {cfg.StripeConnectionID}, "hubspot": {cfg.HubSpotConnectionID}, "slack": {cfg.SlackConnectionID}, "channel": {cfg.SlackChannelID}, "prices": {v.price}, "policy": {cfg.CreationPolicy}}
	configuration := postUIForm(t, app, owner, "/ui/tenants/"+f.tenantID+"/billing-sync/preview", previewForm)
	configurationPage, _ := io.ReadAll(configuration.Body)
	configuration.Body.Close()
	if configuration.StatusCode != 200 || !strings.Contains(string(configurationPage), f.account+" · simulation") || !strings.Contains(string(configurationPage), hubspotAccount+" · simulation") || !strings.Contains(string(configurationPage), slackAccount+" · simulation") {
		t.Fatal("automation preview omitted provider account identities or environment")
	}
	startExecutor(t, v)
	event := deliverBusiness(t, f, v, "customer.subscription.updated")
	waitBusinessStatus(t, f.tenantID, event, "needs_attention")
	v.mu.Lock()
	v.contacts[key]["relaydesk_customer_key"] = key
	v.mu.Unlock()
	var runID, actionID string
	var generation int64
	err = mig.QueryRow(context.Background(), `SELECT r.id,a.id,a.generation FROM runs r JOIN business_run_actions ra ON ra.tenant_id=r.tenant_id AND ra.run_id=r.id JOIN business_actions a ON a.tenant_id=ra.tenant_id AND a.id=ra.action_id WHERE r.tenant_id=$1 AND a.kind='crm_projection' ORDER BY r.created_at DESC LIMIT 1`, f.tenantID).Scan(&runID, &actionID, &generation)
	if err != nil {
		t.Fatal(err)
	}
	path := "/ui/tenants/" + f.tenantID + "/runs/" + runID + "/mapping"
	preview := request(t, app.client, "GET", app.server.URL+path+"?contact_id=101", owner, "", nil)
	page, _ := io.ReadAll(preview.Body)
	preview.Body.Close()
	if preview.StatusCode != 200 || !strings.Contains(string(page), "Identity: Matching customer key") || !strings.Contains(string(page), "Bind Contact") {
		t.Fatalf("mapping preview status=%d missing=%t conflict=%t unavailable=%t", preview.StatusCode,
			strings.Contains(string(page), "Contact missing or archived"), strings.Contains(string(page), "Contact identity conflicts"), strings.Contains(string(page), "Contact preview unavailable"))
	}
	form := url.Values{"csrf_token": {csrf}, "contact_id": {"101"}, "reason": {"Verified exact managed key against local Contact"}}
	bound := postUIForm(t, app, owner, path, form)
	bound.Body.Close()
	if bound.StatusCode != 303 {
		t.Fatalf("bind status=%d", bound.StatusCode)
	}
	var contactID, source string
	if err = mig.QueryRow(context.Background(), `SELECT contact_id,source FROM contact_links WHERE tenant_id=$1 AND hubspot_connection_id=$2 AND customer_key=$3`, f.tenantID, cfg.HubSpotConnectionID, key).Scan(&contactID, &source); err != nil {
		t.Fatal(err)
	}
	if contactID != "101" || source != "explicit" {
		t.Fatal("mapping not persisted")
	}
	var audit int
	if err = mig.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND operation='contact.link' AND context->>'reason'=$2`, f.tenantID, form.Get("reason")).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if audit != 1 {
		t.Fatal("mapping audit reason missing")
	}
	retry := postUIForm(t, app, owner, "/ui/tenants/"+f.tenantID+"/runs/"+runID+"/actions/"+actionID+"/recover", url.Values{"csrf_token": {csrf}, "generation": {strconv.FormatInt(generation, 10)}, "decision": {"manual_retry"}, "reason": {"Mapped exact Contact identity and cleared simulated conflict"}})
	retry.Body.Close()
	if retry.StatusCode != 303 {
		t.Fatalf("retry status=%d", retry.StatusCode)
	}
	waitBusinessStatus(t, f.tenantID, event, "succeeded")
	writes, messages := externalCounts(v)
	if writes != 1 || messages != 0 {
		t.Fatalf("unexpected effects: crm=%d slack=%d", writes, messages)
	}
}

func TestMission06RunHistoryBoundedCursorAndFilter(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	app := newTestApp(t, newFakeOIDC(t))
	owner := login(t, app, "mission06-history-"+randomHex(5), nil)
	ownerID, _ := me(t, app, owner)
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	// Insert persisted, correctly linked records without starting a worker. The
	// history screen must stay bounded even if scheduling has a large backlog.
	_, err := mig.Exec(context.Background(), `WITH events AS (
 INSERT INTO webhook_events(tenant_id,connection_id,provider,provider_account_id,mode,event_id,event_type,
  event_api_version,raw_payload,classification,automation_revision_id,correlation_id)
 SELECT $1,$2,'stripe',$3,'test','evt_page_'||g.i,'customer.subscription.updated',$4,
  '{}'::bytea,'supported',$5,'history fixture' FROM generate_series(1,31) AS g(i)
 RETURNING tenant_id,id,automation_revision_id
 ), jobs AS (
 INSERT INTO planning_jobs(tenant_id,event_row_id,automation_revision_id)
 SELECT tenant_id,id,automation_revision_id FROM events
 RETURNING tenant_id,id,automation_revision_id
 ) INSERT INTO runs(tenant_id,planning_job_id,automation_revision_id)
 SELECT tenant_id,id,automation_revision_id FROM jobs`, f.tenantID, f.connectionID, f.account, stripe.APIVersion, f.revisionID)
	if err != nil {
		t.Fatal(err)
	}
	base := app.server.URL + "/ui/tenants/" + f.tenantID + "/runs"
	first := request(t, app.client, "GET", base+"?status=in_progress", owner, "", nil)
	body, _ := io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != 200 || strings.Count(string(body), "evt_page_") != 25 {
		t.Fatalf("first history page status=%d rows=%d", first.StatusCode, strings.Count(string(body), "evt_page_"))
	}
	match := regexp.MustCompile(`href="[^"]*cursor=([A-Za-z0-9_-]+)"`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatal("history has no stable next cursor")
	}
	second := request(t, app.client, "GET", base+"?status=in_progress&cursor="+string(match[1]), owner, "", nil)
	body, _ = io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != 200 || strings.Count(string(body), "evt_page_") != 6 || strings.Contains(string(body), "Older runs") {
		t.Fatalf("second history page status=%d rows=%d", second.StatusCode, strings.Count(string(body), "evt_page_"))
	}
	empty := request(t, app.client, "GET", base+"?status=unknown", owner, "", nil)
	body, _ = io.ReadAll(empty.Body)
	empty.Body.Close()
	if empty.StatusCode != 200 || !strings.Contains(string(body), "No matching runs") {
		t.Fatal("unknown filter should show a meaningful empty state")
	}
}
