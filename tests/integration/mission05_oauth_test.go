//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type providerTransport func(*http.Request) (*http.Response, error)

func (f providerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fakeProviderBody(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestMission05BrowserProviderStartAndHTMLCallback(t *testing.T) {
	app := newTestApp(t, newFakeOIDC(t))
	hubID := int(time.Now().UnixNano()%900_000_000 + 100_000_000)
	account := strconv.Itoa(hubID)
	app.service.ProviderHTTP = providerSimulator(t, hubID, "oauth crm.objects.contacts.read crm.objects.contacts.write")
	session := login(t, app, uuid.NewString(), nil)
	_, csrf := me(t, app, session)
	tenant := createWorkspace(t, app, session, csrf, "Browser OAuth")
	form := url.Values{"expected_account_id": {account}, "csrf_token": {csrf}}
	req, err := http.NewRequest(http.MethodPost, app.server.URL+"/ui/tenants/"+tenant+"/connections/oauth/hubspot", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	resp, err := app.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("browser start status=%d content-type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	match := regexp.MustCompile(`href="(https?[^"]+)"`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatal("authorization link missing")
	}
	authURL, err := url.Parse(html.UnescapeString(string(match[1])))
	if err != nil || authURL.Query().Get("redirect_uri") != app.server.URL+"/oauth/hubspot/callback" || authURL.Query().Get("state") == "" {
		t.Fatal("browser authorization link invalid")
	}
	var flow *http.Cookie
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "relaydesk_hubspot_oauth_state" {
			flow = cookie
		}
	}
	if flow == nil {
		t.Fatal("browser flow cookie missing")
	}
	callback, err := http.NewRequest(http.MethodGet, app.server.URL+"/oauth/hubspot/callback?state="+url.QueryEscape(authURL.Query().Get("state"))+"&code=sim-code", nil)
	if err != nil {
		t.Fatal(err)
	}
	callback.Header.Set("Accept", "text/html,application/xhtml+xml")
	callback.AddCookie(session)
	callback.AddCookie(flow)
	completed, err := app.client.Do(callback)
	if err != nil {
		t.Fatal(err)
	}
	defer completed.Body.Close()
	content, _ := io.ReadAll(completed.Body)
	if completed.StatusCode != 200 || !strings.Contains(string(content), "Provider connected") || !strings.Contains(string(content), "Lenoir") || !strings.HasPrefix(completed.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("browser callback status=%d content-type=%q", completed.StatusCode, completed.Header.Get("Content-Type"))
	}
}

func providerSimulator(t *testing.T, hubID int, scopes string) *http.Client {
	t.Helper()
	return &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host + r.URL.Path {
		case "api.hubapi.com/oauth/2026-03/token":
			if r.Method != http.MethodPost || r.ParseForm() != nil || r.Form.Get("client_secret") != "hubspot-test-secret" ||
				r.Form.Get("code_verifier") == "" {
				t.Errorf("invalid HubSpot token request")
				return fakeProviderBody(400, `{}`), nil
			}
			body, _ := json.Marshal(map[string]any{"access_token": "simulated-access-secret", "refresh_token": "simulated-refresh-secret",
				"hub_id": hubID, "scopes": strings.Fields(scopes), "expires_in": 1800, "token_type": "bearer"})
			return fakeProviderBody(200, string(body)), nil
		case "api.hubapi.com/account-info/v3/details":
			if r.Header.Get("Authorization") != "Bearer simulated-access-secret" {
				t.Errorf("missing bearer token")
			}
			body, _ := json.Marshal(map[string]any{"portalId": hubID, "accountType": "DEVELOPER_TEST"})
			return fakeProviderBody(200, string(body)), nil
		default:
			t.Errorf("unexpected provider request: %s", r.URL.Host+r.URL.Path)
			return fakeProviderBody(404, `{}`), nil
		}
	})}
}

func beginHubSpot(t *testing.T, app *testApp, session *http.Cookie, csrf, tenant, expected, reconnect string) (*http.Cookie, string) {
	t.Helper()
	resp := request(t, app.client, http.MethodPost, app.server.URL+"/v1/tenants/"+tenant+"/connections/oauth/hubspot",
		session, csrf, map[string]string{"expected_account_id": expected, "reconnect_id": reconnect})
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("begin status=%d %s", resp.StatusCode, body)
	}
	var out struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		t.Fatal("invalid begin response")
	}
	u, err := url.Parse(out.AuthorizationURL)
	if err != nil || u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("redirect_uri") != app.server.URL+"/oauth/hubspot/callback" {
		t.Fatal("OAuth URL/PKCE/redirect mismatch")
	}
	for _, c := range resp.Cookies() {
		if c.Name == "relaydesk_hubspot_oauth_state" {
			return c, u.Query().Get("state")
		}
	}
	t.Fatal("missing provider state cookie")
	return nil, ""
}

func providerCallback(t *testing.T, app *testApp, session, flow *http.Cookie, state string, want int) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, app.server.URL+"/oauth/hubspot/callback?state="+url.QueryEscape(state)+"&code=sim-code", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(session)
	req.AddCookie(flow)
	resp, err := app.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("callback=%d want=%d body=%s", resp.StatusCode, want, body)
	}
	if want != 200 {
		return ""
	}
	var out struct {
		ConnectionID string `json:"connection_id"`
	}
	if json.Unmarshal(body, &out) != nil || out.ConnectionID == "" {
		t.Fatal("missing connection ID")
	}
	return out.ConnectionID
}

func TestMission05OAuthStateAndAccountBinding(t *testing.T) {
	app := newTestApp(t, newFakeOIDC(t))
	hubID := int(time.Now().UnixNano()%900_000_000 + 100_000_000)
	account := strconv.Itoa(hubID)
	app.service.ProviderHTTP = providerSimulator(t, hubID, "oauth crm.objects.contacts.read crm.objects.contacts.write")
	session := login(t, app, uuid.NewString(), nil)
	_, csrf := me(t, app, session)
	tenant := createWorkspace(t, app, session, csrf, "OAuth test")
	flow, state := beginHubSpot(t, app, session, csrf, tenant, account, "")
	providerCallback(t, app, session, flow, "wrong"+state, 400)
	id := providerCallback(t, app, session, flow, state, 200)
	providerCallback(t, app, session, flow, state, 400)
	var raw []byte
	if err := mig.QueryRow(context.Background(), `SELECT ciphertext FROM connection_credentials WHERE connection_id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "simulated-access-secret") || strings.Contains(string(raw), "simulated-refresh-secret") {
		t.Fatal("credential stored in plaintext")
	}
	if _, err := appDB.Pool.Exec(context.Background(), `SELECT ciphertext FROM connection_credentials WHERE connection_id=$1`, id); err == nil {
		t.Fatal("browser role read credential bytes")
	}

	reconnectFlow, reconnectState := beginHubSpot(t, app, session, csrf, tenant, account, id)
	if got := providerCallback(t, app, session, reconnectFlow, reconnectState, 200); got != id {
		t.Fatal("same-account reconnect changed connection ID")
	}
	var generation int64
	if err := mig.QueryRow(context.Background(), `SELECT generation FROM connections WHERE id=$1`, id).Scan(&generation); err != nil || generation != 2 {
		t.Fatalf("reconnect generation=%d err=%v", generation, err)
	}
	badFlow, badState := beginHubSpot(t, app, session, csrf, tenant, "999999", "")
	providerCallback(t, app, session, badFlow, badState, 400)
	resp := request(t, app.client, http.MethodPost, app.server.URL+"/v1/tenants/"+tenant+"/connections/oauth/hubspot",
		session, csrf, map[string]string{"expected_account_id": "999999", "reconnect_id": id})
	if resp.StatusCode == 201 {
		t.Fatal("different-account reconnect initiated")
	}
	resp.Body.Close()
}

func TestMission05OAuthExpiryMembershipAndScopes(t *testing.T) {
	app := newTestApp(t, newFakeOIDC(t))
	hubID := int(time.Now().UnixNano()%900_000_000 + 100_000_000)
	account := strconv.Itoa(hubID)
	app.service.ProviderHTTP = providerSimulator(t, hubID, "oauth crm.objects.contacts.read crm.objects.contacts.write")
	session := login(t, app, uuid.NewString(), nil)
	user, csrf := me(t, app, session)
	tenant := createWorkspace(t, app, session, csrf, "OAuth failures")
	flow, state := beginHubSpot(t, app, session, csrf, tenant, account, "")
	h := sha256.Sum256([]byte(state))
	if _, err := mig.Exec(context.Background(), `UPDATE provider_oauth_flows SET expires_at=now()-interval '1 second' WHERE state_hash=$1`, h[:]); err != nil {
		t.Fatal(err)
	}
	providerCallback(t, app, session, flow, state, 400)
	flow, state = beginHubSpot(t, app, session, csrf, tenant, account, "")
	if _, err := mig.Exec(context.Background(), `UPDATE memberships SET status='removed' WHERE tenant_id=$1 AND user_id=$2`, tenant, user); err != nil {
		t.Fatal(err)
	}
	providerCallback(t, app, session, flow, state, 400)
	if _, err := mig.Exec(context.Background(), `UPDATE memberships SET status='active' WHERE tenant_id=$1 AND user_id=$2`, tenant, user); err != nil {
		t.Fatal(err)
	}
	app.service.ProviderHTTP = providerSimulator(t, hubID, "oauth crm.objects.contacts.read")
	flow, state = beginHubSpot(t, app, session, csrf, tenant, account, "")
	providerCallback(t, app, session, flow, state, 400)
	var n int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM connections WHERE tenant_id=$1`, tenant).Scan(&n); err != nil || n != 0 {
		t.Fatalf("invalid flow created connection: %d %v", n, err)
	}
}
