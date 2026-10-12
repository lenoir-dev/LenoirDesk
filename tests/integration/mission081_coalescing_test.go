//go:build integration

package integration

import (
	"context"
	"testing"
)

func TestMission081PendingCRMCoalescingPreservesRuns(t *testing.T) {
	v := newWorld()
	f, _ := seedBusiness(t, v)
	const firstWave = 12
	ids := make([]string, firstWave)
	for i := range ids {
		ids[i] = deliverBusiness(t, f, v, "customer.subscription.updated")
	}
	await(t, func() bool {
		var planned, actions, links int
		err := mig.QueryRow(context.Background(), `SELECT
		 (SELECT count(*) FROM planning_jobs WHERE tenant_id=$1 AND status='planned'),
		 (SELECT count(*) FROM business_actions WHERE tenant_id=$1 AND kind='crm_projection'),
		 (SELECT count(*) FROM business_run_actions WHERE tenant_id=$1 AND kind='crm_projection')`,
			f.tenantID).Scan(&planned, &actions, &links)
		return err == nil && planned == firstWave && actions == 1 && links == firstWave
	})
	startExecutor(t, v)
	for _, id := range ids {
		waitBusinessStatus(t, f.tenantID, id, "succeeded")
	}
	if writes, messages := externalCounts(v); writes != 1 || messages != 0 {
		t.Fatalf("shared CRM projection effects: writes=%d Slack=%d", writes, messages)
	}
	// A later event cannot inherit an already completed action: it must take
	// another current-state snapshot, which becomes a verified no-op here.
	later := deliverBusiness(t, f, v, "customer.subscription.updated")
	await(t, func() bool { return actionCount(t, f.tenantID, "crm_projection") == 2 })
	waitBusinessStatus(t, f.tenantID, later, "succeeded")
	if writes, messages := externalCounts(v); writes != 1 || messages != 0 {
		t.Fatalf("later event duplicated an effect: writes=%d Slack=%d", writes, messages)
	}
	var totalRuns, totalLinks int
	if err := mig.QueryRow(context.Background(), `SELECT
	 (SELECT count(*) FROM runs WHERE tenant_id=$1),
	 (SELECT count(*) FROM business_run_actions WHERE tenant_id=$1 AND kind='crm_projection')`, f.tenantID).
		Scan(&totalRuns, &totalLinks); err != nil {
		t.Fatal(err)
	}
	if totalRuns != firstWave+1 || totalLinks != firstWave+1 {
		t.Fatalf("durable event obligations missing: runs=%d links=%d", totalRuns, totalLinks)
	}
}
