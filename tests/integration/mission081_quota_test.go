//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
)

func TestMission081QuotaPreviewsDoNotReserveCapacity(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	quota := &execution.ProviderQuota{Client: client}
	budget := execution.Budget{Key: "relaydesk:quota:{rdq}:m081:" + uuid.NewString(), Limit: 1, Window: time.Second}
	if wait, err := quota.Peek(ctx, []execution.Budget{budget}); err != nil || wait <= 0 {
		t.Fatalf("cold quota preview: wait=%s err=%v", wait, err)
	}
	time.Sleep(1100 * time.Millisecond)
	for i := 0; i < 2; i++ {
		if wait, err := quota.Peek(ctx, []execution.Budget{budget}); err != nil || wait != 0 {
			t.Fatalf("non-consuming preview %d: wait=%s err=%v", i, wait, err)
		}
	}
	if wait, err := quota.Allow(ctx, []execution.Budget{budget}); err != nil || wait != 0 {
		t.Fatalf("preview consumed a real provider slot: wait=%s err=%v", wait, err)
	}
	if wait, err := quota.Peek(ctx, []execution.Budget{budget}); err != nil || wait <= 0 {
		t.Fatalf("exhausted provider quota preview: wait=%s err=%v", wait, err)
	}
	limiter := &execution.RedisWriteLimiter{Client: client, Limit: 1, Window: 3 * time.Second}
	tenant, connection := uuid.NewString(), uuid.NewString()
	if wait, err := limiter.Peek(ctx, tenant, connection); err != nil || wait != 0 {
		t.Fatalf("cold write preview: wait=%s err=%v", wait, err)
	}
	if wait, err := limiter.Allow(ctx, tenant, connection); err != nil || wait != 0 {
		t.Fatalf("preview consumed a synthetic write slot: wait=%s err=%v", wait, err)
	}
	if wait, err := limiter.Peek(ctx, tenant, connection); err != nil || wait <= 0 {
		t.Fatalf("exhausted write preview: wait=%s err=%v", wait, err)
	}
}

func TestMission081QuotaWaitSkipsReadsButAllowsLinkedNoop(t *testing.T) {
	v := newWorld()
	f, cfg := seedBusiness(t, v)
	limiter := &execution.RedisWriteLimiter{Client: testRedis(t), Limit: 1, Window: 8 * time.Second}
	stripeHTTP := httptest.NewServer(http.HandlerFunc(v.stripe))
	hubspotHTTP := httptest.NewServer(http.HandlerFunc(v.hubspot))
	slackHTTP := httptest.NewServer(http.HandlerFunc(v.slack))
	ctx, cancel := context.WithCancel(context.Background())
	x := &execution.Executor{DB: db, Box: box, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Endpoints: providers.Endpoints{Stripe: stripeHTTP.URL, HubSpot: hubspotHTTP.URL,
			Slack: slackHTTP.URL, Simulation: true}, RequireLimiter: true, Limiter: limiter}
	id := deliverBusiness(t, f, v, "customer.subscription.updated")
	await(t, func() bool { return actionCount(t, f.tenantID, "crm_projection") == 1 })
	if wait, err := limiter.Allow(context.Background(), f.tenantID, cfg.HubSpotConnectionID); err != nil || wait != 0 {
		t.Fatalf("preconsume synthetic Contact write capacity: wait=%s err=%v", wait, err)
	}
	done := make(chan struct{})
	go func() { defer close(done); x.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; stripeHTTP.Close(); hubspotHTTP.Close(); slackHTTP.Close() })
	waitBusinessStatus(t, f.tenantID, id, "retry_wait")
	var calls, failures int
	if err := mig.QueryRow(context.Background(), `SELECT
	 (SELECT count(*) FROM business_provider_calls WHERE tenant_id=$1),
	 (SELECT retry_failures FROM business_actions WHERE tenant_id=$1 AND kind='crm_projection')`,
		f.tenantID).Scan(&calls, &failures); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || failures != 0 {
		t.Fatalf("known quota wait caused provider reads or retry escalation: calls=%d failures=%d", calls, failures)
	}
	waitBusinessStatus(t, f.tenantID, id, "succeeded")
	if writes, _ := externalCounts(v); writes != 1 {
		t.Fatalf("Contact write after quota release=%d", writes)
	}
	// A later same-state event has a local Contact link. Its current-state
	// read/compare can finish even while the write budget is exhausted.
	later := deliverBusiness(t, f, v, "customer.subscription.updated")
	waitBusinessStatus(t, f.tenantID, later, "succeeded")
	if writes, _ := externalCounts(v); writes != 1 {
		t.Fatalf("linked no-op repeated Contact write: %d", writes)
	}
}
