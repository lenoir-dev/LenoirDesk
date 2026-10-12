//go:build integration

package integration

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestMission04PersistedInspectorAndReconciliationHistory(t *testing.T) {
	world := newWorld()
	f, cfg := seedBusiness(t, world)
	app := newTestApp(t, newFakeOIDC(t))
	owner := login(t, app, "mission04-owner-"+randomHex(5), nil)
	ownerID, csrf := me(t, app, owner)
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	base := "/ui/tenants/" + f.tenantID
	page := ownerPage(t, app, owner, base+"/reconciliation?lang=ru", "Ещё не подтверждено", "Истории сверки пока нет")
	if strings.Contains(page, "Контрольная точка без известных проблем") {
		t.Fatal("empty coverage was presented as healthy")
	}
	startExecutor(t, world)
	eventID := deliverBusiness(t, f, world, "invoice.paid")
	waitBusinessStatus(t, f.tenantID, eventID, "succeeded")
	var runID string
	if err := mig.QueryRow(context.Background(), `SELECT r.id FROM runs r
 JOIN planning_jobs j ON j.tenant_id=r.tenant_id AND j.id=r.planning_job_id
 JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
 WHERE r.tenant_id=$1 AND e.event_id=$2`, f.tenantID, eventID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	detail := ownerPage(t, app, owner, base+"/runs/"+runID+"?lang=ru", "Путь выполнения", "Хронология", "Подтверждённое сообщение Slack")
	if !strings.Contains(detail, "Подписанный webhook") || !strings.Contains(detail, world.customer) || !strings.Contains(detail, "HubSpot") || !strings.Contains(detail, "Slack") {
		t.Fatal("persisted run context or provider stages missing")
	}
	viewer := login(t, app, "mission04-viewer-"+randomHex(5), nil)
	viewerID, _ := me(t, app, viewer)
	grant := request(t, app.client, http.MethodPost, app.server.URL+"/v1/tenants/"+f.tenantID+"/members", owner, csrf,
		map[string]string{"user_id": viewerID, "role": "viewer"})
	closeResponse(t, grant, http.StatusNoContent)
	viewerPage := ownerPage(t, app, viewer, base+"/runs/"+runID+"?lang=en", "Run details")
	if strings.Contains(viewerPage, `name="decision"`) || strings.Contains(viewerPage, "Confirm retry") {
		t.Fatal("viewer received recovery controls")
	}
	if _, err := mig.Exec(context.Background(), `INSERT INTO reconcile_checkpoints
 (tenant_id,stripe_connection_id,kind,window_start,window_end,last_complete_at,gap_code)
 VALUES($1,$2,'events',now()-interval '2 hours',now()-interval '1 hour',now(),'events_permission')`, f.tenantID, cfg.StripeConnectionID); err != nil {
		t.Fatal(err)
	}
	page = ownerPage(t, app, owner, base+"/reconciliation?lang=ru", "Требует внимания", "events_permission")
	if !strings.Contains(page, "Полнота этого периода не подтверждена") {
		t.Fatal("coverage gap explanation missing")
	}
	if _, err := mig.Exec(context.Background(), `INSERT INTO reconcile_requests
 (tenant_id,stripe_connection_id,actor_user_id,from_at,to_at,max_events,reason,status,created_at,finished_at)
 SELECT $1,$2,$3,now()-interval '3 hours',now()-interval '2 hours',10,
 'Synthetic bounded history review', 'completed',now()-make_interval(mins=>g.i),now()
 FROM generate_series(1,22) AS g(i)`, f.tenantID, cfg.StripeConnectionID, ownerID); err != nil {
		t.Fatal(err)
	}
	first := ownerPage(t, app, owner, base+"/reconciliation?lang=en", "Historical requests", "Older requests")
	if strings.Count(first, `class="request-row"`) != 20 {
		t.Fatal("first historical request page is not bounded to 20")
	}
	cursor := regexp.MustCompile(`history_cursor=([A-Za-z0-9_-]+)`).FindStringSubmatch(first)
	if len(cursor) != 2 {
		t.Fatal("historical request cursor missing")
	}
	second := ownerPage(t, app, owner, base+"/reconciliation?history_cursor="+cursor[1]+"&lang=en", "Historical requests")
	if strings.Count(second, `class="request-row"`) != 2 || strings.Contains(second, "Older requests") {
		t.Fatal("historical request pagination repeated or lost rows")
	}
	other := createWorkspace(t, app, owner, csrf, "Mission04 unrelated tenant")
	cross := request(t, app.client, http.MethodGet, app.server.URL+"/ui/tenants/"+other+"/reconciliation", viewer, "", nil)
	closeResponse(t, cross, http.StatusForbidden)
}
