package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/identity"
)

func TestMission04RunJourneyUsesPersistedActions(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	finished := now.Add(2 * time.Minute)
	detail := identity.RunDetail{ID: "run-synthetic", Status: "unknown", EventType: "invoice.paid",
		EventSource: "webhook", AcceptedAt: now, CreatedAt: now.Add(time.Second),
		Actions: []identity.RunAction{
			{Kind: "crm_projection", Status: "succeeded", Attempts: 1, Timeline: []identity.ActionAttempt{{StartedAt: now.Add(10 * time.Second),
				Calls: []identity.ProviderCall{{Provider: "hubspot", Method: "GET", Path: "/crm/v3/objects/contacts/123", StartedAt: now.Add(11 * time.Second)}}}}},
			{Kind: "slack_invoice_paid", Status: "unknown", Attempts: 1, DependsOn: "crm-action", Timeline: []identity.ActionAttempt{{StartedAt: now.Add(30 * time.Second), FinishedAt: &finished}}},
		}}
	stages, timeline, noWrite := buildRunPresentation(detail)
	if !noWrite || stages[1].Status != "succeeded" || stages[2].Status != "unknown" || stages[2].Action.DependsOn != "crm-action" {
		t.Fatalf("run journey lost persisted dependency or outcome: %+v", stages)
	}
	for i := 1; i < len(timeline); i++ {
		if timeline[i].At.Before(timeline[i-1].At) {
			t.Fatal("timeline is not chronological")
		}
	}
	detail.EventSource = "event_discovery"
	detail.Actions = detail.Actions[:1]
	stages, _, _ = buildRunPresentation(detail)
	if stages[2].Status != "suppressed" || stages[2].Action != nil {
		t.Fatal("historical Slack suppression was misrepresented")
	}
	detail.EventType = "customer.subscription.updated"
	stages, _, _ = buildRunPresentation(detail)
	if stages[2].Status != "not_applicable" {
		t.Fatal("non-invoice event incorrectly requires Slack")
	}
}

func TestMission04CoverageStateDoesNotInferSuccess(t *testing.T) {
	now := time.Now().UTC()
	const stripeID = "stripe-1"
	events := identity.ReconcileCheckpoint{ConnectionID: stripeID, Kind: "events", LastCompleteAt: &now}
	known := identity.ReconcileCheckpoint{ConnectionID: stripeID, Kind: "known", LastCompleteAt: &now}
	for _, tc := range []struct {
		name   string
		status identity.OperationalStatus
		want   string
	}{
		{"no checkpoint", identity.OperationalStatus{OutboundEnabled: true}, "unverified"},
		{"recorded but unfinished", identity.OperationalStatus{OutboundEnabled: true, Checkpoints: []identity.ReconcileCheckpoint{{ConnectionID: stripeID, Kind: "events"}}}, "unverified"},
		{"only one scan", identity.OperationalStatus{OutboundEnabled: true, Checkpoints: []identity.ReconcileCheckpoint{events}}, "unverified"},
		{"completed", identity.OperationalStatus{OutboundEnabled: true, Checkpoints: []identity.ReconcileCheckpoint{events, known}}, "healthy"},
		{"gap", identity.OperationalStatus{OutboundEnabled: true, Checkpoints: []identity.ReconcileCheckpoint{{ConnectionID: stripeID, Kind: "events", LastCompleteAt: &now, GapCode: "events_permission"}, known}}, "degraded"},
		{"restore gate", identity.OperationalStatus{OutboundEnabled: false, Checkpoints: []identity.ReconcileCheckpoint{events, known}}, "blocked"},
		{"unknown effect", identity.OperationalStatus{OutboundEnabled: true, Checkpoints: []identity.ReconcileCheckpoint{events, known}, UnknownActions: 1}, "degraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reconciliationState(tc.status, []identity.Connection{{ID: stripeID, Provider: "stripe", Status: "active", HealthStatus: "verified"}}); got != tc.want {
				t.Fatalf("state = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestMission04UTCFormAndViewerRendering(t *testing.T) {
	form := url.Values{"connection_id": {"11111111-1111-4111-8111-111111111111"}, "from_at": {"2026-10-10T12:34:56"},
		"to_at": {"2026-10-10T13:34:56"}, "max_events": {"25"}, "reason": {"Synthetic operator review"}}
	r := httptest.NewRequest(http.MethodPost, "/reconciliation/preview", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	input, err := historicalForm(r)
	if err != nil || input.FromAt.Location() != time.UTC || input.FromAt.Second() != 56 || input.MaxEvents != 25 {
		t.Fatalf("UTC datetime-local parsing changed bounds: %+v, %v", input, err)
	}
	v := uiView{Tenant: "synthetic", Lang: "ru", Theme: "dark", Screen: "Run details",
		Workspace: identity.Workspace{Name: "Synthetic", Role: "viewer"},
		User:      identity.Principal{Email: "viewer@example.invalid"},
		Detail: identity.RunDetail{ID: "run-synthetic", EventType: "invoice.paid", EventID: "<script>", EventSource: "event_discovery",
			AcceptedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(), Actions: []identity.RunAction{{ID: "action-synthetic", Kind: "slack_invoice_paid", Status: "unknown"}}}}
	v.RunStages, v.RunTimeline, _ = buildRunPresentation(v.Detail)
	var rendered bytes.Buffer
	if err := uiTemplates.ExecuteTemplate(&rendered, "run_detail", v); err != nil {
		t.Fatal(err)
	}
	page := rendered.String()
	for _, marker := range []string{"Путь выполнения", "Результат неизвестен", "Сохранённая последовательность"} {
		if !strings.Contains(page, marker) {
			t.Fatalf("Run Details missing %q", marker)
		}
	}
	if strings.Contains(page, "name=\"decision\"") || strings.Contains(page, "<script>") {
		t.Fatal("viewer received recovery form or unescaped event")
	}
}
