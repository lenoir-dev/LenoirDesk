//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/lenoir-dev/LenoirDesk/internal/retention"
)

func TestMission07RetentionPreservesEffectIdentity(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	startExecutor(t, v)
	eventID := deliverBusiness(t, f, v, "invoice.paid")
	waitBusinessStatus(t, f.tenantID, eventID, "succeeded")
	ctx := context.Background()
	_, err := mig.Exec(ctx, `UPDATE webhook_events SET received_at=now()-interval '8 days'
 WHERE tenant_id=$1 AND event_id=$2`, f.tenantID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mig.Exec(ctx, `UPDATE runs SET finished_at=now()-interval '31 days' WHERE tenant_id=$1`, f.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mig.Exec(ctx, `UPDATE business_attempts SET finished_at=now()-interval '31 days' WHERE tenant_id=$1`, f.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mig.Exec(ctx, `UPDATE business_provider_calls SET finished_at=now()-interval '31 days' WHERE tenant_id=$1`, f.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	policy := retention.DefaultPolicy()
	policy.BatchSize = 500
	first, err := retention.Batch(ctx, db, policy)
	if err != nil {
		t.Fatal(err)
	}
	if first.RawRedacted < 1 || first.RunsRemoved < 1 || first.AttemptsRemoved < 1 || first.ProviderCallsRemoved < 1 {
		t.Fatalf("retention did not cover terminal synthetic records: %+v", first)
	}
	var rawBytes, events, effects, runs, attempts int
	err = mig.QueryRow(ctx, `SELECT length(raw_payload) FROM webhook_events WHERE tenant_id=$1 AND event_id=$2`, f.tenantID, eventID).Scan(&rawBytes)
	if err != nil || rawBytes != 0 {
		t.Fatalf("raw payload was not redacted: bytes=%d err=%v", rawBytes, err)
	}
	if err = mig.QueryRow(ctx, `SELECT count(*) FROM webhook_events WHERE tenant_id=$1 AND event_id=$2`, f.tenantID, eventID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err = mig.QueryRow(ctx, `SELECT count(*) FROM business_actions WHERE tenant_id=$1`, f.tenantID).Scan(&effects); err != nil {
		t.Fatal(err)
	}
	if err = mig.QueryRow(ctx, `SELECT count(*) FROM runs WHERE tenant_id=$1`, f.tenantID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err = mig.QueryRow(ctx, `SELECT count(*) FROM business_attempts WHERE tenant_id=$1`, f.tenantID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if events != 1 || effects != 2 || runs != 0 || attempts != 0 {
		t.Fatalf("tombstone/cleanup mismatch events=%d effects=%d runs=%d attempts=%d", events, effects, runs, attempts)
	}
	second, err := retention.Batch(ctx, db, policy)
	if err != nil {
		t.Fatal(err)
	}
	if second.RawRedacted != 0 || second.RunsRemoved != 0 {
		t.Fatalf("retention was not repeatable: %+v", second)
	}
}

func TestMission07RetentionCannotEraseUnfinishedObligations(t *testing.T) {
	f := seed(t, true)
	ctx := context.Background()
	tx, err := mig.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var eventRow, jobID string
	err = tx.QueryRow(ctx, `INSERT INTO webhook_events(tenant_id,connection_id,provider,
 provider_account_id,mode,event_id,event_type,event_api_version,raw_payload,
 classification,automation_revision_id,correlation_id,received_at)
 VALUES($1,$2,'stripe',$3,'test',$4,'customer.subscription.updated',
 '2026-08-26.dahlia',convert_to('synthetic unfinished payload','UTF8'),
 'supported',$5,'retention-unfinished',now()-interval '8 days') RETURNING id`,
		f.tenantID, f.connectionID, f.account, "evt_"+randomHex(8), f.revisionID).Scan(&eventRow)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `INSERT INTO planning_jobs(tenant_id,event_row_id,automation_revision_id,
 status,created_at) VALUES($1,$2,$3,'pending',now()-interval '31 days') RETURNING id`,
		f.tenantID, eventRow, f.revisionID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO business_actions(tenant_id,automation_revision_id,kind,
 effect_key,stripe_connection_id,customer_id,status,created_at)
 VALUES($1,$2,'crm_projection',$3,$4,'cus_unfinished','pending',now()-interval '31 days')`,
		f.tenantID, f.revisionID, "retention-pending-"+randomHex(8), f.connectionID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT * FROM retention_batch(100,7,30,90)`); err != nil {
		t.Fatal(err)
	}
	var rawLength, jobs, actions int
	if err = tx.QueryRow(ctx, `SELECT length(raw_payload) FROM webhook_events WHERE tenant_id=$1 AND id=$2`,
		f.tenantID, eventRow).Scan(&rawLength); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM planning_jobs WHERE tenant_id=$1 AND id=$2 AND status='pending'`,
		f.tenantID, jobID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM business_actions WHERE tenant_id=$1 AND status='pending'`,
		f.tenantID).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if rawLength == 0 || jobs != 1 || actions != 1 {
		t.Fatalf("retention erased unfinished data: raw=%d jobs=%d actions=%d", rawLength, jobs, actions)
	}
}
