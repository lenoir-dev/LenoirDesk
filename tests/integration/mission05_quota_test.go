//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	opt, err := redis.ParseURL("redis://127.0.0.1:56379")
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(opt)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestMission05RedisMultiBudgetAtomicityAndColdStart(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	quota := &execution.ProviderQuota{Client: client}
	keyA, keyB := "relaydesk:quota:{rdq}:test:"+uuid.NewString(), "relaydesk:quota:{rdq}:test:"+uuid.NewString()
	a, b := execution.Budget{Key: keyA, Limit: 1, Window: time.Second}, execution.Budget{Key: keyB, Limit: 1, Window: time.Second}
	wait, err := quota.Allow(ctx, []execution.Budget{a, b})
	if err != nil || wait < time.Second {
		t.Fatalf("cold start did not wait: wait=%s err=%v", wait, err)
	}
	if again, err := quota.Allow(ctx, []execution.Budget{a, b}); err != nil || again <= 0 {
		t.Fatalf("concurrent cold-start request bypassed wait: wait=%s err=%v", again, err)
	}
	time.Sleep(1100 * time.Millisecond)
	if wait, err = quota.Allow(ctx, []execution.Budget{a}); err != nil || wait != 0 {
		t.Fatalf("first budget not admitted: %s %v", wait, err)
	}
	if wait, err = quota.Allow(ctx, []execution.Budget{a, b}); err != nil || wait <= 0 {
		t.Fatalf("exhausted combined budget admitted: %s %v", wait, err)
	}
	if wait, err = quota.Allow(ctx, []execution.Budget{b}); err != nil || wait != 0 {
		t.Fatalf("denied multi-budget call consumed the other budget: %s %v", wait, err)
	}
	_ = client.Del(ctx, keyA, keyA+":warm", keyB, keyB+":warm").Err()
	if wait, err = quota.Allow(ctx, []execution.Budget{a, b}); err != nil || wait < time.Second {
		t.Fatalf("state loss did not cause cold wait: %s %v", wait, err)
	}
}

func TestMission05RedisConcurrentQuotaAndOutage(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	quota := &execution.ProviderQuota{Client: client}
	budget := execution.Budget{Key: "relaydesk:quota:{rdq}:test:" + uuid.NewString(), Limit: 7, Window: time.Second}
	_, _ = quota.Allow(ctx, []execution.Budget{budget})
	time.Sleep(1100 * time.Millisecond)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait, err := quota.Allow(ctx, []execution.Budget{budget})
			if err == nil && wait == 0 {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 7 {
		t.Fatalf("atomic quota admitted %d calls, want 7", got)
	}
	_ = client.Close()
	if _, err := quota.Allow(ctx, []execution.Budget{budget}); err == nil {
		t.Fatal("closed Redis client permitted an outbound request")
	}
	recovered := testRedis(t)
	if wait, err := (&execution.ProviderQuota{Client: recovered}).Allow(ctx, []execution.Budget{budget}); err != nil || wait <= 0 {
		t.Fatalf("recovered Redis did not preserve exhausted budget: wait=%s err=%v", wait, err)
	}
}

func TestMission05ProviderCooldownLowerBound(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	quota := &execution.ProviderQuota{Client: client}
	budget := execution.Budget{Key: "relaydesk:quota:{rdq}:test:" + uuid.NewString(), Limit: 10, Window: time.Second}
	if _, err := quota.Allow(ctx, []execution.Budget{budget}); err != nil {
		t.Fatal(err)
	}
	if err := quota.Cooldown(ctx, budget.Key, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := quota.Cooldown(ctx, budget.Key, time.Second); err != nil {
		t.Fatal(err)
	}
	if wait, err := quota.Allow(ctx, []execution.Budget{budget}); err != nil || wait < time.Second {
		t.Fatalf("provider Retry-After cooldown shortened or bypassed: wait=%s err=%v", wait, err)
	}
}

func TestMission05ManualRetryCannotBypassQuota(t *testing.T) {
	v := newWorld()
	v.slackError = "channel_not_found"
	f, _ := seedBusiness(t, v)
	stripeHTTP := httptest.NewServer(http.HandlerFunc(v.stripe))
	hubspotHTTP := httptest.NewServer(http.HandlerFunc(v.hubspot))
	slackHTTP := httptest.NewServer(http.HandlerFunc(v.slack))
	ctx, cancel := context.WithCancel(context.Background())
	x := &execution.Executor{DB: db, Box: box, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Endpoints:      providers.Endpoints{Stripe: stripeHTTP.URL, HubSpot: hubspotHTTP.URL, Slack: slackHTTP.URL, Simulation: true},
		RequireLimiter: true, Limiter: &execution.RedisWriteLimiter{Client: testRedis(t), Limit: 1, Window: 4 * time.Second}}
	done := make(chan struct{})
	go func() { defer close(done); x.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; stripeHTTP.Close(); hubspotHTTP.Close(); slackHTTP.Close() })
	id := deliverBusiness(t, f, v, "invoice.paid")
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "needs_attention" })
	var actionID string
	var generation int64
	if err := mig.QueryRow(ctx, `SELECT id,generation FROM business_actions
 WHERE tenant_id=$1 AND kind='slack_invoice_paid'`, f.tenantID).Scan(&actionID, &generation); err != nil {
		t.Fatal(err)
	}
	idp := newFakeOIDC(t)
	app := newTestApp(t, idp)
	owner := login(t, app, "quota-owner-"+randomHex(4), nil)
	ownerID, csrf := me(t, app, owner)
	if _, err := mig.Exec(ctx, `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	v.slackError = ""
	v.mu.Unlock()
	path := app.server.URL + "/v1/tenants/" + f.tenantID + "/actions/" + actionID + "/retry"
	closeResponse(t, request(t, app.client, "POST", path, owner, csrf,
		map[string]any{"reason": "Retry after destination repair", "expected_generation": generation,
			"idempotency_key": "quota-retry-" + randomHex(4)}), 200)
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "retry_wait" })
	var calls int
	if err := mig.QueryRow(ctx, `SELECT count(*) FROM business_provider_calls pc
 JOIN business_attempts ba ON ba.tenant_id=pc.tenant_id AND ba.id=pc.attempt_id
 WHERE ba.tenant_id=$1 AND ba.action_id=$2 AND pc.request_path='/api/chat.postMessage'`,
		f.tenantID, actionID).Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("manual retry bypassed quota: Slack calls=%d err=%v", calls, err)
	}
	await(t, func() bool { return eventRunStatus(t, f.tenantID, id) == "succeeded" })
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.slackMessages != 1 || v.hubspotWrites != 1 {
		t.Fatalf("unexpected effects after quota release: Slack=%d CRM=%d", v.slackMessages, v.hubspotWrites)
	}
}
