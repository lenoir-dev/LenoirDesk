//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/telemetry"
)

var mission07MetricsOnce sync.Once
var mission07MetricsError error
var mission07MetricsShutdown func(context.Context) error

func mission07Metrics(t *testing.T) {
	t.Helper()
	mission07MetricsOnce.Do(func() {
		t.Setenv("RELAYDESK_METRICS_ADDR", "127.0.0.1:0")
		mission07MetricsShutdown, mission07MetricsError = telemetry.Setup()
	})
	if mission07MetricsError != nil {
		t.Fatal(mission07MetricsError)
	}
}

func TestMission07OutboxFaultProducesActionableMetric(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	eventID := "evt_" + randomHex(10)
	if status := post(t, f, billingPayload(eventID, "invoice.paid", v.customer, v.invoice, v.subscription), time.Now(), f.secret); status != 202 {
		t.Fatalf("webhook status=%d", status)
	}
	ctx := context.Background()
	await(t, func() bool {
		var n int
		_ = mig.QueryRow(ctx, `SELECT count(*) FROM business_actions WHERE tenant_id=$1 AND kind='slack_invoice_paid'`, f.tenantID).Scan(&n)
		return n == 1
	})
	var outboxID string
	err := mig.QueryRow(ctx, `INSERT INTO business_action_outbox(tenant_id,action_id,dispatch_generation,created_at)
	 SELECT tenant_id,id,1,now()-interval '10 minutes' FROM business_actions
	 WHERE tenant_id=$1 AND kind='slack_invoice_paid' RETURNING id`, f.tenantID).Scan(&outboxID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = mig.Exec(context.Background(), `DELETE FROM business_action_outbox WHERE tenant_id=$1 AND id=$2`, f.tenantID, outboxID)
	})
	h, err := db.BusinessHealth(ctx)
	if err != nil || h.OldestUnpublishedSeconds < 600 {
		t.Fatalf("fault not observed by PostgreSQL: age=%d err=%v", h.OldestUnpublishedSeconds, err)
	}
	ready, err := http.Get(server.URL + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	_ = ready.Body.Close()
	if ready.StatusCode != http.StatusNoContent {
		t.Fatalf("dependency readiness status=%d", ready.StatusCode)
	}
	// Readiness describes dependencies, while the separate business metric
	// exposes the stalled outbox. Both observations must be available.
	mission07Metrics(t)
	telemetry.UpdateOperational(map[string]int64{"oldest_outbox_seconds": h.OldestUnpublishedSeconds})
	w := httptest.NewRecorder()
	telemetry.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `measure="oldest_outbox_seconds"`) ||
		!strings.Contains(w.Body.String(), "relaydesk_operational_value") {
		t.Fatalf("fault was not exported: status=%d", w.Code)
	}
}

func TestMission07UnknownAndDeadActionAgesRemainObservable(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	ctx := context.Background()
	tx, err := mig.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `INSERT INTO business_actions(tenant_id,automation_revision_id,kind,effect_key,
 stripe_connection_id,customer_id,invoice_id,status,updated_at)
 VALUES($1,$2,'slack_invoice_paid',$3,$4,$5,$6,'unknown',now()-interval '2 hours'),
 ($1,$2,'crm_projection',$7,$4,$5,'','dead',now()-interval '3 hours')`,
		f.tenantID, f.revisionID, "observability-unknown-"+randomHex(8), f.connectionID,
		v.customer, v.invoice, "observability-dead-"+randomHex(8))
	if err != nil {
		t.Fatal(err)
	}
	var unknown, dead, unknownAge, deadAge int64
	if err = tx.QueryRow(ctx, `SELECT unknown_actions,dead_actions,oldest_unknown_seconds
 FROM business_recovery_health()`).Scan(&unknown, &dead, &unknownAge); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT oldest_dead_seconds FROM operational_metrics()`).Scan(&deadAge); err != nil {
		t.Fatal(err)
	}
	if unknown < 1 || dead < 1 || unknownAge < 7200 || deadAge < 10800 {
		t.Fatalf("unresolved effects hidden: unknown=%d dead=%d unknown_age=%d dead_age=%d",
			unknown, dead, unknownAge, deadAge)
	}
}

func TestMission07ProviderAuthFailureExportsSafeMetric(t *testing.T) {
	v := newWorld()
	f, config := seedBusiness(t, v)
	ciphertext, nonce, err := box.Seal([]byte("invalid-simulator-credential"), f.tenantID,
		config.HubSpotConnectionID, "hubspot", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = mig.Exec(context.Background(), `UPDATE connection_credentials SET ciphertext=$3,nonce=$4
 WHERE tenant_id=$1 AND connection_id=$2`, f.tenantID, config.HubSpotConnectionID, ciphertext, nonce); err != nil {
		t.Fatal(err)
	}
	mission07Metrics(t)
	startExecutor(t, v)
	eventID := "evt_" + randomHex(10)
	if status := post(t, f, billingPayload(eventID, "customer.subscription.updated", v.customer, v.invoice, v.subscription), time.Now(), f.secret); status != http.StatusAccepted {
		t.Fatalf("auth-failure event status=%d", status)
	}
	waitBusinessStatus(t, f.tenantID, eventID, "blocked_auth")
	metric := httptest.NewRecorder()
	telemetry.MetricsHandler().ServeHTTP(metric, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := metric.Body.String()
	if metric.Code != http.StatusOK || !strings.Contains(body, `kind="blocked_auth"`) ||
		!strings.Contains(body, `provider="hubspot"`) || strings.Contains(body, "invalid-simulator-credential") {
		t.Fatalf("auth failure metric absent or unsafe: status=%d", metric.Code)
	}
}
