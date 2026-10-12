//go:build mission04preview

package api

import (
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/identity"
)

// TestMission04Preview is an opt-in, local-only visual fixture. Every status
// and field models a persisted read-model value; no database or provider is
// contacted, and no POST is accepted. Integration tests verify the real reads.
func TestMission04Preview(t *testing.T) {
	if os.Getenv("RELAYDESK_MISSION04_PREVIEW") != "1" {
		t.Skip("set RELAYDESK_MISSION04_PREVIEW=1 for synthetic local preview")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "synthetic preview is read-only", http.StatusMethodNotAllowed)
			return
		}
		if len(r.URL.Path) >= 8 && r.URL.Path[:8] == "/assets/" {
			serveUIAsset(w, r)
			return
		}
		lang, theme := r.URL.Query().Get("lang"), r.URL.Query().Get("theme")
		if lang != "ru" {
			lang = "en"
		}
		if theme != "light" {
			theme = "dark"
		}
		v := uiView{Tenant: "synthetic", Lang: lang, Theme: theme, CSRF: "synthetic-only", CanOperate: true,
			Workspace: identity.Workspace{Name: "Synthetic studio", Role: "owner"}, User: identity.Principal{Email: "operator@example.invalid"},
			EnglishURL: r.URL.Path + "?lang=en&theme=" + theme, RussianURL: r.URL.Path + "?lang=ru&theme=" + theme,
			LightURL: r.URL.Path + "?lang=" + lang + "&theme=light", DarkURL: r.URL.Path + "?lang=" + lang + "&theme=dark",
			SystemURL: r.URL.Path + "?lang=" + lang + "&theme=dark"}
		scenario := r.URL.Query().Get("scenario")
		page := "run_detail"
		if r.URL.Path == "/ui/tenants/synthetic/reconciliation" {
			page = "reconciliation"
			v.Screen = "Reconciliation"
			populateMission04ReconcileFixture(&v, scenario, now)
		} else {
			v.Screen = "Run details"
			v.Detail = mission04RunFixture(scenario, now)
			v.RunStages, v.RunTimeline, v.CRMNoWrite = buildRunPresentation(v.Detail)
			v.HubSpotPortal = "246800001"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := uiTemplates.ExecuteTemplate(w, page, v); err != nil {
			t.Logf("preview render: %v", err)
		}
	})
	t.Logf("synthetic preview: http://%s/ui/tenants/synthetic/runs/demo?scenario=success", listener.Addr())
	if err := server.Serve(listener); err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

func mission04RunFixture(scenario string, now time.Time) identity.RunDetail {
	crmCall := identity.ProviderCall{Provider: "hubspot", Method: "PATCH", Path: "/crm/v3/objects/contacts/101", StartedAt: now.Add(24 * time.Second)}
	crm := identity.RunAction{ID: "11111111-1111-4111-8111-111111111111", Kind: "crm_projection", Status: "succeeded", Attempts: 1,
		ContactID: "101", Generation: 2, Timeline: []identity.ActionAttempt{{Number: 1, StartedAt: now.Add(15 * time.Second), Calls: []identity.ProviderCall{crmCall}}}}
	slack := identity.RunAction{ID: "22222222-2222-4222-8222-222222222222", Kind: "slack_invoice_paid", Status: "succeeded", Attempts: 1,
		DependsOn: crm.ID, SlackChannel: "CDEMO1234", SlackTS: "1760184000.123456", Generation: 2,
		Timeline: []identity.ActionAttempt{{Number: 1, StartedAt: now.Add(40 * time.Second), Calls: []identity.ProviderCall{{Provider: "slack", Method: "POST", Path: "/api/chat.postMessage", StartedAt: now.Add(42 * time.Second)}}}}}
	finished := now.Add(48 * time.Second)
	d := identity.RunDetail{ID: "33333333-3333-4333-8333-333333333333", Status: "succeeded", EventID: "evt_synthetic_paid_001",
		EventType: "invoice.paid", EventSource: "webhook", AcceptedAt: now, CreatedAt: now.Add(2 * time.Second), FinishedAt: &finished,
		RevisionID: "44444444-4444-4444-8444-444444444444", Revision: 3, StripeMode: "test", CustomerID: "cus_synthetic_001",
		InvoiceID: "in_synthetic_001", CustomerKey: "stripe:acct_synthetic:test:cus_synthetic_001",
		Config: business.Config{SlackChannelID: "CDEMO1234"}, Actions: []identity.RunAction{crm, slack}}
	switch scenario {
	case "noop":
		d.EventType, d.InvoiceID = "customer.subscription.updated", ""
		d.Actions = d.Actions[:1]
		d.Actions[0].Timeline[0].Calls[0].Method = "GET"
	case "retry":
		d.Status, d.FinishedAt = "retry_wait", nil
		d.Actions = d.Actions[:1]
		d.Actions[0].Status, d.Actions[0].ErrorClass = "retry_wait", "hubspot_timeout"
		retry := now.Add(5 * time.Minute)
		d.Actions[0].NextAttemptAt = &retry
	case "conflict":
		d.Status, d.FinishedAt = "needs_attention", nil
		d.Actions = d.Actions[:1]
		d.Actions[0].Status, d.Actions[0].ErrorClass = "needs_attention", "hubspot_identity_conflict"
	case "unknown":
		d.Status, d.FinishedAt = "unknown", nil
		d.Actions[1].Status, d.Actions[1].ErrorClass, d.Actions[1].SlackTS = "unknown", "slack_result_unknown", ""
	case "suppressed":
		d.EventSource = "event_discovery"
		d.Actions = d.Actions[:1]
	case "dead":
		d.Status = "dead"
		d.Actions = d.Actions[:1]
		d.Actions[0].Status, d.Actions[0].ErrorClass = "dead", "hubspot_timeout"
	case "confirmed":
		d.Actions[1].ConfirmationSource = "operator"
		d.Actions[1].Decisions = []identity.ActionDecision{{Decision: "confirm_unknown", Reason: "Checked exact channel and timestamp", FromStatus: "unknown", ToStatus: "succeeded", CreatedAt: now.Add(46 * time.Second)}}
		d.Audit = []identity.AuditEntry{{Operation: "business.confirm_unknown", TargetType: "business_action", TargetID: d.Actions[1].ID, Result: "success", CreatedAt: now.Add(46 * time.Second)}}
	}
	return d
}

func populateMission04ReconcileFixture(v *uiView, scenario string, now time.Time) {
	v.Connections = []identity.Connection{{ID: "11111111-1111-4111-8111-111111111111", Provider: "stripe", ProviderAccountID: "acct_synthetic_test",
		Mode: "test", Environment: "simulation", Status: "active", HealthStatus: "verified"}}
	v.ReconcileConnections = v.Connections
	v.HasReconcileConnection = true
	v.HistoricalDraft = historicalDraft{From: now.Add(-24 * time.Hour).Format("2006-01-02T15:04:05"),
		To: now.Add(-5 * time.Minute).Format("2006-01-02T15:04:05"), Max: "100"}
	v.Operational.OutboundEnabled = true
	completed := now.Add(-10 * time.Minute)
	checkpoint := identity.ReconcileCheckpoint{ConnectionID: v.Connections[0].ID, Kind: "events", WindowStart: now.Add(-25 * time.Minute),
		WindowEnd: now.Add(-12 * time.Minute), LastCompleteAt: &completed, ScannedCount: 25, DiscoveredCount: 2}
	request := identity.ReconcileRequest{ID: "55555555-5555-4555-8555-555555555555", ConnectionID: v.Connections[0].ID,
		Status: "completed", FromAt: now.Add(-3 * time.Hour), ToAt: now.Add(-2 * time.Hour), MaxEvents: 100, Scanned: 25,
		Discovered: 2, CreatedAt: now.Add(-90 * time.Minute), FinishedAt: &completed, ActorID: "66666666-6666-4666-8666-666666666666", Reason: "Synthetic bounded history review"}
	if scenario != "empty" {
		v.Operational.Checkpoints = []identity.ReconcileCheckpoint{checkpoint, {
			ConnectionID: v.Connections[0].ID, Kind: "known", WindowStart: now.Add(-25 * time.Minute),
			WindowEnd: now.Add(-12 * time.Minute), LastCompleteAt: &completed,
		}}
		v.LastCompleteAt = &completed
	}
	switch scenario {
	case "gap":
		v.Operational.Checkpoints[0].GapCode, v.CoverageGaps = "events_permission", 1
	case "auth":
		v.Connections[0].HealthStatus = "invalid"
		v.ReconcileConnections, v.HasReconcileConnection = nil, false
		v.Operational.Checkpoints[0].GapCode, v.CoverageGaps = "events_permission", 1
	case "pending":
		request.Status, request.FinishedAt = "pending", nil
		v.Operational.Requests = []identity.ReconcileRequest{request}
	case "completed":
		v.Operational.Requests = []identity.ReconcileRequest{request}
	case "failed":
		request.Status, request.GapCode = "blocked", "events_permission"
		v.Operational.Requests = []identity.ReconcileRequest{request}
	case "restore":
		v.Operational.OutboundEnabled = false
	}
	v.ReconcileState = reconciliationState(v.Operational, v.Connections)
	if scenario == "empty" {
		v.ReconcileState = "unverified"
	}
}
