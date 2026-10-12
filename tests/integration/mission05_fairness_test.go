//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestMission05TenantFloodDoesNotMonopolizeDispatch(t *testing.T) {
	ctx := context.Background()
	flood, _ := seedBusiness(t, newWorld())
	other, _ := seedBusiness(t, newWorld())
	for i := 0; i < 12; i++ {
		_, err := mig.Exec(ctx, `INSERT INTO business_actions(tenant_id,automation_revision_id,kind,effect_key,stripe_connection_id,customer_id,created_at)
 VALUES($1,$2,'crm_projection',$3,$4,$5,now()-interval '1 hour')`, flood.tenantID, flood.revisionID,
			fmt.Sprintf("flood-%d", i), flood.connectionID, fmt.Sprintf("flood-customer-%d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	// Both tenants must have actions past the fixed CRM collection window;
	// this test measures queue fairness, not admission of a fresh aggregate.
	if _, err := mig.Exec(ctx, `INSERT INTO business_actions(tenant_id,automation_revision_id,kind,effect_key,stripe_connection_id,customer_id,created_at)
 VALUES($1,$2,'crm_projection','other-tenant',$3,'other-customer',now()-interval '1 hour')`, other.tenantID, other.revisionID, other.connectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `SELECT queue_business_actions(16)`); err != nil {
		t.Fatal(err)
	}
	var floodQueued, otherQueued int
	if err := mig.QueryRow(ctx, `SELECT count(*) FROM business_actions WHERE tenant_id=$1 AND status='queued'`, flood.tenantID).Scan(&floodQueued); err != nil {
		t.Fatal(err)
	}
	if err := mig.QueryRow(ctx, `SELECT count(*) FROM business_actions WHERE tenant_id=$1 AND status='queued'`, other.tenantID).Scan(&otherQueued); err != nil {
		t.Fatal(err)
	}
	if floodQueued > 4 || otherQueued != 1 {
		t.Fatalf("unfair queue admission: flood=%d other=%d", floodQueued, otherQueued)
	}
	rows, err := mig.Query(ctx, `SELECT id,dispatch_generation FROM business_actions WHERE tenant_id=$1 AND status='queued' ORDER BY created_at,id`, flood.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	var generations []int64
	for rows.Next() {
		var id string
		var generation int64
		if err := rows.Scan(&id, &generation); err != nil {
			t.Fatal(err)
		}
		ids, generations = append(ids, id), append(generations, generation)
	}
	rows.Close()
	if len(ids) < 3 {
		t.Fatalf("need 3 queued flood actions, got %d", len(ids))
	}
	for i := 0; i < 3; i++ {
		var actionID string
		err := db.Pool.QueryRow(ctx, `SELECT action_id FROM claim_business_action_pointer($1,$2,$3)`, flood.tenantID, ids[i], generations[i]).Scan(&actionID)
		if i < 2 && (err != nil || actionID != ids[i]) {
			t.Fatalf("first two tenant claims: i=%d err=%v", i, err)
		}
		if i == 2 && err != pgx.ErrNoRows {
			t.Fatalf("third concurrent tenant claim was admitted: err=%v", err)
		}
	}
	var otherID string
	var otherGeneration int64
	if err := mig.QueryRow(ctx, `SELECT id,dispatch_generation FROM business_actions WHERE tenant_id=$1 AND status='queued'`, other.tenantID).Scan(&otherID, &otherGeneration); err != nil {
		t.Fatal(err)
	}
	var claimedOther string
	if err := db.Pool.QueryRow(ctx, `SELECT action_id FROM claim_business_action_pointer($1,$2,$3)`, other.tenantID, otherID, otherGeneration).Scan(&claimedOther); err != nil || claimedOther != otherID {
		t.Fatalf("other tenant did not progress: %s %v", claimedOther, err)
	}
}
