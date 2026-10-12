//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/delivery"
	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
	"github.com/redis/go-redis/v9"
	"github.com/stripe/stripe-go/v86/webhook"
)

type m081Input struct {
	fixture                                   int
	id, kind, customer, invoice, subscription string
}

type m081Customer struct{ subscription, latestInvoice string }
type m081Invoice struct{ customer, subscription string }

type m081TenantProgress struct {
	Accepted, Within30, Within60, Within120, Unconverged int `json:",omitempty"`
}

type m081QueueSample struct {
	AtSeconds                              float64 `json:"at_seconds"`
	Outbox, Jobs, Actions, Running, DBPool int64   `json:",omitempty"`
}
type m081Stripe struct {
	customers map[string]m081Customer
	invoices  map[string]m081Invoice
	price     string
}

func (s *m081Stripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "unauthorized", 401)
		return
	}
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/v1/customers/"):
		id := strings.TrimPrefix(path, "/v1/customers/")
		if _, ok := s.customers[id]; !ok {
			http.NotFound(w, r)
			return
		}
		jsonResponse(w, map[string]any{"id": id, "object": "customer", "livemode": false})
	case path == "/v1/subscriptions":
		id := r.URL.Query().Get("customer")
		customer, ok := s.customers[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		jsonResponse(w, map[string]any{"object": "list", "has_more": false, "data": []any{map[string]any{
			"id": customer.subscription, "object": "subscription", "customer": id, "status": "active",
			"livemode": false, "cancel_at_period_end": false, "latest_invoice": customer.latestInvoice,
			"items": map[string]any{"object": "list", "data": []any{map[string]any{
				"id": "si_test", "price": map[string]any{"id": s.price}}}},
		}}})
	case strings.HasPrefix(path, "/v1/invoices/"):
		id := strings.TrimPrefix(path, "/v1/invoices/")
		invoice, ok := s.invoices[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		jsonResponse(w, map[string]any{"id": id, "object": "invoice", "customer": invoice.customer,
			"livemode": false, "status": "paid", "attempted": true, "currency": "usd", "amount_paid": 1500,
			"parent": map[string]any{"type": "subscription_details", "subscription_details": map[string]any{"subscription": invoice.subscription}}})
	default:
		http.NotFound(w, r)
	}
}

func m081Inputs(profile string, seed int64) ([]m081Input, *m081Stripe) {
	rng := rand.New(rand.NewSource(seed))
	items := make([]m081Input, 6000)
	stripe := &m081Stripe{customers: map[string]m081Customer{}, invoices: map[string]m081Invoice{}}
	serial := make([]int, 20)
	var previous m081Input
	for i := range items {
		if i > 0 && i%100 == 0 {
			items[i] = previous
			continue
		}
		idx := i % 20
		if profile != "A" {
			idx = 0
			if i%2 != 0 {
				idx = 1 + rng.Intn(19)
			}
		}
		serial[idx]++
		entry := m081Input{fixture: idx, id: fmt.Sprintf("evt_m081_%s_%06d", profile, i), kind: "invoice.paid"}
		if profile == "C" {
			entry.customer, entry.invoice, entry.subscription = "cus_m081_shared", "in_m081_shared", "sub_m081_shared"
		} else {
			customerSerial := serial[idx]
			if customerSerial%5 == 1 && customerSerial > 1 {
				customerSerial--
			}
			entry.customer = fmt.Sprintf("cus_m081_%02d_%06d", idx, customerSerial)
			entry.invoice = fmt.Sprintf("in_m081_%02d_%06d", idx, i)
			entry.subscription = fmt.Sprintf("sub_m081_%02d_%06d", idx, customerSerial)
			stripe.customers[entry.customer] = m081Customer{entry.subscription, entry.invoice}
			stripe.invoices[entry.invoice] = m081Invoice{entry.customer, entry.subscription}
			switch i % 20 {
			case 10, 11, 12, 13, 14:
				entry.kind = "invoice.payment_failed"
			case 15, 16, 17, 18, 19:
				entry.kind = "customer.subscription.updated"
			}
		}
		items[i], previous = entry, entry
	}
	return items, stripe
}

func m081Percentile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	return values[int(float64(len(values)-1)*q)]
}

// This benchmark exercises the production JetStream planner and action path,
// unlike Mission 08's historical poller profile. Each run owns fresh services.
func TestMission081LoadProfile(t *testing.T) {
	if os.Getenv("RELAYDESK_M081_LOAD") != "1" {
		t.Skip("use scripts/mission081_load.py")
	}
	profile := os.Getenv("RELAYDESK_M081_PROFILE")
	if profile != "A" && profile != "B" && profile != "C" {
		t.Fatal("profile must be A, B or C")
	}
	options, err := redis.ParseURL(os.Getenv("RELAYDESK_M08_REDIS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	redisClient := redis.NewClient(options)
	defer redisClient.Close()
	if err = redisClient.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	v := newWorld()
	fixtures := make([]fixture, 20)
	for i := range fixtures {
		fixtures[i], _ = seedBusiness(t, v)
	}
	items, stripeWorld := m081Inputs(profile, 808)
	stripeWorld.price = v.price
	if profile == "C" {
		v.customer, v.invoice, v.subscription = "cus_m081_shared", "in_m081_shared", "sub_m081_shared"
	}
	unique := map[string]m081Input{}
	customers := map[string]bool{}
	invoices := map[string]bool{}
	paidEffects := map[string]bool{}
	for _, item := range items {
		unique[item.id] = item
		customers[strconv.Itoa(item.fixture)+"|"+item.customer] = true
		invoices[strconv.Itoa(item.fixture)+"|"+item.invoice] = true
		if item.kind == "invoice.paid" {
			paidEffects[strconv.Itoa(item.fixture)+"|"+item.invoice] = true
		}
	}
	var outageKey, rateKey string
	if profile == "B" {
		// Scope faults to the complete provider connection. Every Contact key
		// carries its Stripe account identity, so all Customers of this tenant
		// hit the same outage or 429 policy.
		outageKey, rateKey = fixtures[1].account, fixtures[2].account
	} else if profile == "C" {
		for _, item := range items {
			if item.fixture == 1 && outageKey == "" {
				outageKey, _ = business.CustomerKey(fixtures[1].account, "test", item.customer)
			}
			if item.fixture == 2 && rateKey == "" {
				rateKey, _ = business.CustomerKey(fixtures[2].account, "test", item.customer)
			}
		}
	}
	start := time.Now()
	var resourceBefore, resourceAfter syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &resourceBefore)
	var providerMu sync.Mutex
	rateResponses, outageResponses := 0, 0
	contacts := &loadHubspotSimulator{nextID: 200000, byKey: map[string]loadContact{}, byID: map[string]string{}, firstWrite: map[string]time.Time{}, world: v}
	stripeHandler := http.HandlerFunc(stripeWorld.ServeHTTP)
	if profile == "C" {
		stripeHandler = v.stripe
	}
	stripeHTTP := httptest.NewServer(stripeHandler)
	hubspotHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if profile != "A" {
			body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if readErr != nil {
				http.Error(w, "bad request", 400)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			matches := func(key string) bool { return strings.Contains(r.URL.Path, key) || bytes.Contains(body, []byte(key)) }
			providerMu.Lock()
			if matches(outageKey) && time.Since(start) < 60*time.Second {
				outageResponses++
				providerMu.Unlock()
				http.Error(w, "synthetic outage", 503)
				return
			}
			if matches(rateKey) && rateResponses < 3 {
				rateResponses++
				providerMu.Unlock()
				w.Header().Set("Retry-After", "1")
				http.Error(w, "synthetic quota", 429)
				return
			}
			providerMu.Unlock()
		}
		contacts.ServeHTTP(w, r)
	}))
	slackHTTP := httptest.NewServer(http.HandlerFunc(v.slack))
	defer stripeHTTP.Close()
	defer hubspotHTTP.Close()
	defer slackHTTP.Close()
	ctx, cancel := context.WithCancel(context.Background())
	x := &execution.Executor{DB: db, Box: box, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Endpoints:      providers.Endpoints{Stripe: stripeHTTP.URL, HubSpot: hubspotHTTP.URL, Slack: slackHTTP.URL, Simulation: true},
		RequireLimiter: true, Limiter: &execution.RedisWriteLimiter{Client: redisClient, Limit: 5, Window: time.Second}}
	w, nc, err := delivery.New(ctx, db, natsURL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	w.Business = x
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx); nc.Close() }()
	defer func() { cancel(); <-done }()
	var peakPool, peakOutbox, peakJobs, peakActions, peakRunning atomic.Int64
	var timelineMu sync.Mutex
	var timeline []m081QueueSample
	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		ticks := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			var o, j, a, r int64
			if mig.QueryRow(ctx, `SELECT (SELECT count(*) FROM outbox WHERE published_at IS NULL),
			 (SELECT count(*) FROM planning_jobs WHERE status='pending'),
			 (SELECT count(*) FROM business_actions WHERE status IN ('pending','queued','running','retry_wait')),
			 (SELECT count(*) FROM business_actions WHERE status='running')`).Scan(&o, &j, &a, &r) == nil {
				for _, point := range []struct {
					value int64
					peak  *atomic.Int64
				}{
					{o, &peakOutbox}, {j, &peakJobs}, {a, &peakActions}, {r, &peakRunning},
				} {
					value, peak := point.value, point.peak
					for value > peak.Load() && !peak.CompareAndSwap(peak.Load(), value) {
					}
				}
				ticks++
				if ticks%10 == 0 {
					timelineMu.Lock()
					timeline = append(timeline, m081QueueSample{AtSeconds: time.Since(start).Seconds(),
						Outbox: o, Jobs: j, Actions: a, Running: r, DBPool: int64(db.Pool.Stat().AcquiredConns())})
					timelineMu.Unlock()
				}
			}
			p := int64(db.Pool.Stat().AcquiredConns())
			for p > peakPool.Load() && !peakPool.CompareAndSwap(peakPool.Load(), p) {
			}
		}
	}()
	defer func() { cancel(); <-sampleDone }()
	var mu sync.Mutex
	latencies := make([]float64, 0, len(items))
	codes := map[int]int{}
	client := &http.Client{Timeout: 10 * time.Second}
	var wg sync.WaitGroup
	slots := make(chan struct{}, 64)
	for i, item := range items {
		time.Sleep(time.Until(start.Add(time.Duration(i) * 10 * time.Millisecond)))
		f := fixtures[item.fixture]
		body := billingPayload(item.id, item.kind, item.customer, item.invoice, item.subscription)
		signature := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: f.secret, Timestamp: time.Now()})
		slots <- struct{}{}
		wg.Add(1)
		go func(f fixture, body []byte, header string) {
			defer wg.Done()
			defer func() { <-slots }()
			req, requestErr := http.NewRequest(http.MethodPost, server.URL+"/webhooks/stripe/"+f.ref, bytes.NewReader(body))
			if requestErr != nil {
				return
			}
			req.Header.Set("Stripe-Signature", header)
			began := time.Now()
			resp, callErr := client.Do(req)
			elapsed := time.Since(began)
			code := 0
			if callErr == nil {
				code = resp.StatusCode
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			mu.Lock()
			latencies = append(latencies, float64(elapsed.Microseconds())/1000)
			codes[code]++
			mu.Unlock()
		}(f, body, signature.Header)
	}
	wg.Wait()
	ingressSeconds := time.Since(start).Seconds()
	var outboxAtIngress, jobsAtIngress int64
	if err = mig.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM outbox WHERE published_at IS NULL),
	 (SELECT count(*) FROM planning_jobs WHERE status='pending')`).Scan(&outboxAtIngress, &jobsAtIngress); err != nil {
		t.Fatal(err)
	}
	// Every accepted event gets its full 60-second SLO window. The second
	// minute records recovery/backlog rather than silently censoring it.
	observeSeconds := 120
	if raw := os.Getenv("RELAYDESK_M081_OBSERVE_SECONDS"); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 120 || parsed > 900 || (profile != "B" && profile != "C") {
			t.Fatal("extended observation requires profile B or C and 120..900 seconds")
		}
		observeSeconds = parsed
	}
	deadline := time.Now().Add(time.Duration(observeSeconds) * time.Second)
	for time.Now().Before(deadline) {
		var unfinished int64
		if err = mig.QueryRow(context.Background(), `SELECT
		 (SELECT count(*) FROM planning_jobs WHERE status='pending')+
		 (SELECT count(*) FROM business_actions WHERE status IN ('pending','queued','running','retry_wait'))`).Scan(&unfinished); err != nil {
			t.Fatal(err)
		}
		if unfinished == 0 && time.Since(start) >= 120*time.Second {
			break
		}
		time.Sleep(2 * time.Second)
	}
	end := time.Now()
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &resourceAfter)
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	ctxQuery := context.Background()
	var inbox, actions, succeeded, unfinished, runCount, runSucceeded, deadlocks int64
	if err = mig.QueryRow(ctxQuery, `SELECT (SELECT count(*) FROM webhook_events),
	 (SELECT count(*) FROM business_actions),(SELECT count(*) FROM business_actions WHERE status='succeeded'),
	 (SELECT count(*) FROM planning_jobs WHERE status='pending')+
	 (SELECT count(*) FROM business_actions WHERE status IN ('pending','queued','running','retry_wait')),
	 (SELECT count(*) FROM runs),(SELECT count(*) FROM runs WHERE status='succeeded'),
	 (SELECT deadlocks FROM pg_stat_database WHERE datname=current_database())`).Scan(
		&inbox, &actions, &succeeded, &unfinished, &runCount, &runSucceeded, &deadlocks); err != nil {
		t.Fatal(err)
	}
	rows, err := mig.Query(ctxQuery, `SELECT e.event_id,e.provider_account_id,e.mode,e.received_at,
	 a.status,a.updated_at FROM webhook_events e
	 LEFT JOIN planning_jobs j ON j.tenant_id=e.tenant_id AND j.event_row_id=e.id
	 LEFT JOIN runs r ON r.tenant_id=j.tenant_id AND r.planning_job_id=j.id
	 LEFT JOIN business_run_actions ra ON ra.tenant_id=r.tenant_id AND ra.run_id=r.id AND ra.kind='crm_projection'
	 LEFT JOIN business_actions a ON a.tenant_id=ra.tenant_id AND a.id=ra.action_id`)
	if err != nil {
		t.Fatal(err)
	}
	converged := []float64{}
	within30, within60, within120, unconverged := 0, 0, 0, 0
	perTenant := make(map[string]*m081TenantProgress, len(fixtures))
	for i := range fixtures {
		perTenant[strconv.Itoa(i)] = &m081TenantProgress{}
	}
	var outageLatestCompletion time.Time
	completedCRMActions, missingCRMActions := 0, 0
	contacts.mu.Lock()
	firstWrite := make(map[string]time.Time, len(contacts.firstWrite))
	for key, value := range contacts.firstWrite {
		firstWrite[key] = value
	}
	contactCount := len(contacts.byKey)
	contacts.mu.Unlock()
	for rows.Next() {
		var id, account, mode string
		var accepted time.Time
		var crmStatus *string
		var crmCompleted *time.Time
		if err = rows.Scan(&id, &account, &mode, &accepted, &crmStatus, &crmCompleted); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		item, ok := unique[id]
		if !ok {
			rows.Close()
			t.Fatal("unexpected event identity")
		}
		key, keyErr := business.CustomerKey(account, mode, item.customer)
		if keyErr != nil {
			rows.Close()
			t.Fatal(keyErr)
		}
		progress := perTenant[strconv.Itoa(item.fixture)]
		progress.Accepted++
		if crmStatus == nil {
			missingCRMActions++
		}
		if crmStatus == nil || *crmStatus != "succeeded" || crmCompleted == nil || crmCompleted.After(end) {
			unconverged++
			progress.Unconverged++
			continue
		}
		if firstWrite[key].IsZero() {
			rows.Close()
			t.Fatal("CRM action succeeded without a Contact projection")
		}
		completedCRMActions++
		if profile == "B" && item.fixture == 1 && crmCompleted.After(outageLatestCompletion) {
			outageLatestCompletion = *crmCompleted
		}
		ms := crmCompleted.Sub(accepted).Seconds()
		if ms < 0 {
			ms = 0
		}
		converged = append(converged, ms)
		if ms <= 30 {
			within30++
			progress.Within30++
		}
		if ms <= 60 {
			within60++
			progress.Within60++
		}
		if ms <= 120 {
			within120++
			progress.Within120++
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	providerCounts := map[string]int64{}
	callRows, err := mig.Query(ctxQuery, `SELECT provider,method,
	 CASE WHEN request_path LIKE '/v1/customers/%' THEN 'customer_read'
	 WHEN request_path='/v1/subscriptions' THEN 'subscription_list'
	 WHEN request_path LIKE '/v1/invoices/%' THEN 'invoice_read'
	 WHEN request_path LIKE '/crm/v3/objects/contacts/%' THEN 'contact_read_or_patch'
	 WHEN request_path='/api/chat.postMessage' THEN 'message_send'
	 ELSE 'other' END,count(*) FROM business_provider_calls GROUP BY 1,2,3`)
	if err != nil {
		t.Fatal(err)
	}
	for callRows.Next() {
		var provider, method, kind string
		var count int64
		if err = callRows.Scan(&provider, &method, &kind, &count); err != nil {
			callRows.Close()
			t.Fatal(err)
		}
		providerCounts[provider+"/"+method+"/"+kind] = count
	}
	if err = callRows.Err(); err != nil {
		callRows.Close()
		t.Fatal(err)
	}
	callRows.Close()
	var writes, messages int
	v.mu.Lock()
	writes, messages = v.hubspotWrites, v.slackMessages
	v.mu.Unlock()
	providerMu.Lock()
	got429, got503 := rateResponses, outageResponses
	providerMu.Unlock()
	var outboxP50, outboxP95, planningP50, planningP95, providerP95, firstAttemptP95, runP95 float64
	if err = mig.QueryRow(ctxQuery, `SELECT
	 coalesce(percentile_cont(.5) WITHIN GROUP (ORDER BY extract(epoch FROM published_at-created_at)) FILTER (WHERE published_at IS NOT NULL),0),
	 coalesce(percentile_cont(.95) WITHIN GROUP (ORDER BY extract(epoch FROM published_at-created_at)) FILTER (WHERE published_at IS NOT NULL),0)
	 FROM outbox`).Scan(&outboxP50, &outboxP95); err != nil {
		t.Fatal(err)
	}
	if err = mig.QueryRow(ctxQuery, `SELECT
	 coalesce(percentile_cont(.5) WITHIN GROUP (ORDER BY extract(epoch FROM planned_at-created_at)) FILTER (WHERE planned_at IS NOT NULL),0),
	 coalesce(percentile_cont(.95) WITHIN GROUP (ORDER BY extract(epoch FROM planned_at-created_at)) FILTER (WHERE planned_at IS NOT NULL),0)
	 FROM planning_jobs`).Scan(&planningP50, &planningP95); err != nil {
		t.Fatal(err)
	}
	if err = mig.QueryRow(ctxQuery, `SELECT coalesce(percentile_cont(.95) WITHIN GROUP
	 (ORDER BY extract(epoch FROM finished_at-request_started_at)) FILTER (WHERE finished_at IS NOT NULL),0)
	 FROM business_provider_calls`).Scan(&providerP95); err != nil {
		t.Fatal(err)
	}
	if err = mig.QueryRow(ctxQuery, `SELECT coalesce(percentile_cont(.95) WITHIN GROUP
	 (ORDER BY extract(epoch FROM ba.started_at-a.created_at)),0)
	 FROM business_attempts ba JOIN business_actions a ON a.tenant_id=ba.tenant_id AND a.id=ba.action_id
	 WHERE ba.attempt_number=1`).Scan(&firstAttemptP95); err != nil {
		t.Fatal(err)
	}
	if err = mig.QueryRow(ctxQuery, `SELECT coalesce(percentile_cont(.95) WITHIN GROUP
	 (ORDER BY extract(epoch FROM r.finished_at-e.received_at)),0)
	 FROM runs r JOIN planning_jobs j ON j.tenant_id=r.tenant_id AND j.id=r.planning_job_id
	 JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
	 WHERE r.status='succeeded' AND r.finished_at IS NOT NULL`).Scan(&runP95); err != nil {
		t.Fatal(err)
	}
	timelineMu.Lock()
	queueTimeline := append([]m081QueueSample(nil), timeline...)
	timelineMu.Unlock()
	var outageRecoverySeconds any
	if profile == "B" && !outageLatestCompletion.IsZero() {
		outageRecoverySeconds = outageLatestCompletion.Sub(start.Add(60 * time.Second)).Seconds()
	}
	result := map[string]any{
		"profile": profile, "seed": 808, "tenants": 20, "requested_deliveries": len(items),
		"unique_event_ids": len(unique), "distinct_customer_keys": len(customers), "distinct_invoice_keys": len(invoices),
		"distinct_paid_effects": len(paidEffects), "http_status_counts": codes, "ingress_seconds": ingressSeconds,
		"ingress_p50_ms": m081Percentile(latencies, .50), "ingress_p95_ms": m081Percentile(latencies, .95),
		"ingress_p99_ms": m081Percentile(latencies, .99), "durable_inbox": inbox,
		"crm_converged_30s_percent":      100 * float64(within30) / float64(len(unique)),
		"crm_converged_60s_percent":      100 * float64(within60) / float64(len(unique)),
		"crm_converged_120s_percent":     100 * float64(within120) / float64(len(unique)),
		"crm_latency_completed_p50_s":    m081Percentile(converged, .50),
		"crm_latency_completed_p95_s":    m081Percentile(converged, .95),
		"crm_latency_completed_p99_s":    m081Percentile(converged, .99),
		"crm_unconverged_at_observation": unconverged, "crm_completed_event_actions": completedCRMActions,
		"crm_missing_event_actions":         missingCRMActions,
		"crm_latency_basis":                 "each unique accepted event to its own succeeded CRM action commit; no-op compares count as completion; duplicate Event IDs excluded",
		"crm_latency_percentiles_censoring": "completed_only; unresolved reported separately",
		"business_actions":                  actions, "succeeded_actions": succeeded, "unfinished_obligations": unfinished,
		"runs": runCount, "succeeded_runs": runSucceeded,
		"unpublished_outbox_at_ingress": outboxAtIngress, "pending_jobs_at_ingress": jobsAtIngress,
		"peak_unpublished_outbox": peakOutbox.Load(), "peak_pending_jobs": peakJobs.Load(),
		"peak_unfinished_actions": peakActions.Load(), "peak_running_actions": peakRunning.Load(),
		"queue_timeline_10s": queueTimeline, "per_tenant_crm_progress": perTenant,
		"outage_tenant_latest_observed_convergence_after_restore_s": outageRecoverySeconds,
		"outbox_publish_p50_s": outboxP50, "outbox_publish_p95_s": outboxP95,
		"planning_p50_s": planningP50, "planning_p95_s": planningP95,
		"provider_http_completed_p95_s": providerP95, "action_created_to_first_attempt_p95_s": firstAttemptP95,
		"succeeded_run_accept_to_finish_p95_s": runP95,
		"db_pool_peak_acquired":                peakPool.Load(), "db_deadlocks": deadlocks,
		"go_heap_mib": float64(memory.HeapAlloc) / 1048576,
		"go_process_user_cpu_seconds": float64(resourceAfter.Utime.Sec-resourceBefore.Utime.Sec) +
			float64(resourceAfter.Utime.Usec-resourceBefore.Utime.Usec)/1e6,
		"provider_http_requests": providerCounts, "actual_contact_writes": writes, "contact_count": contactCount,
		"actual_slack_sends": messages, "simulated_429": got429, "simulated_503": got503,
		"fault_scope":         map[string]string{"A": "none", "B": "tenant connection", "C": "one customer per tenant"}[profile],
		"observation_seconds": end.Sub(start).Seconds(), "worker_path": "JetStream action pointer",
	}
	encoded, _ := json.Marshal(result)
	t.Logf("MISSION081_LOAD_JSON %s", encoded)
	if codes[202]+codes[200] != len(items) || inbox != int64(len(unique)) || messages > len(paidEffects) ||
		(profile == "A" && (got429 != 0 || got503 != 0)) || (profile != "A" && (got429 == 0 || got503 == 0)) {
		t.Fatalf("profile invariant failed: accepted=%d duplicate=%d inbox=%d Slack=%d 429=%d 503=%d",
			codes[202], codes[200], inbox, messages, got429, got503)
	}
}
