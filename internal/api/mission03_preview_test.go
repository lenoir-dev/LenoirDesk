//go:build mission03preview

package api

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/identity"
)

// TestMission03Preview is a local-only rendering harness. It uses no database,
// identity provider or network provider and writes no business state. Run with
// go test -tags mission03preview -run TestMission03Preview -timeout=0 -v ./internal/api.
func TestMission03Preview(t *testing.T) {
	if os.Getenv("RELAYDESK_MISSION03_PREVIEW") != "1" {
		t.Skip("set RELAYDESK_MISSION03_PREVIEW=1 to start the local view harness")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	stamp := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	connections := []identity.Connection{
		{ID: "11111111-1111-4111-8111-111111111111", Provider: "stripe", ProviderAccountID: "acct_synthetic_test", Environment: "simulation", Mode: "test", Status: "active", HealthStatus: "verified"},
		{ID: "22222222-2222-4222-8222-222222222222", Provider: "hubspot", ProviderAccountID: "246800001", Environment: "simulation", Mode: "test", Status: "active", HealthStatus: "verified", AuthState: "ready"},
		{ID: "33333333-3333-4333-8333-333333333333", Provider: "slack", ProviderAccountID: "TDEMO1234", Environment: "simulation", Mode: "test", Status: "active", HealthStatus: "verified", AuthState: "ready"},
	}
	for i := 0; i < 12; i++ {
		connections = append(connections, identity.Connection{ID: fmt.Sprintf("%08d-6666-4666-8666-%012d", i+1, i+1),
			Provider: "stripe", ProviderAccountID: fmt.Sprintf("acct_synthetic_alternate_long_account_%02d", i+1),
			Environment: "simulation", Mode: "test", Status: "active", HealthStatus: "verified"})
	}
	config := business.Config{Template: business.Template, StripeConnectionID: connections[0].ID,
		HubSpotConnectionID: connections[1].ID, SlackConnectionID: connections[2].ID,
		TrackedPriceIDs: []string{"price_synthetic_basic", "price_synthetic_team"}, SlackChannelID: "CDEMO1234",
		CreationPolicy: "explicit_only", ActivationTime: stamp}
	revisions := []identity.BillingRevision{{ID: "44444444-4444-4444-8444-444444444444", StripeID: connections[0].ID,
		Revision: 2, Status: "active", ActivatedAt: stamp, Config: config},
		{ID: "55555555-5555-4555-8555-555555555555", StripeID: connections[0].ID,
			Revision: 1, Status: "inactive", ActivatedAt: stamp.Add(-24 * time.Hour), Config: config}}
	statuses := []string{"succeeded", "pending", "running", "retry_wait", "blocked_config", "needs_attention", "unknown", "dead"}
	allRuns := make([]identity.Run, 31)
	counts := map[string]int64{}
	for i := range allRuns {
		status := statuses[i%len(statuses)]
		counts[status]++
		step := "crm_projection"
		if i%3 == 0 {
			step = "slack_notification"
		}
		allRuns[i] = identity.Run{ID: fmt.Sprintf("%08d-0000-4000-8000-%012d", i+1, i+1), EventID: fmt.Sprintf("evt_synthetic_%03d", i+1),
			EventType: "invoice.paid", Status: status, CustomerID: fmt.Sprintf("cus_demo_%03d", i+1), InvoiceID: fmt.Sprintf("in_demo_%03d", i+1),
			CreatedAt: stamp.Add(-time.Duration(i) * time.Hour), Attempts: i % 4, Step: step}
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		v := uiView{Tenant: "synthetic", Lang: lang, Theme: theme, Workspace: identity.Workspace{Name: "Synthetic studio", Role: "owner"},
			User: identity.Principal{Email: "operator@example.invalid"}, CSRF: "synthetic-only", CanOperate: true, IsOwner: true,
			EnglishURL: r.URL.Path + "?lang=en&theme=" + theme, RussianURL: r.URL.Path + "?lang=ru&theme=" + theme,
			LightURL: r.URL.Path + "?lang=" + lang + "&theme=light", DarkURL: r.URL.Path + "?lang=" + lang + "&theme=dark",
			SystemURL: r.URL.Path + "?lang=" + lang + "&theme=dark", Connections: connections,
			ManagedProperties: []string{"relaydesk_customer_key", "relaydesk_subscription_status", "relaydesk_payment_state"},
			RunCounts:         counts, RunTotal: 31, Filters: runFilters, Filter: r.URL.Query().Get("status")}
		page := "automation"
		if r.URL.Path == "/ui/tenants/synthetic/runs" {
			page = "runs"
			v.Screen = "Runs"
			if r.URL.Query().Get("scenario") == "empty" {
				v.RunCounts, v.RunTotal = map[string]int64{}, 0
			} else {
				for _, run := range allRuns {
					if v.Filter == "" || v.Filter == run.Status || v.Filter == "in_progress" && (run.Status == "pending" || run.Status == "running") ||
						v.Filter == "blocked" && (run.Status == "blocked_auth" || run.Status == "blocked_config") {
						v.Runs = append(v.Runs, run)
					}
				}
				if len(v.Runs) > 25 {
					if r.URL.Query().Get("cursor") == "page2" {
						v.Runs = v.Runs[25:]
					} else {
						v.Runs = v.Runs[:25]
						v.NextCursor = "page2"
					}
				}
			}
		} else {
			v.Screen = "Automation"
			v.NextRevision = 1
			v.AutomationDraft.Policy = "explicit_only"
			if r.URL.Query().Get("scenario") != "empty" {
				v.Revisions, v.NextRevision = append([]identity.BillingRevision(nil), revisions...), 3
				v.ActiveRevision = &v.Revisions[0]
				if r.URL.Query().Get("scenario") == "paused" {
					v.ActiveRevision.Paused = true
				}
				v.AutomationDraft = automationDraft{Stripe: config.StripeConnectionID, HubSpot: config.HubSpotConnectionID,
					Slack: config.SlackConnectionID, Prices: "price_synthetic_basic\nprice_synthetic_team", Channel: config.SlackChannelID, Policy: config.CreationPolicy}
			} else {
				v.Connections = nil
			}
			if r.URL.Query().Get("scenario") == "validation" {
				v.Error = "automation_invalid_form"
				v.AutomationDraft.Prices = "price_synthetic_basic\nprice_synthetic_basic"
				v.FieldErrors = map[string]string{"prices": "automation_prices_duplicate_error"}
			}
			if r.Method == http.MethodPost && r.URL.Path == "/ui/tenants/synthetic/billing-sync/preview" {
				if err := r.ParseForm(); err != nil {
					http.Error(w, "invalid form", 400)
					return
				}
				cfg := billingForm(r)
				if cfg.Validate() != nil || len(automationFieldErrors(cfg)) != 0 {
					v.Error = "automation_invalid_form"
					v.FieldErrors = automationFieldErrors(cfg)
					v.AutomationDraft = automationDraft{Stripe: r.FormValue("stripe"), HubSpot: r.FormValue("hubspot"), Slack: r.FormValue("slack"), Prices: r.FormValue("prices"), Channel: r.FormValue("channel"), Policy: r.FormValue("policy")}
					w.WriteHeader(400)
				} else {
					page = "automation_preview"
					v.Detail.Config = cfg
				}
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := uiTemplates.ExecuteTemplate(w, page, v); err != nil {
			t.Logf("preview render: %v", err)
		}
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	t.Logf("synthetic preview: http://%s/ui/tenants/synthetic/billing-sync?scenario=active", listener.Addr())
	if err := server.Serve(listener); err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
