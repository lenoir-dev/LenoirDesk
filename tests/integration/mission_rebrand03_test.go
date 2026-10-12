//go:build integration

package integration

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stripe/stripe-go/v86"
)

func TestMission03AutomationReviewAndLocalizedRunHistory(t *testing.T) {
	world := newWorld()
	f, cfg := seedBusiness(t, world)
	app := newTestApp(t, newFakeOIDC(t))
	owner := login(t, app, "mission03-owner-"+randomHex(5), nil)
	ownerID, csrf := me(t, app, owner)
	if _, err := mig.Exec(context.Background(), `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')`, f.tenantID, ownerID); err != nil {
		t.Fatal(err)
	}
	base := "/ui/tenants/" + f.tenantID
	page := ownerPage(t, app, owner, base+"/billing-sync?lang=ru&theme=light", "Синхронизация подписок", "Проверить настройку", `data-theme="light"`)
	if !strings.Contains(page, `name="stripe"`) || !strings.Contains(page, `name="prices"`) || !strings.Contains(page, `name="policy"`) {
		t.Fatal("guided configuration lost a required business field")
	}
	form := url.Values{"csrf_token": {csrf}, "stripe": {cfg.StripeConnectionID}, "hubspot": {cfg.HubSpotConnectionID},
		"slack": {cfg.SlackConnectionID}, "channel": {cfg.SlackChannelID}, "prices": {world.price + "\n" + world.price}, "policy": {cfg.CreationPolicy}}
	var before, after int
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM automation_revisions WHERE tenant_id=$1`, f.tenantID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	invalid := postUIForm(t, app, owner, base+"/billing-sync/preview?lang=ru", form)
	invalidPage, _ := io.ReadAll(invalid.Body)
	_ = invalid.Body.Close()
	if invalid.StatusCode != http.StatusBadRequest || !strings.Contains(string(invalidPage), "Удалите повторяющиеся Price ID") ||
		!strings.Contains(string(invalidPage), world.price+"\n"+world.price) || !strings.Contains(string(invalidPage), `aria-invalid="true"`) {
		t.Fatalf("duplicate Price validation or draft preservation failed: %d", invalid.StatusCode)
	}
	form.Set("prices", "  "+world.price+" \n")
	preview := postUIForm(t, app, owner, base+"/billing-sync/preview?lang=ru", form)
	previewPage, _ := io.ReadAll(preview.Body)
	_ = preview.Body.Close()
	if preview.StatusCode != http.StatusOK || !strings.Contains(string(previewPage), "Проверьте автоматизацию") ||
		!strings.Contains(string(previewPage), f.account+" · симуляция") || !strings.Contains(string(previewPage), "Тестовый режим") ||
		!strings.Contains(string(previewPage), world.price) {
		t.Fatalf("review omitted real selected values or locale: %d", preview.StatusCode)
	}
	if err := mig.QueryRow(context.Background(), `SELECT count(*) FROM automation_revisions WHERE tenant_id=$1`, f.tenantID).Scan(&after); err != nil || before != after {
		t.Fatal("review unexpectedly activated a revision")
	}
	viewer := login(t, app, "mission03-viewer-"+randomHex(5), nil)
	viewerID, viewerCSRF := me(t, app, viewer)
	grant := request(t, app.client, http.MethodPost, app.server.URL+"/v1/tenants/"+f.tenantID+"/members", owner, csrf,
		map[string]string{"user_id": viewerID, "role": "viewer"})
	closeResponse(t, grant, http.StatusNoContent)
	page = ownerPage(t, app, viewer, base+"/billing-sync?lang=ru", "Наблюдатель может только просматривать")
	if strings.Contains(page, `action="`+base+`/billing-sync/preview"`) {
		t.Fatal("viewer received revision edit form")
	}
	viewerForm := url.Values{"csrf_token": {viewerCSRF}, "stripe": {cfg.StripeConnectionID}, "hubspot": {cfg.HubSpotConnectionID},
		"slack": {cfg.SlackConnectionID}, "channel": {cfg.SlackChannelID}, "prices": {world.price}, "policy": {cfg.CreationPolicy}}
	denied := postUIForm(t, app, viewer, base+"/billing-sync/preview", viewerForm)
	closeResponse(t, denied, http.StatusForbidden)

	_, err := mig.Exec(context.Background(), `WITH events AS (
 INSERT INTO webhook_events(tenant_id,connection_id,provider,provider_account_id,mode,event_id,event_type,
  event_api_version,raw_payload,classification,automation_revision_id,correlation_id)
 SELECT $1,$2,'stripe',$3,'test','evt_m03_'||g.i,'invoice.paid',$4,
  '{}'::bytea,'supported',$5,'mission03 synthetic fixture' FROM generate_series(1,27) AS g(i)
 RETURNING tenant_id,id,automation_revision_id
 ), jobs AS (
 INSERT INTO planning_jobs(tenant_id,event_row_id,automation_revision_id)
 SELECT tenant_id,id,automation_revision_id FROM events
 RETURNING tenant_id,id,automation_revision_id
 ) INSERT INTO runs(tenant_id,planning_job_id,automation_revision_id)
 SELECT tenant_id,id,automation_revision_id FROM jobs`, f.tenantID, f.connectionID, f.account, stripe.APIVersion, f.revisionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = mig.Exec(context.Background(), `UPDATE runs r SET status='unknown' FROM planning_jobs j,webhook_events e
 WHERE r.tenant_id=$1 AND j.tenant_id=r.tenant_id AND j.id=r.planning_job_id
 AND e.tenant_id=j.tenant_id AND e.id=j.event_row_id AND e.event_id='evt_m03_1'`, f.tenantID); err != nil {
		t.Fatal(err)
	}
	first := ownerPage(t, app, owner, base+"/runs?status=in_progress&lang=ru&theme=dark", "История выполнения", "Ожидают / выполняются")
	if strings.Count(first, "evt_m03_") != 25 || !strings.Contains(first, `hx-select="#runs-content"`) || !strings.Contains(first, "Более ранние запуски") {
		t.Fatal("localized cursor page or HTMX filter contract failed")
	}
	match := regexp.MustCompile(`href="[^"]*cursor=([A-Za-z0-9_-]+)"`).FindStringSubmatch(first)
	if len(match) != 2 {
		t.Fatal("cursor missing")
	}
	second := ownerPage(t, app, owner, base+"/runs?status=in_progress&cursor="+match[1], "evt_m03_")
	if strings.Count(second, "evt_m03_") != 1 || strings.Contains(second, "Older runs") {
		t.Fatal("second page repeated or lost runs")
	}
	unknown := ownerPage(t, app, owner, base+"/runs?status=unknown&lang=ru", "Результат неизвестен", "evt_m03_1")
	if strings.Count(unknown, "evt_m03_") != 1 || !strings.Contains(unknown, "Slack мог отправить") {
		t.Fatal("unknown outcome was collapsed or misclassified")
	}
	other := createWorkspace(t, app, owner, csrf, "Unrelated synthetic workspace")
	cross := request(t, app.client, http.MethodGet, app.server.URL+"/ui/tenants/"+other+"/runs", viewer, "", nil)
	closeResponse(t, cross, http.StatusForbidden)
}
