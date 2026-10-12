//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stripe/stripe-go/v86"
)

func TestLenoirDeskThemeAndDashboardTenantScope(t *testing.T) {
	app := newTestApp(t, newFakeOIDC(t))
	owner := login(t, app, "design02-owner-"+randomHex(5), nil)
	ownerID, csrf := me(t, app, owner)
	first := createWorkspace(t, app, owner, csrf, "First dashboard")
	selected, page := localizedPage(t, app, "/ui/tenants/"+first+"?theme=light&lang=ru", owner)
	if !strings.Contains(page, `data-theme="light"`) || !strings.Contains(page, "Показатели рабочей области") ||
		!strings.Contains(page, "Первый запуск появится здесь") {
		t.Fatal("empty light/Russian dashboard did not render")
	}
	var theme, language *http.Cookie
	for _, c := range selected.Cookies() {
		switch c.Name {
		case "lenoirdesk_theme":
			theme = c
		case "lenoirdesk_lang":
			language = c
		}
	}
	if theme == nil || language == nil || !theme.HttpOnly || !language.HttpOnly {
		t.Fatal("presentation preferences were not persisted securely")
	}
	_, page = localizedPage(t, app, "/ui/tenants/"+first+"/connections", owner, theme, language)
	if !strings.Contains(page, `data-theme="light"`) || !strings.Contains(page, "Провайдеры не подключены") ||
		!strings.Contains(page, "Ожидаемый ID портала HubSpot") {
		t.Fatal("localized connection setup missing")
	}
	_, page = localizedPage(t, app, "/ui/tenants/"+first+"?theme=dark&lang=en", owner)
	if !strings.Contains(page, `data-theme="dark"`) || !strings.Contains(page, "Runs over time") && !strings.Contains(page, "Your first run will appear here") {
		t.Fatal("dark dashboard missing")
	}
	f1, _ := seedBusiness(t, newWorld())
	f2, _ := seedBusiness(t, newWorld())
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f1.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		f     fixture
		count int
	}{{f1, 3}, {f2, 1}} {
		_, err := mig.Exec(context.Background(), `WITH events AS (
 INSERT INTO webhook_events(tenant_id,connection_id,provider,provider_account_id,mode,event_id,event_type,
  event_api_version,raw_payload,classification,automation_revision_id,correlation_id)
 SELECT $1,$2,'stripe',$3,'test',$4||g.i,'customer.subscription.updated',$5,
  '{}'::bytea,'supported',$6,'dashboard fixture' FROM generate_series(1,$7) AS g(i)
 RETURNING tenant_id,id,automation_revision_id
 ), jobs AS (
 INSERT INTO planning_jobs(tenant_id,event_row_id,automation_revision_id)
 SELECT tenant_id,id,automation_revision_id FROM events
 RETURNING tenant_id,id,automation_revision_id
 ) INSERT INTO runs(tenant_id,planning_job_id,automation_revision_id)
 SELECT tenant_id,id,automation_revision_id FROM jobs`, fixture.f.tenantID, fixture.f.connectionID,
			fixture.f.account, "evt_design02_"+randomHex(7), stripe.APIVersion, fixture.f.revisionID, fixture.count)
		if err != nil {
			t.Fatal(err)
		}
	}
	resp := request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+f1.tenantID+"/overview", owner, "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("overview status=%d", resp.StatusCode)
	}
	var overview struct {
		RunTrend []struct {
			Count int64 `json:"count"`
		} `json:"run_trend"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&overview); err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, day := range overview.RunTrend {
		sum += day.Count
	}
	if len(overview.RunTrend) != 14 || sum != 3 {
		t.Fatalf("tenant-scoped fourteen-day trend days=%d total=%d", len(overview.RunTrend), sum)
	}
	_, page = localizedPage(t, app, "/ui/tenants/"+f1.tenantID, owner)
	if !strings.Contains(page, "3 runs in this period") || strings.Contains(page, "4 runs in this period") {
		t.Fatal("rendered chart totals differ from tenant data")
	}
	if !strings.Contains(page, `<title>`) || !strings.Contains(page, "2026") {
		t.Fatal("accessible chart detail missing")
	}
	viewer := login(t, app, "design02-viewer-"+randomHex(5), nil)
	viewerID, _ := me(t, app, viewer)
	grant := request(t, app.client, "POST", app.server.URL+"/v1/tenants/"+f1.tenantID+"/members", owner, csrf, map[string]string{"user_id": viewerID, "role": "viewer"})
	closeResponse(t, grant, http.StatusNoContent)
	_, page = localizedPage(t, app, "/ui/tenants/"+f1.tenantID+"/connections", viewer)
	if strings.Contains(page, "Confirm disconnect") || strings.Contains(page, "Expected HubSpot portal ID") {
		t.Fatal("viewer received owner controls")
	}
	forbidden := request(t, app.client, "GET", app.server.URL+"/v1/tenants/"+f2.tenantID+"/overview", viewer, "", nil)
	_, _ = io.Copy(io.Discard, forbidden.Body)
	_ = forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-tenant overview status=%d", forbidden.StatusCode)
	}
}
