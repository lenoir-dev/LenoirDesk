//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lenoir-dev/LenoirDesk/internal/api"
	"github.com/lenoir-dev/LenoirDesk/internal/config"
	"github.com/lenoir-dev/LenoirDesk/internal/identity"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

type fakeOIDC struct {
	server         *httptest.Server
	mu             sync.RWMutex
	key            *rsa.PrivateKey
	kid            string
	browserSubject string
}

func newFakeOIDC(t *testing.T) *fakeOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOIDC{key: key, kid: "key-1", browserSubject: "relaydesk-local-demo"}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}
func (f *fakeOIDC) rotate(t *testing.T) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.key = key
	f.kid = "key-2"
	f.mu.Unlock()
}
func (f *fakeOIDC) setBrowserSubject(subject string) {
	f.mu.Lock()
	f.browserSubject = subject
	f.mu.Unlock()
}
func (f *fakeOIDC) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		writeFake(w, map[string]any{"issuer": f.server.URL, "authorization_endpoint": f.server.URL + "/authorize",
			"token_endpoint": f.server.URL + "/token", "jwks_uri": f.server.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"}})
	case "/keys":
		f.mu.RLock()
		key, kid := f.key, f.kid
		f.mu.RUnlock()
		writeFake(w, map[string]any{"keys": []any{map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())}}})
	case "/authorize":
		q := r.URL.Query()
		redirect, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || redirect.Scheme != "http" || !strings.HasPrefix(redirect.Hostname(), "127.0.0.1") ||
			q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" {
			http.Error(w, "invalid local demo authorization", 400)
			return
		}
		f.mu.RLock()
		subject := f.browserSubject
		f.mu.RUnlock()
		code := strings.Join([]string{"valid", q.Get("code_challenge"), q.Get("nonce"), subject}, "~")
		values := redirect.Query()
		values.Set("state", q.Get("state"))
		values.Set("code", code)
		redirect.RawQuery = values.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	case "/token":
		clientID, secret, ok := r.BasicAuth()
		if !ok || clientID != "relaydesk-test" || secret != "test-client-secret" {
			http.Error(w, "invalid client", 401)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", 400)
			return
		}
		parts := strings.Split(r.Form.Get("code"), "~")
		if len(parts) != 4 || r.Form.Get("grant_type") != "authorization_code" {
			http.Error(w, "invalid code", 400)
			return
		}
		challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(challenge[:]) != parts[1] {
			http.Error(w, "PKCE mismatch", 400)
			return
		}
		signed := f.sign(parts[0], parts[2], parts[3])
		writeFake(w, map[string]any{"access_token": "opaque-test-access", "token_type": "Bearer", "expires_in": 3600, "id_token": signed})
	default:
		http.NotFound(w, r)
	}
}
func writeFake(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (f *fakeOIDC) sign(mode, nonce, subject string) string {
	f.mu.RLock()
	key, kid := f.key, f.kid
	f.mu.RUnlock()
	now := time.Now()
	issuer, audience := f.server.URL, "relaydesk-test"
	if mode == "issuer" {
		issuer = "https://wrong.invalid"
	}
	if mode == "audience" {
		audience = "another-client"
	}
	if mode == "nonce" {
		nonce = "incorrect-nonce"
	}
	exp := now.Add(time.Hour).Unix()
	if mode == "expired" {
		exp = now.Add(-time.Hour).Unix()
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	claimValues := map[string]any{"iss": issuer, "sub": subject, "aud": audience, "exp": exp,
		"iat": now.Unix(), "nonce": nonce, "email": subject + "@example.invalid"}
	if mode == "azp" {
		claimValues["azp"] = "another-client"
	}
	claims, _ := json.Marshal(claimValues)
	message := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(message))
	if mode == "signature" {
		wrong, _ := rsa.GenerateKey(rand.Reader, 2048)
		key = wrong
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return message + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type testApp struct {
	server  *httptest.Server
	service *identity.Service
	client  *http.Client
	logs    *lockedBuffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func newTestApp(t *testing.T, provider *fakeOIDC) *testApp {
	return newTestAppHubSpot(t, provider, "")
}

func newTestAppHubSpot(t *testing.T, provider *fakeOIDC, hubspotURL string) *testApp {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	origin := "http://" + srv.Listener.Addr().String()
	cfg := config.Config{Environment: "test", HTTPAddr: "127.0.0.1:0", PublicOrigin: origin,
		DemoMode:        os.Getenv("RELAYDESK_INTERACTIVE_DEMO") == "1",
		AppDatabaseURL:  testAppDatabaseURL,
		AuthDatabaseURL: testAuthDatabaseURL,
		OIDCIssuer:      provider.server.URL, OIDCClientID: "relaydesk-test", OIDCClientSecret: "test-client-secret",
		OIDCRedirectURL: origin + "/auth/callback", EncryptionKey: testKey,
		HubSpotClientID: "hubspot-test-client", HubSpotClientSecret: "hubspot-test-secret",
		SlackClientID: "slack-test-client", SlackClientSecret: "slack-test-secret",
		HubSpotAPIURL: hubspotURL,
		ReadTimeout:   5 * time.Second, WriteTimeout: 10 * time.Second}
	service, err := identity.New(context.Background(), authDB, appDB, box, cfg)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	srv.Config.Handler = api.New(cfg, db, box, slog.New(slog.NewTextHandler(logs, nil)), service).Handler
	srv.Start()
	t.Cleanup(srv.Close)
	return &testApp{server: srv, service: service, logs: logs, client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func request(t *testing.T, client *http.Client, method, target string, cookie *http.Cookie, csrf string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
func closeResponse(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != want {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, want, raw)
	}
}
func flow(t *testing.T, app *testApp, mode, subject string, old *http.Cookie) (*http.Cookie, *http.Cookie, string, string) {
	t.Helper()
	login := request(t, app.client, "GET", app.server.URL+"/auth/login", nil, "", nil)
	if login.StatusCode != 302 {
		closeResponse(t, login, 302)
	}
	location, err := url.Parse(login.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := location.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatal("missing PKCE/state/nonce")
	}
	var stateCookie *http.Cookie
	for _, c := range login.Cookies() {
		if c.Name == "relaydesk_oidc_state" {
			stateCookie = c
		}
	}
	login.Body.Close()
	if stateCookie == nil {
		t.Fatal("state cookie missing")
	}
	code := strings.Join([]string{mode, q.Get("code_challenge"), q.Get("nonce"), subject}, "~")
	callback := app.server.URL + "/auth/callback?state=" + url.QueryEscape(q.Get("state")) + "&code=" + url.QueryEscape(code)
	return stateCookie, old, callback, q.Get("state")
}
func completeFlow(t *testing.T, app *testApp, stateCookie, old *http.Cookie, callback string, want int) *http.Cookie {
	t.Helper()
	req, err := http.NewRequest("GET", callback, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(stateCookie)
	if old != nil {
		req.AddCookie(old)
	}
	resp, err := app.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("callback status=%d want=%d body=%s", resp.StatusCode, want, raw)
	}
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "relaydesk_dev_session" {
			session = c
		}
	}
	return session
}
func login(t *testing.T, app *testApp, subject string, old *http.Cookie) *http.Cookie {
	t.Helper()
	state, previous, callback, _ := flow(t, app, "valid", subject, old)
	session := completeFlow(t, app, state, previous, callback, 303)
	if session == nil {
		t.Fatal("session cookie missing")
	}
	return session
}
func me(t *testing.T, app *testApp, cookie *http.Cookie) (string, string) {
	t.Helper()
	resp := request(t, app.client, "GET", app.server.URL+"/v1/me", cookie, "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("me status=%d", resp.StatusCode)
	}
	var v struct {
		ID   string `json:"id"`
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v.ID, v.CSRF
}
func createWorkspace(t *testing.T, app *testApp, cookie *http.Cookie, csrf, name string) string {
	t.Helper()
	resp := request(t, app.client, "POST", app.server.URL+"/v1/tenants", cookie, csrf, map[string]string{"name": name})
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("workspace status=%d body=%s", resp.StatusCode, raw)
	}
	var v struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v.ID
}

func TestH_AuthenticationAndSession(t *testing.T) {
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	for _, mode := range []string{"issuer", "audience", "signature", "expired", "nonce", "azp"} {
		t.Run(mode, func(t *testing.T) {
			state, old, callback, _ := flow(t, app, mode, "negative-"+mode, nil)
			if got := completeFlow(t, app, state, old, callback, 400); got != nil {
				t.Fatal("invalid token created session")
			}
		})
	}
	expiredFlowCookie, _, expiredCallback, expiredState := flow(t, app, "valid", "expired-flow", nil)
	stateHash := sha256.Sum256([]byte(expiredState))
	if _, err := mig.Exec(context.Background(), `UPDATE oidc_flows SET expires_at=now()-interval '1 second' WHERE state_hash=$1`, stateHash[:]); err != nil {
		t.Fatal(err)
	}
	completeFlow(t, app, expiredFlowCookie, nil, expiredCallback, 400)
	state, old, callback, _ := flow(t, app, "valid", "state-check", nil)
	if got := completeFlow(t, app, state, old, strings.Replace(callback, "state=", "state=wrong", 1), 400); got != nil {
		t.Fatal("wrong state accepted")
	}
	session := completeFlow(t, app, state, old, callback, 303)
	if session == nil {
		t.Fatal("valid login failed")
	}
	if got := completeFlow(t, app, state, old, callback, 400); got != nil {
		t.Fatal("replayed callback accepted")
	}
	userID, csrf := me(t, app, session)
	if userID == "" || csrf == "" {
		t.Fatal("missing authenticated identity")
	}
	otherInstance := newTestApp(t, idp)
	if sameID, _ := me(t, otherInstance, session); sameID != userID {
		t.Fatal("session depended on one API instance's memory")
	}
	response := request(t, app.client, "GET", app.server.URL+"/v1/me", session, "", nil)
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || bytes.Contains(data, []byte(session.Value)) ||
		strings.Contains(app.logs.String(), session.Value) ||
		strings.Contains(app.logs.String(), "test-client-secret") {
		t.Fatal("session or OIDC secret exposed in API response or logs")
	}
	closeResponse(t, request(t, app.client, "POST", app.server.URL+"/v1/tenants", session, "", map[string]string{"name": "CSRF blocked"}), 403)
	oldSession := session
	session = login(t, app, "state-check", oldSession)
	if session.Value == oldSession.Value {
		t.Fatal("session fixation/rotation failure")
	}
	if sameID, _ := me(t, app, session); sameID != userID {
		t.Fatal("same issuer and subject created a second user")
	}
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/me", oldSession, "", nil), 401)
	_, csrf = me(t, app, session)
	closeResponse(t, request(t, app.client, "POST", app.server.URL+"/auth/logout", session, csrf, nil), 204)
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/me", session, "", nil), 401)
	expired := login(t, app, "expired-session", nil)
	h := sha256.Sum256([]byte(expired.Value))
	if _, err := mig.Exec(context.Background(), `UPDATE user_sessions SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, h[:]); err != nil {
		t.Fatal(err)
	}
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/me", expired, "", nil), 401)
	idle := login(t, app, "idle-session", nil)
	idleHash := sha256.Sum256([]byte(idle.Value))
	if _, err := mig.Exec(context.Background(), `UPDATE user_sessions SET last_seen_at=now()-interval '31 minutes' WHERE token_hash=$1`, idleHash[:]); err != nil {
		t.Fatal(err)
	}
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/me", idle, "", nil), 401)
	idp.rotate(t)
	rotated := login(t, app, "rotation-check", nil)
	me(t, app, rotated)
}

func TestI_TenantAuthorizationAndRBAC(t *testing.T) {
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	aCookie := login(t, app, "owner-a", nil)
	aID, aCSRF := me(t, app, aCookie)
	bCookie := login(t, app, "owner-b", nil)
	bID, bCSRF := me(t, app, bCookie)
	aTenant := createWorkspace(t, app, aCookie, aCSRF, "Workspace A")
	bTenant := createWorkspace(t, app, bCookie, bCSRF, "Workspace B")
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+bTenant, aCookie, "", nil), 403)
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+aTenant, bCookie, "", nil), 403)
	closeResponse(t, request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+bTenant+"/members", aCookie, aCSRF, map[string]string{"user_id": aID, "role": "owner"}), 403)
	var bConnection string
	if err := mig.QueryRow(context.Background(), `INSERT INTO connections(tenant_id,provider,provider_account_id,mode)
	    VALUES($1,'hubspot',$2,'test') RETURNING id`, bTenant, "portal-"+randomHex(5)).Scan(&bConnection); err != nil {
		t.Fatal(err)
	}
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+bTenant+"/connections", aCookie, "", nil), 403)
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+aTenant+"/connections/"+bConnection, aCookie, "", nil), 404)
	grant := map[string]string{"user_id": bID, "role": "viewer"}
	closeResponse(t, request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+aTenant+"/members", aCookie, aCSRF, grant), 204)
	firstPage := request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+aTenant+"/members?limit=1", aCookie, "", nil)
	var memberPage struct {
		Items []identity.Member `json:"items"`
		Next  string            `json:"next_cursor"`
	}
	if err := json.NewDecoder(firstPage.Body).Decode(&memberPage); err != nil {
		t.Fatal(err)
	}
	_ = firstPage.Body.Close()
	if firstPage.StatusCode != 200 || len(memberPage.Items) != 1 || memberPage.Next == "" {
		t.Fatalf("first member page: status=%d items=%d cursor=%q", firstPage.StatusCode, len(memberPage.Items), memberPage.Next)
	}
	secondPage := request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+aTenant+"/members?limit=1&cursor="+url.QueryEscape(memberPage.Next), aCookie, "", nil)
	var second struct {
		Items []identity.Member `json:"items"`
		Next  string            `json:"next_cursor"`
	}
	if err := json.NewDecoder(secondPage.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	_ = secondPage.Body.Close()
	if secondPage.StatusCode != 200 || len(second.Items) != 1 || second.Next != "" || second.Items[0].UserID == memberPage.Items[0].UserID {
		t.Fatalf("second member page: status=%d items=%d cursor=%q", secondPage.StatusCode, len(second.Items), second.Next)
	}
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+aTenant+"/connections", bCookie, "", nil), 200)
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+aTenant+"/runs", bCookie, "", nil), 200)
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+aTenant+"/members", bCookie, "", nil), 403)
	closeResponse(t, request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+aTenant+"/members", bCookie, bCSRF, map[string]string{"user_id": bID, "role": "owner"}), 403)
	var bRole string
	if err := mig.QueryRow(context.Background(), `SELECT role FROM memberships WHERE tenant_id=$1 AND user_id=$2`, aTenant, bID).Scan(&bRole); err != nil || bRole != "viewer" {
		t.Fatalf("unauthorized grant changed membership: role=%q err=%v", bRole, err)
	}
	ownerPrincipal, err := app.service.Current(context.Background(), aCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	viewerPrincipal, err := app.service.Current(context.Background(), bCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	checkPermission := func(p identity.Principal, permission, want string) {
		t.Helper()
		var got *string
		if err := appDB.Pool.QueryRow(context.Background(), `SELECT tenant_authorize($1,$2,$3)`, p.TokenHash, aTenant, permission).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if value := func() string {
			if got == nil {
				return ""
			}
			return *got
		}(); value != want {
			t.Fatalf("permission %s: got %q want %q", permission, value, want)
		}
	}
	for _, permission := range []string{"read", "operate", "members_read", "members_write"} {
		checkPermission(ownerPrincipal, permission, "owner")
	}
	checkPermission(viewerPrincipal, "read", "viewer")
	checkPermission(viewerPrincipal, "operate", "")
	checkPermission(viewerPrincipal, "members_write", "")
	closeResponse(t, request(t, app.client, "PATCH", app.server.URL+"/v1/tenants/"+aTenant+"/members/"+bID, aCookie, aCSRF, map[string]string{"role": "operator"}), 204)
	checkPermission(viewerPrincipal, "operate", "operator")
	checkPermission(viewerPrincipal, "members_write", "")
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+aTenant+"/connections", bCookie, "", nil), 200)
	closeResponse(t, request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+aTenant+"/members", bCookie, bCSRF, map[string]string{"user_id": bID, "role": "owner"}), 403)
	closeResponse(t, request(t, app.client, "DELETE", app.server.URL+"/v1/tenants/"+aTenant+"/members/"+aID, aCookie, aCSRF, nil), 409)
	closeResponse(t, request(t, app.client, "DELETE", app.server.URL+"/v1/tenants/"+aTenant+"/members/"+bID, aCookie, aCSRF, nil), 204)
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+aTenant+"/runs", bCookie, "", nil), 403)
	var ownerCount, auditCount int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM memberships WHERE tenant_id=$1 AND role='owner' AND status='active'`, aTenant).Scan(&ownerCount); err != nil {
		t.Fatal(err)
	}
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE tenant_id=$1`, aTenant).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if ownerCount != 1 || auditCount != 4 {
		t.Fatalf("owner=%d audit=%d", ownerCount, auditCount)
	}
}

func TestL_ConcurrentOIDCCallbackConsumesStateOnce(t *testing.T) {
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	state, _, callback, _ := flow(t, app, "valid", "single-use", nil)
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest("GET", callback, nil)
			if err != nil {
				results <- 0
				return
			}
			req.AddCookie(state)
			resp, err := app.client.Do(req)
			if err != nil {
				results <- 0
				return
			}
			results <- resp.StatusCode
			resp.Body.Close()
		}()
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[303] != 1 || counts[400] != 1 {
		t.Fatalf("concurrent callbacks: %v", counts)
	}
}

func TestJ_ConcurrencyAndRevocation(t *testing.T) {
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	aCookie := login(t, app, "race-owner", nil)
	aID, aCSRF := me(t, app, aCookie)
	bCookie := login(t, app, "race-member", nil)
	bID, bCSRF := me(t, app, bCookie)
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids <- createWorkspace(t, app, aCookie, aCSRF, fmt.Sprintf("Concurrent %d", i))
		}(i)
	}
	wg.Wait()
	close(ids)
	var tenantID string
	for id := range ids {
		if tenantID == "" {
			tenantID = id
		}
		var n int
		if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM memberships WHERE tenant_id=$1 AND user_id=$2 AND role='owner'`, id, aID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("atomic owner n=%d err=%v", n, err)
		}
	}
	closeResponse(t, request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+tenantID+"/members", aCookie, aCSRF, map[string]string{"user_id": bID, "role": "operator"}), 204)
	p, err := app.service.Current(context.Background(), bCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, err := app.service.AuthorizedTenantTx(context.Background(), p, tenantID, "read")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan int, 1)
	go func() {
		resp := request(t, app.client, "DELETE", app.server.URL+"/v1/tenants/"+tenantID+"/members/"+bID, aCookie, aCSRF, nil)
		result <- resp.StatusCode
		resp.Body.Close()
	}()
	select {
	case <-result:
		t.Fatal("revocation bypassed active membership lock")
	case <-time.After(150 * time.Millisecond):
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case status := <-result:
		if status != 204 {
			t.Fatalf("revoke status=%d", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revoke blocked")
	}
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+tenantID, bCookie, "", nil), 403)
	// Grant a second owner, then race two removals. The serialized tenant lock
	// must preserve at least one owner regardless of which request wins.
	closeResponse(t, request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+tenantID+"/members", aCookie, aCSRF, map[string]string{"user_id": bID, "role": "owner"}), 204)
	results := make(chan int, 2)
	for _, target := range []string{aID, bID} {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := request(t, app.client, "DELETE", app.server.URL+"/v1/tenants/"+tenantID+"/members/"+target, aCookie, aCSRF, nil)
			results <- resp.StatusCode
			resp.Body.Close()
		}()
	}
	wg.Wait()
	close(results)
	var active int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM memberships WHERE tenant_id=$1 AND status='active' AND role='owner'`, tenantID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active < 1 {
		t.Fatal("concurrent removals deleted final owner")
	}
	_ = bCSRF
}

func TestK_BrowserDatabasePrivileges(t *testing.T) {
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	a := login(t, app, "db-owner-a", nil)
	_, aCSRF := me(t, app, a)
	b := login(t, app, "db-owner-b", nil)
	_, bCSRF := me(t, app, b)
	aTenant := createWorkspace(t, app, a, aCSRF, "Database A")
	bTenant := createWorkspace(t, app, b, bCSRF, "Database B")
	ctx := context.Background()
	for _, sql := range []string{
		`SELECT count(*) FROM user_sessions`,
		`SELECT count(*) FROM connection_credentials`,
		`SELECT raw_payload FROM webhook_events LIMIT 1`,
		`SELECT count(*) FROM claim_outbox(1,1)`,
		`SELECT session_rotate(NULL,decode(repeat('01',32),'hex'),gen_random_uuid(),now()+interval '1 hour')`,
	} {
		var n any
		err := appDB.Pool.QueryRow(ctx, sql).Scan(&n)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("browser role privilege check %s: %v", sql, err)
		}
	}
	for _, check := range []struct {
		name string
		pool *store.Store
		sql  string
	}{
		{"worker cannot mint session", db, `SELECT session_rotate(NULL,decode(repeat('01',32),'hex'),gen_random_uuid(),now()+interval '1 hour')`},
		{"worker cannot create identity", db, `SELECT oidc_user('https://issuer.invalid','forged','')`},
		{"OIDC role cannot claim outbox", authDB, `SELECT count(*) FROM claim_outbox(1,1)`},
		{"OIDC role cannot read tenant data", authDB, `SELECT count(*) FROM tenants`},
	} {
		var n any
		err := check.pool.Pool.QueryRow(ctx, check.sql).Scan(&n)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s: %v", check.name, err)
		}
	}
	conn, err := appDB.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	bareTx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bareTx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, bTenant); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := bareTx.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("tenant GUC alone escaped RLS: count=%d err=%v", count, err)
	}
	if err := bareTx.QueryRow(ctx, `SELECT count(*) FROM webhook_events`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("tenant GUC alone exposed webhook metadata: count=%d err=%v", count, err)
	}
	if err := bareTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := app.service.Current(ctx, a.Value)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.session_hash',$2,true)`, bTenant, hex.EncodeToString(p.TokenHash)); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("A session read B via RLS: count=%d err=%v", count, err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, aTenant); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("A own tenant missing: count=%d err=%v", count, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("transaction context leaked through pool: count=%d err=%v", count, err)
	}
	spoof, err := http.NewRequest("GET", app.server.URL+"/v1/tenants/"+bTenant, nil)
	if err != nil {
		t.Fatal(err)
	}
	spoof.AddCookie(a)
	spoof.Header.Set("X-Tenant-ID", bTenant)
	spoof.Header.Set("X-Role", "owner")
	spoof.Header.Set("X-User-ID", "fake")
	resp, err := app.client.Do(spoof)
	if err != nil {
		t.Fatal(err)
	}
	closeResponse(t, resp, 403)
}
