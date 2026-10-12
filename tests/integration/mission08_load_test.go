//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
	"github.com/redis/go-redis/v9"
	"github.com/stripe/stripe-go/v86/webhook"
)

// Run only through scripts/mission08_load.py against disposable infrastructure.
func TestMission08LoadProfile(t *testing.T) {
	if os.Getenv("RELAYDESK_M08_LOAD") != "1" {
		t.Skip("run the isolated Mission 08 load harness")
	}
	redisURL := os.Getenv("RELAYDESK_M08_REDIS_URL")
	if redisURL == "" {
		t.Fatal("isolated Redis URL required")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	redisClient := redis.NewClient(options)
	defer redisClient.Close()
	if err = redisClient.Ping(context.Background()).Err(); err != nil {
		t.Fatal("isolated Redis unavailable")
	}
	v := newWorld()
	fixtures := make([]fixture, 20)
	for i := range fixtures {
		fixtures[i], _ = seedBusiness(t, v)
	}
	outageKey, err := business.CustomerKey(fixtures[1].account, "test", v.customer)
	if err != nil {
		t.Fatal(err)
	}
	rateKey, err := business.CustomerKey(fixtures[2].account, "test", v.customer)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var providerMu sync.Mutex
	rateResponses := 0
	outageResponses := 0
	contacts := &loadHubspotSimulator{nextID: 200000, byKey: map[string]loadContact{}, byID: map[string]string{}, world: v}
	stripeHTTP := httptest.NewServer(http.HandlerFunc(v.stripe))
	hubspotHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		contacts.ServeHTTP(w, r)
	}))
	slackHTTP := httptest.NewServer(http.HandlerFunc(v.slack))
	defer stripeHTTP.Close()
	defer hubspotHTTP.Close()
	defer slackHTTP.Close()
	ctx, cancel := context.WithCancel(context.Background())
	x := &execution.Executor{DB: db, Box: box, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Endpoints: providers.Endpoints{Stripe: stripeHTTP.URL, HubSpot: hubspotHTTP.URL,
			Slack: slackHTTP.URL, Simulation: true}, RequireLimiter: true,
		Limiter: &execution.RedisWriteLimiter{Client: redisClient, Limit: 5, Window: time.Second}}
	done := make(chan struct{})
	go func() { defer close(done); x.Run(ctx) }()
	defer func() { cancel(); <-done }()
	var peakPool atomic.Int64
	poolDone := make(chan struct{})
	go func() {
		defer close(poolDone)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				active := int64(db.Pool.Stat().AcquiredConns())
				for active > peakPool.Load() && !peakPool.CompareAndSwap(peakPool.Load(), active) {
				}
			}
		}
	}()
	defer func() { cancel(); <-poolDone }()
	var before, after syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &before)
	var durationsMu sync.Mutex
	durations := make([]time.Duration, 0, 6000)
	codes := make(map[int]int)
	var wg sync.WaitGroup
	slots := make(chan struct{}, 64)
	rng := rand.New(rand.NewSource(808))
	suffix := randomHex(6)
	client := &http.Client{Timeout: 10 * time.Second}
	var previousFixture fixture
	var previousID string
	for i := 0; i < 6000; i++ {
		time.Sleep(time.Until(start.Add(time.Duration(i) * 10 * time.Millisecond)))
		idx := 0
		if i%2 != 0 {
			idx = 1 + rng.Intn(19)
		}
		f := fixtures[idx]
		id := "evt_m08_" + suffix + "_" + strconv.Itoa(i)
		if i > 0 && i%100 == 0 {
			id, f = previousID, previousFixture
		} else {
			previousID, previousFixture = id, f
		}
		body := billingPayload(id, "invoice.paid", v.customer, v.invoice, v.subscription)
		signature := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
			Payload: body, Secret: f.secret, Timestamp: time.Now()})
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
			durationsMu.Lock()
			durations = append(durations, elapsed)
			codes[code]++
			durationsMu.Unlock()
		}(f, body, signature.Header)
	}
	wg.Wait()
	ingressSeconds := time.Since(start).Seconds()
	var outboxAtIngress, pendingJobsAtIngress int64
	if err = mig.QueryRow(context.Background(), `SELECT
 (SELECT count(*) FROM outbox WHERE published_at IS NULL),
 (SELECT count(*) FROM planning_jobs WHERE status='pending')`).Scan(&outboxAtIngress, &pendingJobsAtIngress); err != nil {
		t.Fatal(err)
	}
	redisKeysAtIngress, redisErr := redisClient.DBSize(context.Background()).Result()
	if redisErr != nil {
		t.Fatal(redisErr)
	}
	// This fixed observation window reports a censored drain, never an invented
	// completion. It also covers the end of the 60-second synthetic outage.
	deadline := time.Now().Add(120 * time.Second)
	var unfinished int
	for {
		if err = mig.QueryRow(context.Background(), `SELECT
 (SELECT count(*) FROM planning_jobs WHERE status='pending')+
 (SELECT count(*) FROM business_actions WHERE status IN ('pending','queued','running','retry_wait'))`).Scan(&unfinished); err != nil {
			t.Fatal(err)
		}
		if unfinished == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &after)
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	percentile := func(p float64) float64 {
		if len(durations) == 0 {
			return 0
		}
		index := int(float64(len(durations)-1) * p)
		return float64(durations[index].Microseconds()) / 1000
	}
	var inbox, actions, succeeded, providerCalls, unknown, outbox, deadlocks, oldestPendingSeconds int64
	var syncWithin60 float64
	ctxQuery := context.Background()
	_ = mig.QueryRow(ctxQuery, `SELECT count(*) FROM webhook_events WHERE event_id LIKE $1`, "evt_m08_"+suffix+"_%").Scan(&inbox)
	_ = mig.QueryRow(ctxQuery, `SELECT count(*),count(*) FILTER (WHERE status='succeeded'),
 count(*) FILTER (WHERE status='unknown') FROM business_actions`).Scan(&actions, &succeeded, &unknown)
	_ = mig.QueryRow(ctxQuery, `SELECT count(*) FROM business_provider_calls`).Scan(&providerCalls)
	_ = mig.QueryRow(ctxQuery, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&outbox)
	_ = mig.QueryRow(ctxQuery, `SELECT deadlocks FROM pg_stat_database WHERE datname=current_database()`).Scan(&deadlocks)
	_ = mig.QueryRow(ctxQuery, `SELECT coalesce(max(extract(epoch FROM now()-created_at))::bigint,0)
 FROM business_actions WHERE status IN ('pending','queued','running','retry_wait')`).Scan(&oldestPendingSeconds)
	_ = mig.QueryRow(ctxQuery, `SELECT coalesce(100.0*count(*) FILTER (WHERE a.status='succeeded'
 AND a.updated_at<=e.received_at+interval '60 seconds')/nullif(count(*),0),0)
 FROM business_actions a JOIN business_run_actions l ON l.tenant_id=a.tenant_id AND l.action_id=a.id
 JOIN runs r ON r.tenant_id=l.tenant_id AND r.id=l.run_id
 JOIN planning_jobs j ON j.tenant_id=r.tenant_id AND j.id=r.planning_job_id
 JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
 WHERE a.kind='crm_projection' AND e.event_id LIKE $1`, "evt_m08_"+suffix+"_%").Scan(&syncWithin60)
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	v.mu.Lock()
	messages, writes := v.slackMessages, v.hubspotWrites
	v.mu.Unlock()
	providerMu.Lock()
	got429, got503 := rateResponses, outageResponses
	providerMu.Unlock()
	perTenant := make(map[string]map[string]int64, len(fixtures))
	for i, f := range fixtures {
		rows, queryErr := mig.Query(ctxQuery, `SELECT status,count(*) FROM business_actions
 WHERE tenant_id=$1 AND kind='crm_projection' GROUP BY status`, f.tenantID)
		if queryErr != nil {
			t.Fatal(queryErr)
		}
		progress := make(map[string]int64)
		for rows.Next() {
			var status string
			var count int64
			if scanErr := rows.Scan(&status, &count); scanErr != nil {
				rows.Close()
				t.Fatal(scanErr)
			}
			progress[status] = count
		}
		if rows.Err() != nil {
			t.Fatal(rows.Err())
		}
		rows.Close()
		perTenant[strconv.Itoa(i)] = progress
	}
	redisKeys, redisErr := redisClient.DBSize(ctxQuery).Result()
	if redisErr != nil {
		t.Fatal(redisErr)
	}
	classCounts := map[string]int64{}
	classRows, classErr := mig.Query(ctxQuery, `SELECT status,coalesce(error_class,''),count(*)
 FROM business_actions GROUP BY status,error_class`)
	if classErr != nil {
		t.Fatal(classErr)
	}
	for classRows.Next() {
		var status, class string
		var count int64
		if scanErr := classRows.Scan(&status, &class, &count); scanErr != nil {
			classRows.Close()
			t.Fatal(scanErr)
		}
		classCounts[status+"/"+class] = count
	}
	if classRows.Err() != nil {
		t.Fatal(classRows.Err())
	}
	classRows.Close()
	result := map[string]any{
		"requested_events": 6000, "tenants": 20, "ingress_seconds": ingressSeconds,
		"http_status_counts": codes, "acceptance_p50_ms": percentile(.50),
		"acceptance_p95_ms": percentile(.95), "acceptance_p99_ms": percentile(.99),
		"durable_inbox": inbox, "business_actions": actions, "succeeded_actions": succeeded,
		"unknown_actions": unknown, "unpublished_outbox": outbox, "unpublished_outbox_at_ingress": outboxAtIngress,
		"pending_jobs_at_ingress": pendingJobsAtIngress, "oldest_pending_seconds": oldestPendingSeconds,
		"unfinished_after_120s": unfinished, "per_tenant_crm_progress": perTenant,
		"crm_within_60s_percent": syncWithin60, "provider_http_calls": providerCalls,
		"simulated_hubspot_writes": writes, "simulated_slack_messages": messages,
		"simulated_429": got429, "simulated_503": got503, "redis_keys_at_ingress": redisKeysAtIngress,
		"redis_keys_at_end": redisKeys, "action_class_counts": classCounts,
		"configured_worker_capacity": 4, "reconciliation_lag": "not_exercised_in_ingress_profile",
		"go_heap_mib": float64(memory.HeapAlloc) / 1048576,
		"go_process_user_cpu_seconds": float64(after.Utime.Sec-before.Utime.Sec) +
			float64(after.Utime.Usec-before.Utime.Usec)/1e6,
		"db_pool_acquired_at_end": db.Pool.Stat().AcquiredConns(), "db_pool_peak_acquired": peakPool.Load(),
		"db_deadlocks": deadlocks,
	}
	encoded, _ := json.Marshal(result)
	t.Logf("MISSION08_LOAD_JSON %s", encoded)
	if codes[202]+codes[200] != 6000 || inbox != int64(codes[202]) || messages > 20 || got429 == 0 || got503 == 0 {
		t.Fatalf("load invariant failed: accepted=%d duplicate=%d inbox=%d Slack=%d", codes[202], codes[200], inbox, messages)
	}
}

type loadContact struct {
	id         string
	properties map[string]string
}

// The shared integration fixture uses Contact 101 for every key. This load
// simulator needs stable distinct IDs for different tenant customer keys.
type loadHubspotSimulator struct {
	mu         sync.Mutex
	nextID     int
	byKey      map[string]loadContact
	byID       map[string]string
	firstWrite map[string]time.Time
	world      *providerWorld
}

func (s *loadHubspotSimulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer sim_hubspot" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	const prefix = "/crm/v3/objects/contacts/"
	if strings.HasPrefix(r.URL.Path, prefix) && r.Method == http.MethodGet {
		lookup := strings.TrimPrefix(r.URL.Path, prefix)
		key := lookup
		if r.URL.Query().Get("idProperty") == "" {
			key = s.byID[lookup]
		}
		contact, ok := s.byKey[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		jsonResponse(w, map[string]any{"id": contact.id, "properties": contact.properties, "archived": false})
		return
	}
	if r.URL.Path == prefix+"batch/upsert" && r.Method == http.MethodPost {
		var request struct {
			Inputs []struct {
				ID         string            `json:"id"`
				IDProperty string            `json:"idProperty"`
				Properties map[string]string `json:"properties"`
			} `json:"inputs"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Inputs) != 1 ||
			request.Inputs[0].IDProperty != "relaydesk_customer_key" ||
			request.Inputs[0].Properties["relaydesk_customer_key"] != request.Inputs[0].ID {
			http.Error(w, "invalid synthetic Contact upsert", http.StatusBadRequest)
			return
		}
		key := request.Inputs[0].ID
		contact, ok := s.byKey[key]
		if !ok {
			s.nextID++
			contact.id = strconv.Itoa(s.nextID)
			s.byID[contact.id] = key
		}
		contact.properties = request.Inputs[0].Properties
		s.byKey[key] = contact
		if s.firstWrite != nil && s.firstWrite[key].IsZero() {
			s.firstWrite[key] = time.Now()
		}
		s.world.mu.Lock()
		s.world.hubspotWrites++
		s.world.mu.Unlock()
		jsonResponse(w, map[string]any{"status": "COMPLETE", "results": []any{map[string]any{
			"id": contact.id, "properties": contact.properties, "archived": false}}})
		return
	}
	if strings.HasPrefix(r.URL.Path, prefix) && r.Method == http.MethodPatch {
		id := strings.TrimPrefix(r.URL.Path, prefix)
		key, ok := s.byID[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Properties map[string]string `json:"properties"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Properties["relaydesk_customer_key"] != key {
			http.Error(w, "invalid synthetic Contact patch", http.StatusBadRequest)
			return
		}
		contact := s.byKey[key]
		contact.properties = request.Properties
		s.byKey[key] = contact
		if s.firstWrite != nil && s.firstWrite[key].IsZero() {
			s.firstWrite[key] = time.Now()
		}
		s.world.mu.Lock()
		s.world.hubspotWrites++
		s.world.mu.Unlock()
		jsonResponse(w, map[string]any{"id": contact.id, "properties": contact.properties, "archived": false})
		return
	}
	http.NotFound(w, r)
}
