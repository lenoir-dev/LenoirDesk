//go:build integration

package integration

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMission07ReconciliationScreenAndRBAC(t *testing.T) {
	app := newTestApp(t, newFakeOIDC(t))
	owner := login(t, app, "mission07-owner-"+randomHex(4), nil)
	_, ownerCSRF := me(t, app, owner)
	tenant := createWorkspace(t, app, owner, ownerCSRF, "Mission 07 operations")
	viewer := login(t, app, "mission07-viewer-"+randomHex(4), nil)
	viewerID, viewerCSRF := me(t, app, viewer)
	closeResponse(t, request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+tenant+"/members",
		owner, ownerCSRF, map[string]string{"user_id": viewerID, "role": "viewer"}), 204)
	for _, cookie := range []*http.Cookie{owner, viewer} {
		closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+tenant+"/reconciliation", cookie, "", nil), 200)
		resp := request(t, app.client, "GET", app.server.URL+"/ui/tenants/"+tenant+"/reconciliation", cookie, "", nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), "Coverage checkpoints") || strings.Contains(string(body), "whsec_") {
			t.Fatalf("screen status=%d or unsafe content", resp.StatusCode)
		}
	}
	input := map[string]any{"connection_id": uuid.NewString(),
		"from_at": time.Now().Add(-time.Hour).UTC(), "to_at": time.Now().Add(-5 * time.Minute).UTC(),
		"max_events": 10, "reason": "Viewer cannot request scans"}
	closeResponse(t, request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+tenant+"/reconciliations",
		viewer, viewerCSRF, input), 403)
	closeResponse(t, request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+tenant+"/reconciliations",
		owner, ownerCSRF, input), 400)
	var stripeConnection string
	if err := mig.QueryRow(context.Background(), `INSERT INTO connections(tenant_id,provider,provider_account_id,
	 mode,status,webhook_ref_hash,health_status,execution_environment)
	 VALUES($1,'stripe',$2,'test','active',decode($3,'hex'),'verified','simulation') RETURNING id`,
		tenant, "acct_"+randomHex(8), randomHex(32)).Scan(&stripeConnection); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Exec(context.Background(), `INSERT INTO automation_revisions(tenant_id,connection_id,revision,status,template_key)
	 VALUES($1,$2,1,'active','subscription_billing_sync.v1')`, tenant, stripeConnection); err != nil {
		t.Fatal(err)
	}
	input["connection_id"] = stripeConnection
	created := request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+tenant+"/reconciliations",
		owner, ownerCSRF, input)
	closeResponse(t, created, 201)
	var requests, audit int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM reconcile_requests WHERE tenant_id=$1 AND stripe_connection_id=$2`,
		tenant, stripeConnection).Scan(&requests); err != nil || requests != 1 {
		t.Fatalf("historical request count=%d err=%v", requests, err)
	}
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND operation='reconciliation.request'`,
		tenant).Scan(&audit); err != nil || audit != 1 {
		t.Fatalf("historical request audit=%d err=%v", audit, err)
	}
	other := createWorkspace(t, app, owner, ownerCSRF, "Different tenant")
	closeResponse(t, request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+other+"/reconciliation", viewer, "", nil), 403)
}
