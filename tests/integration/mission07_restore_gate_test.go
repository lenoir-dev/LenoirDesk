//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/crypto"
	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
	"github.com/lenoir-dev/LenoirDesk/internal/reconciliation"
)

func TestMission07RestoreGateStopsOutboundClaims(t *testing.T) {
	ctx := context.Background()
	if _, err := mig.Exec(ctx, `UPDATE restore_safety_gate SET outbound_enabled=false WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := mig.Exec(context.Background(), `UPDATE restore_safety_gate SET outbound_enabled=true WHERE singleton=true`); err != nil {
			t.Errorf("restore test gate cleanup: %v", err)
		}
	})
	x := &execution.Executor{DB: db}
	ack, err := x.ExecutePointer(ctx, "00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002", 1)
	if err != nil || ack {
		t.Fatalf("outbound claim during recovery: ack=%t err=%v", ack, err)
	}
	var allowed bool
	if err = mig.QueryRow(ctx, `SELECT outbound_enabled FROM restore_safety_gate WHERE singleton=true`).Scan(&allowed); err != nil || allowed {
		t.Fatalf("restore gate allowed=%t err=%v", allowed, err)
	}
}

func TestMission07RestoreQuarantinesAmbiguousSlack(t *testing.T) {
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
	// Roll back the destructive test transaction, keeping the shared development
	// database and its operational gate unchanged.
	tx, err := mig.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var affected int64
	if err = tx.QueryRow(ctx, `SELECT enter_restore_recovery('Synthetic isolated recovery transaction')`).Scan(&affected); err != nil || affected < 1 {
		t.Fatalf("quarantine count=%d err=%v", affected, err)
	}
	var state, class string
	if err = tx.QueryRow(ctx, `SELECT status,error_class FROM business_actions WHERE tenant_id=$1 AND kind='slack_invoice_paid'`, f.tenantID).
		Scan(&state, &class); err != nil || state != "unknown" || class != "restore_uncertain" {
		t.Fatalf("quarantine state=%s class=%s err=%v", state, class, err)
	}
	if _, err = tx.Exec(ctx, `SELECT clear_restore_recovery('Synthetic review completed safely')`); err == nil {
		t.Fatal("clearance accepted unresolved notification")
	}
}

func TestMission07WrongRecoveryKeyCannotDiscoverWork(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	ctx := context.Background()
	if _, err := mig.Exec(ctx, `INSERT INTO reconcile_checkpoints(tenant_id,stripe_connection_id,kind,window_start,window_end,next_run_at)
 VALUES($1,$2,'events',now()-interval '10 minutes',now()-interval '3 minutes',now())`, f.tenantID, f.connectionID); err != nil {
		t.Fatal(err)
	}
	wrongBox, err := crypto.NewBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	r := &reconciliation.Reconciler{DB: db, Box: wrongBox,
		Endpoints: providers.Endpoints{Stripe: "http://127.0.0.1:1", HubSpot: "http://127.0.0.1:1", Slack: "http://127.0.0.1:1", Simulation: true}}
	worked, err := r.StepTenant(ctx, f.tenantID)
	if err != nil || !worked {
		t.Fatalf("wrong-key scan worked=%t err=%v", worked, err)
	}
	var gap string
	var completed *time.Time
	if err = mig.QueryRow(ctx, `SELECT coalesce(gap_code,''),last_complete_at FROM reconcile_checkpoints
 WHERE tenant_id=$1 AND stripe_connection_id=$2 AND kind='events'`, f.tenantID, f.connectionID).Scan(&gap, &completed); err != nil {
		t.Fatal(err)
	}
	if gap != "credential_unavailable" || completed != nil {
		t.Fatalf("wrong key advanced coverage gap=%q completed=%v", gap, completed)
	}
}

func TestMission07CRMCurrentStateRepairAfterRecoveryClearance(t *testing.T) {
	if os.Getenv("RELAYDESK_RESTORE_DRILL_TARGET") == "1" {
		var started *time.Time
		var quarantined int64
		if err := mig.QueryRow(context.Background(), `SELECT recovery_started_at,quarantined_notifications
 FROM restore_safety_gate WHERE singleton=true`).Scan(&started, &quarantined); err != nil || started == nil || quarantined < 1 {
			t.Fatalf("target is not a reviewed restored database: started=%v quarantined=%d err=%v", started, quarantined, err)
		}
	}
	v := newWorld()
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	initialID := "evt_" + randomHex(10)
	if status := post(t, f, billingPayload(initialID, "customer.subscription.updated", v.customer, v.invoice, v.subscription), time.Now(), f.secret); status != http.StatusAccepted {
		t.Fatalf("initial signed event status=%d", status)
	}
	waitBusinessStatus(t, f.tenantID, initialID, "succeeded")
	v.mu.Lock()
	for _, contact := range v.contacts {
		contact["relaydesk_projection_hash"] = "drift-after-backup"
	}
	v.mu.Unlock()
	ctx := context.Background()
	if _, err := mig.Exec(ctx, `UPDATE restore_safety_gate SET outbound_enabled=false WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := mig.Exec(context.Background(), `UPDATE restore_safety_gate SET outbound_enabled=true WHERE singleton=true`); err != nil {
			t.Errorf("restore gate cleanup: %v", err)
		}
	})
	if _, err := mig.Exec(ctx, `INSERT INTO reconcile_checkpoints(tenant_id,stripe_connection_id,kind,window_start,window_end,next_run_at)
 VALUES($1,$2,'known',now()-interval '10 minutes',now()-interval '3 minutes',now())`, f.tenantID, f.connectionID); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(v.stripe))
	defer server.Close()
	r := &reconciliation.Reconciler{DB: db, Box: box,
		Endpoints: providers.Endpoints{Stripe: server.URL, HubSpot: server.URL, Slack: server.URL, Simulation: true}}
	worked, err := r.StepTenant(ctx, f.tenantID)
	if err != nil || !worked {
		t.Fatalf("post-recovery known-state scan worked=%t err=%v", worked, err)
	}
	var knownID string
	if err := mig.QueryRow(ctx, `SELECT event_id FROM webhook_events WHERE tenant_id=$1 AND source='known_state' ORDER BY received_at DESC LIMIT 1`, f.tenantID).Scan(&knownID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if writes, messages := externalCounts(v); writes != 1 || messages != 0 {
		t.Fatalf("recovery gate leaked outbound writes=%d messages=%d", writes, messages)
	}
	if _, err := mig.Exec(ctx, `UPDATE restore_safety_gate SET outbound_enabled=true WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	waitBusinessStatus(t, f.tenantID, knownID, "succeeded")
	if writes, messages := externalCounts(v); writes != 2 || messages != 0 {
		t.Fatalf("current-state repair after clearance writes=%d messages=%d", writes, messages)
	}
}
