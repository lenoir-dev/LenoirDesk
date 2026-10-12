//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
	"github.com/lenoir-dev/LenoirDesk/internal/telemetry"
	"go.opentelemetry.io/otel"
)

// This gate starts with a signed webhook and asks the real executor to perform
// both business actions. Tempo must return spans from those actions, rather
// than a standalone span sent only to test the collector.
func TestMission081BusinessActionTraceInTempo(t *testing.T) {
	if os.Getenv("RELAYDESK_M081_TRACE") != "1" {
		t.Skip("use scripts/mission081_tempo.py")
	}
	queryURL := os.Getenv("RELAYDESK_M081_TEMPO_QUERY_URL")
	if os.Getenv("RELAYDESK_OTLP_TRACES_URL") == "" || queryURL == "" {
		t.Fatal("Tempo OTLP and query URLs are required")
	}
	shutdown, err := telemetry.Setup()
	if err != nil {
		t.Fatal(err)
	}
	flushed := false
	defer func() {
		if flushed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			t.Errorf("trace flush: %v", err)
		}
	}()
	ctx, root := otel.Tracer("relaydesk/acceptance").Start(context.Background(), "mission081.owner.billing")
	traceID := root.SpanContext().TraceID().String()
	v := newWorld()
	f, _ := seedBusiness(t, v)
	stripeHTTP := httptest.NewServer(http.HandlerFunc(v.stripe))
	hubspotHTTP := httptest.NewServer(http.HandlerFunc(v.hubspot))
	slackHTTP := httptest.NewServer(http.HandlerFunc(v.slack))
	defer stripeHTTP.Close()
	defer hubspotHTTP.Close()
	defer slackHTTP.Close()
	x := &execution.Executor{DB: db, Box: box, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Endpoints: providers.Endpoints{Stripe: stripeHTTP.URL, HubSpot: hubspotHTTP.URL,
			Slack: slackHTTP.URL, Simulation: true}}
	eventID := deliverBusiness(t, f, v, "invoice.paid")
	var jobID string
	var generation int64
	err = mig.QueryRow(ctx, `SELECT j.id,j.dispatch_generation FROM planning_jobs j
	 JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
	 WHERE j.tenant_id=$1 AND e.event_id=$2`, f.tenantID, eventID).Scan(&jobID, &generation)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Plan(ctx, f.tenantID, jobID, generation); err != nil {
		t.Fatal(err)
	}
	// The fixed coalescing interval starts at the durable action creation time.
	time.Sleep(1200 * time.Millisecond)
	for _, kind := range []string{"crm_projection", "slack_invoice_paid"} {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err = db.QueueBusiness(ctx); err != nil {
				t.Fatal(err)
			}
			var actionID string
			var dispatch int64
			err = mig.QueryRow(ctx, `SELECT id,dispatch_generation FROM business_actions
			 WHERE tenant_id=$1 AND kind=$2 AND status='queued' ORDER BY created_at DESC LIMIT 1`,
				f.tenantID, kind).Scan(&actionID, &dispatch)
			if err == nil {
				ack, executeErr := x.ExecutePointer(ctx, f.tenantID, actionID, dispatch)
				if executeErr != nil || !ack {
					t.Fatalf("%s pointer: ack=%t err=%v", kind, ack, executeErr)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s action not queued: %v", kind, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitBusinessStatus(t, f.tenantID, eventID, "succeeded")
	if writes, messages := externalCounts(v); writes != 1 || messages != 1 {
		t.Fatalf("business effects: Contact writes=%d Slack sends=%d", writes, messages)
	}
	root.End()
	flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err = shutdown(flushCtx); err != nil {
		cancel()
		t.Fatal(err)
	}
	flushed = true
	cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, getErr := client.Get(strings.TrimRight(queryURL, "/") + "/api/traces/" + traceID)
		if getErr == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK {
				serialized := string(body)
				if strings.Contains(serialized, "business.action.execute") && strings.Contains(serialized, "provider.http") {
					for _, forbidden := range []string{f.secret, v.customer, "sim_hubspot", "sim_slack", "sk_test_simulated", "Authorization", "Bearer "} {
						if strings.Contains(serialized, forbidden) {
							t.Fatalf("trace contains a forbidden secret or identity field")
						}
					}
					t.Logf("Tempo business trace verified: trace_id=%s action and provider spans present", traceID)
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("Tempo did not return the business action and provider spans")
}
