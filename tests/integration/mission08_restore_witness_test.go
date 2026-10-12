//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lenoir-dev/LenoirDesk/internal/recoverywitness"
)

func TestMission08ExternalWitnessRejectsOlderDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w := &recoverywitness.Witness{Pool: mig, Path: filepath.Join(dir, "state.json")}
	var initial int64
	if err := mig.QueryRow(ctx, `SELECT effect_sequence FROM restore_witness_state WHERE singleton=true`).Scan(&initial); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Exec(ctx, `UPDATE restore_witness_state SET effect_sequence=0 WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := mig.Exec(context.Background(), `UPDATE restore_witness_state SET effect_sequence=$1 WHERE singleton=true`, initial); err != nil {
			t.Errorf("restore witness test cleanup: %v", err)
		}
		if _, err := mig.Exec(context.Background(), `UPDATE restore_safety_gate SET outbound_enabled=true WHERE singleton=true`); err != nil {
			t.Errorf("restore gate test cleanup: %v", err)
		}
	})
	if err := w.Check(ctx); err == nil {
		t.Fatal("missing external state allowed outbound")
	}
	if err := w.Bind(ctx, "Initial isolated test enrollment", false); err != nil {
		t.Fatal(err)
	}
	runtime := &recoverywitness.Witness{Pool: db.Pool, Path: w.Path}
	if err := runtime.Check(ctx); err != nil {
		t.Fatalf("newly enrolled witness rejected: %v", err)
	}
	f, _ := seedBusiness(t, newWorld())
	var completedAction string
	if err := mig.QueryRow(ctx, `INSERT INTO business_actions(tenant_id,automation_revision_id,kind,effect_key,
 stripe_connection_id,customer_id) VALUES($1,$2,'crm_projection',$3,$4,'cus_witness_test') RETURNING id`,
		f.tenantID, f.revisionID, "witness-test-"+randomHex(8), f.connectionID).Scan(&completedAction); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Prepare(ctx, f.tenantID, completedAction); err != nil {
		t.Fatalf("pre-outbound mark failed: %v", err)
	}
	if _, err := mig.Exec(ctx, `UPDATE business_actions SET status='succeeded' WHERE tenant_id=$1 AND id=$2`, f.tenantID, completedAction); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Complete(ctx, f.tenantID, completedAction); err != nil {
		t.Fatalf("durable action did not clear in-flight mark: %v", err)
	}
	if err := (&recoverywitness.Witness{Pool: db.Pool, Path: w.Path}).Check(ctx); err != nil {
		t.Fatalf("ordinary clean restart rejected: %v", err)
	}
	if err := runtime.Prepare(ctx, "tenant-test", "action-test"); err != nil {
		t.Fatalf("in-flight mark failed: %v", err)
	}
	if err := runtime.Check(ctx); err != nil {
		t.Fatalf("current worker rejected its own in-flight mark: %v", err)
	}
	if err := (&recoverywitness.Witness{Pool: db.Pool, Path: w.Path}).Check(ctx); err == nil {
		t.Fatal("new worker resumed from a backup taken during an external call")
	}
	// A manual restore of the earlier snapshot brings back its old SQL gate.
	// The independent file must still stop the next claim and HTTP request.
	if _, err := mig.Exec(ctx, `UPDATE restore_witness_state SET effect_sequence=0 WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Check(ctx); err == nil {
		t.Fatal("older restored database reused outbound clearance")
	}
	if err := runtime.Prepare(ctx, "tenant-test", "action-test-2"); err == nil {
		t.Fatal("older restored database prepared another external effect")
	}
	if err := w.Bind(ctx, "Unsafe clearance while gate is open", true); err == nil {
		t.Fatal("rebind accepted without restore recovery")
	}
	if _, err := mig.Exec(ctx, `UPDATE restore_safety_gate SET outbound_enabled=false,recovery_started_at=now() WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	if err := w.Bind(ctx, "Reviewed synthetic old snapshot", true); err != nil {
		t.Fatalf("reviewed rebind failed: %v", err)
	}
	if err := runtime.Check(ctx); err == nil {
		t.Fatal("SQL recovery gate did not remain closed after rebind")
	}
	if _, err := mig.Exec(ctx, `UPDATE restore_safety_gate SET outbound_enabled=true WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	// Recovery requires a fresh worker process; the old process must not reuse
	// in-memory knowledge of an effect interrupted by the restore.
	if err := (&recoverywitness.Witness{Pool: db.Pool, Path: w.Path}).Check(ctx); err != nil {
		t.Fatalf("reviewed clearance rejected: %v", err)
	}
}
