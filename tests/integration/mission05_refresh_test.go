//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lenoir-dev/LenoirDesk/internal/providerconn"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

func TestMission05ConcurrentRefreshAndUnknownOutcome(t *testing.T) {
	ctx := context.Background()
	app := newTestApp(t, newFakeOIDC(t))
	hubID := int(time.Now().UnixNano()%900_000_000 + 100_000_000)
	account := strconv.Itoa(hubID)
	app.service.ProviderHTTP = providerSimulator(t, hubID, "oauth crm.objects.contacts.read crm.objects.contacts.write")
	session := login(t, app, uuid.NewString(), nil)
	_, csrf := me(t, app, session)
	tenant := createWorkspace(t, app, session, csrf, "Refresh test")
	flow, state := beginHubSpot(t, app, session, csrf, tenant, account, "")
	connection := providerCallback(t, app, session, flow, state, 200)
	if _, err := mig.Exec(ctx, `UPDATE provider_token_state SET expires_at=now()+interval '20 seconds'
 WHERE tenant_id=$1 AND connection_id=$2`, tenant, connection); err != nil {
		t.Fatal(err)
	}
	firstDB, err := store.Open(ctx, testRuntimeDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer firstDB.Close()
	secondDB, err := store.Open(ctx, testRuntimeDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer secondDB.Close()
	var calls atomic.Int64
	refreshHTTP := &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/oauth/2026-03/token":
			if r.ParseForm() != nil || r.Form.Get("grant_type") != "refresh_token" {
				t.Errorf("refresh grant missing")
			}
			calls.Add(1)
			time.Sleep(100 * time.Millisecond)
			body, _ := json.Marshal(map[string]any{"access_token": "refreshed-access-secret", "refresh_token": "rotated-refresh-secret",
				"hub_id": hubID, "scopes": []string{"oauth", "crm.objects.contacts.read", "crm.objects.contacts.write"},
				"expires_in": 1800, "token_type": "bearer"})
			return fakeProviderBody(200, string(body)), nil
		case "/account-info/v3/details":
			body, _ := json.Marshal(map[string]any{"portalId": hubID, "accountType": "DEVELOPER_TEST"})
			return fakeProviderBody(200, string(body)), nil
		}
		t.Errorf("unexpected refresh path %s", r.URL.Path)
		return fakeProviderBody(404, `{}`), nil
	})}
	client := providerconn.Client{Config: providerconn.Config{HubSpotID: "hubspot-test-client", HubSpotSecret: "hubspot-test-secret"}, HTTP: refreshHTTP}
	r1 := &providerconn.Refresher{DB: firstDB, Box: box, Client: client}
	r2 := &providerconn.Refresher{DB: secondDB, Box: box, Client: client}
	var wg sync.WaitGroup
	for _, refresher := range []*providerconn.Refresher{r1, r2} {
		wg.Add(1)
		go func(r *providerconn.Refresher) {
			defer wg.Done()
			err := r.Ensure(ctx, tenant, connection)
			if err != nil && !errors.Is(err, providerconn.ErrRefreshBusy) {
				t.Errorf("refresh: %v", err)
			}
		}(refresher)
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent refresh calls=%d want=1", calls.Load())
	}
	var version int64
	var stateValue string
	if err := mig.QueryRow(ctx, `SELECT credential_version,auth_state FROM provider_token_state WHERE connection_id=$1`, connection).
		Scan(&version, &stateValue); err != nil || version != 2 || stateValue != "ready" {
		t.Fatalf("refresh state version=%d state=%s err=%v", version, stateValue, err)
	}
	var oldBytes []byte
	if err := mig.QueryRow(ctx, `SELECT ciphertext FROM connection_credentials WHERE connection_id=$1 AND credential_version=1`, connection).Scan(&oldBytes); err != nil || len(oldBytes) != 0 {
		t.Fatalf("old credential not erased: len=%d err=%v", len(oldBytes), err)
	}

	if _, err := mig.Exec(ctx, `UPDATE provider_token_state SET expires_at=now()+interval '20 seconds' WHERE connection_id=$1`, connection); err != nil {
		t.Fatal(err)
	}
	uncertainHTTP := &http.Client{Transport: providerTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, io.ErrUnexpectedEOF
	})}
	r1.Client.HTTP = uncertainHTTP
	if err := r1.Ensure(ctx, tenant, connection); !errors.Is(err, providerconn.ErrUncertainRefresh) {
		t.Fatalf("unknown refresh result=%v", err)
	}
	if err := r2.Ensure(ctx, tenant, connection); !errors.Is(err, providerconn.ErrBlockedAuth) {
		t.Fatalf("blocked refresh result=%v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("uncertain refresh retried remotely: calls=%d", calls.Load())
	}
	if err := mig.QueryRow(ctx, `SELECT auth_state FROM provider_token_state WHERE connection_id=$1`, connection).Scan(&stateValue); err != nil || stateValue != "blocked_auth" {
		t.Fatalf("uncertain refresh not blocked: %s %v", stateValue, err)
	}
}
