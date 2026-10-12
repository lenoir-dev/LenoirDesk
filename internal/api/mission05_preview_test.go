//go:build mission05preview

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

// TestMission05Preview serves current templates with synthetic data for visual
// inspection. It accepts GET only and never opens a database or provider client.
func TestMission05Preview(t *testing.T) {
	if os.Getenv("RELAYDESK_MISSION05_PREVIEW") != "1" {
		t.Skip("set RELAYDESK_MISSION05_PREVIEW=1 for local visual preview")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	stamp := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "visual preview is read-only", http.StatusMethodNotAllowed)
			return
		}
		if len(r.URL.Path) >= 8 && r.URL.Path[:8] == "/assets/" {
			serveUIAsset(w, r)
			return
		}
		if r.URL.Path == "/error" {
			if r.URL.Query().Get("scenario") == "forbidden" {
				writeUIError(w, r, http.StatusForbidden, "forbidden")
			} else {
				writeUIError(w, r, http.StatusUnauthorized, "unauthenticated")
			}
			return
		}
		lang, theme := r.URL.Query().Get("lang"), r.URL.Query().Get("theme")
		if lang != "ru" {
			lang = "en"
		}
		if theme != "light" && theme != "system" {
			theme = "dark"
		}
		url := r.URL.Path
		user := identity.Principal{Email: "operator@example.invalid"}
		workspace := identity.Workspace{ID: "11111111-1111-4111-8111-111111111111", Name: "Synthetic operations workspace with a long name", Role: "owner"}
		if url == "/entry" || url == "/workspaces" {
			data := pageData{Lang: lang, Theme: theme, CSRF: "synthetic-only",
				EnglishURL: url + "?lang=en&theme=" + theme, RussianURL: url + "?lang=ru&theme=" + theme,
				LightURL: url + "?lang=" + lang + "&theme=light", DarkURL: url + "?lang=" + lang + "&theme=dark",
				SystemURL: url + "?lang=" + lang + "&theme=system"}
			if url == "/workspaces" {
				data.User = &user
				data.Workspaces = []identity.Workspace{workspace, {ID: "22222222-2222-4222-8222-222222222222", Name: "Second synthetic workspace", Role: "viewer"}}
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := page.Execute(w, data); err != nil {
				t.Logf("page preview: %v", err)
			}
			return
		}
		v := uiView{Tenant: "synthetic", Lang: lang, Theme: theme, CSRF: "synthetic-only", CanOperate: true, IsOwner: true,
			Workspace: workspace, User: user, EnglishURL: url + "?lang=en&theme=" + theme,
			RussianURL: url + "?lang=ru&theme=" + theme, LightURL: url + "?lang=" + lang + "&theme=light",
			DarkURL: url + "?lang=" + lang + "&theme=dark", SystemURL: url + "?lang=" + lang + "&theme=system"}
		connections := []identity.Connection{
			{ID: "33333333-3333-4333-8333-333333333333", Provider: "stripe", ProviderAccountID: "acct_synthetic_test", Environment: "simulation", Mode: "test", Status: "active", HealthStatus: "verified", VerifiedAt: &stamp},
			{ID: "44444444-4444-4444-8444-444444444444", Provider: "hubspot", ProviderAccountID: "246800001", Environment: "simulation", Mode: "test", Status: "active", HealthStatus: "verified", AuthState: "ready", VerifiedAt: &stamp},
			{ID: "55555555-5555-4555-8555-555555555555", Provider: "slack", ProviderAccountID: "TDEMO1234", Environment: "simulation", Mode: "test", Status: "active", HealthStatus: "verified", AuthState: "ready", VerifiedAt: &stamp},
		}
		if r.URL.Query().Get("scenario") == "attention" {
			connections[1].HealthStatus, connections[1].AuthState = "invalid", "blocked_auth"
		}
		if r.URL.Query().Get("scenario") == "empty" {
			connections = nil
		}
		if len(connections) == 0 && (url == "/mapping" || url == "/reconciliation-preview") {
			http.NotFound(w, r)
			return
		}
		v.Connections = connections
		v.ProviderCards = buildProviderCards(connections)
		v.Overview = identity.Overview{RunCounts: map[string]int64{}, RunTrend: []identity.RunTrendDay{}}
		v.Steps = []onboardingStep{{Label: "Connect Stripe Test Mode", State: "waiting", Detail: tr(lang, "Verify the provider account and permissions.")},
			{Label: "Connect HubSpot", State: "waiting", Detail: tr(lang, "Verify the provider account and permissions.")},
			{Label: "Connect Slack", State: "waiting", Detail: tr(lang, "Verify the provider account and permissions.")},
			{Label: "Activate billing automation", State: "waiting", Detail: tr(lang, "Select verified connections and review the first revision.")},
			{Label: "Observe first event", State: "waiting", Detail: tr(lang, "Send a supported Stripe Test Mode event after activation.")}}
		v.ShowOnboarding = len(connections) == 0
		v.OverallState = "setup"
		if len(connections) > 0 {
			v.Overview.RunCounts["succeeded"] = 18
			v.Overview.ActiveRevisions = 1
			v.Overview.LastSuccessful = &stamp
			v.OverallState = "healthy"
			v.CompleteSteps = len(v.Steps)
			for i := range v.Steps {
				v.Steps[i].State = "complete"
			}
			v.Runs = []identity.Run{{ID: "66666666-6666-4666-8666-666666666666", EventType: "invoice.paid", Status: "succeeded", CreatedAt: stamp}}
			v.TrendBars, v.TrendTotal, v.TrendMax = makeTrendBars([]identity.RunTrendDay{{Date: "2026-10-10", Count: 8}, {Date: "2026-10-11", Count: 10}})
			v.TrendStart, v.TrendEnd = "2026-10-10", "2026-10-11"
		}
		name, screen := "overview", "Overview"
		switch url {
		case "/connections":
			name, screen = "connections", "Connections"
		case "/mapping":
			name, screen = "mapping", "Run details"
			v.Detail = identity.RunDetail{ID: "66666666-6666-4666-8666-666666666666", CustomerID: "cus_synthetic_001",
				CustomerKey: "stripe:acct_synthetic_test:test:cus_synthetic_001",
				Config:      business.Config{HubSpotConnectionID: connections[1].ID}}
			v.HasPreview = true
			v.Preview = identity.ContactPreview{ID: "101", CustomerKey: v.Detail.CustomerKey, IdentityStatus: "matching"}
			v.CandidateID = "101"
		case "/reconciliation-preview":
			name, screen = "reconciliation_preview", "Reconciliation"
			v.Historical = identity.HistoricalReconcileInput{ConnectionID: connections[0].ID,
				FromAt: stamp.Add(-24 * time.Hour), ToAt: stamp.Add(-5 * time.Minute), MaxEvents: 100,
				Reason: "Synthetic bounded review for visual quality"}
			v.EstimatedPages = 4
		}
		v.Screen = screen
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := uiTemplates.ExecuteTemplate(w, name, v); err != nil {
			t.Logf("UI preview: %v", err)
		}
	})
	t.Logf("synthetic preview: http://%s/entry", listener.Addr())
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	if err := server.Serve(listener); err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
