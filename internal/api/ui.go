package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/identity"
	"github.com/lenoir-dev/LenoirDesk/internal/ingress"
)

func (a *application) uiBase(w http.ResponseWriter, r *http.Request, screen string) (uiView, bool) {
	tenant := chi.URLParam(r, "tenant")
	p := principal(r)
	workspace, err := a.identity.GetWorkspace(r.Context(), p, tenant)
	if err != nil {
		appError(w, r, err)
		return uiView{}, false
	}
	return uiView{Tenant: tenant, Workspace: workspace, User: p, CSRF: a.identity.CSRF(p), Screen: screen,
		Lang: a.pageLanguage(w, r), Theme: a.pageTheme(w, r), EnglishURL: languageURL(r, "en"), RussianURL: languageURL(r, "ru"),
		LightURL: themeURL(r, "light"), DarkURL: themeURL(r, "dark"), SystemURL: themeURL(r, "system"),
		CanOperate: workspace.Role == "owner" || workspace.Role == "operator", IsOwner: workspace.Role == "owner",
		DemoMode: a.cfg.DemoMode, AllowConnectionChanges: workspace.Role == "owner" && !a.cfg.DemoMode,
		Notice: safeNotice(r.URL.Query().Get("notice"))}, true
}

func safeNotice(code string) string {
	switch code {
	case "connected":
		return "Connection verified and stored."
	case "disconnected":
		return "Connection disconnected; its stored credentials were revoked."
	case "activated":
		return "Automation revision activated for new events."
	case "paused":
		return "Outbound effects paused. Accepted work remains durable."
	case "resumed":
		return "Outbound effects resumed."
	case "recovered":
		return "Operator decision recorded. Refresh to see the current action state."
	case "linked":
		return "Contact mapping saved. Retry only the eligible CRM action from this run."
	case "reconciliation_requested":
		return "Bounded historical reconciliation requested. Follow its persisted status below."
	default:
		return ""
	}
}

func (a *application) renderUI(w http.ResponseWriter, status int, page string, data uiView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := uiTemplates.ExecuteTemplate(w, page, data); err != nil {
		a.log.Error("UI template failed", "page", page)
	}
}

func (a *application) overviewScreen(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Overview")
	if !ok {
		return
	}
	var err error
	v.Overview, err = a.identity.WorkspaceOverview(r.Context(), principal(r), v.Tenant)
	if err != nil {
		appError(w, r, err)
		return
	}
	v.Operational, err = a.identity.ReconciliationStatus(r.Context(), principal(r), v.Tenant)
	if err != nil {
		appError(w, r, err)
		return
	}
	v.Connections, _, err = a.identity.ListConnections(r.Context(), principal(r), v.Tenant, 100, "")
	if err != nil {
		appError(w, r, err)
		return
	}
	v.Revisions, err = a.identity.ListBillingRevisions(r.Context(), principal(r), v.Tenant, 20)
	if err != nil {
		appError(w, r, err)
		return
	}
	v.Runs, _, err = a.identity.ListRunsFiltered(r.Context(), principal(r), v.Tenant, 5, "", "")
	if err != nil {
		appError(w, r, err)
		return
	}
	for _, spec := range []struct{ provider, label string }{{"stripe", "Connect Stripe Test Mode"}, {"hubspot", "Connect HubSpot"}, {"slack", "Connect Slack"}} {
		state, detail := "waiting", "Verify the provider account and permissions."
		for _, c := range v.Connections {
			if c.Provider != spec.provider || c.Status != "active" {
				continue
			}
			if c.HealthStatus == "verified" && (c.AuthState == "" || c.AuthState == "ready") {
				state, detail = "complete", fmt.Sprintf(tr(v.Lang, "verified_connection_fmt"), tr(v.Lang, c.Environment), c.ProviderAccountID)
				break
			}
			state, detail = "blocked", tr(v.Lang, "Connection needs attention:")+" "+tr(v.Lang, statusLabel(c.HealthStatus))
		}
		v.Steps = append(v.Steps, onboardingStep{Label: spec.label, State: state, Detail: detail, Link: "/ui/tenants/" + v.Tenant + "/connections"})
	}
	state, detail := "waiting", "Select verified connections and review the first revision."
	for _, rev := range v.Revisions {
		if rev.Status == "active" {
			state, detail = "complete", tr(v.Lang, "Active revision")+" #"+strconvI(rev.Revision)
			if rev.Paused {
				state, detail = "blocked", "Active revision is paused"
			}
			break
		}
	}
	v.Steps = append(v.Steps, onboardingStep{Label: "Activate billing automation", State: state, Detail: detail, Link: "/ui/tenants/" + v.Tenant + "/billing-sync"})
	state, detail = "waiting", "Send a supported Stripe Test Mode event after activation."
	if len(v.Runs) > 0 {
		state, detail = "complete", "At least one supported event has a persisted run."
	}
	v.Steps = append(v.Steps, onboardingStep{Label: "Observe first event", State: state, Detail: detail, Link: "/ui/tenants/" + v.Tenant + "/runs"})
	v.ProviderCards = buildProviderCards(v.Connections)
	for _, step := range v.Steps {
		if step.State == "complete" {
			v.CompleteSteps++
		}
	}
	v.ShowOnboarding = len(v.Runs) == 0 || v.Overview.ActiveRevisions == 0
	v.AttentionCount = v.Overview.RunCounts["needs_attention"] + v.Overview.RunCounts["unknown"] +
		v.Overview.RunCounts["dead"] + v.Overview.RunCounts["blocked_auth"] + v.Overview.RunCounts["blocked_config"]
	v.OverallState = "setup"
	if v.AttentionCount > 0 || v.Operational.OldestOutboxSeconds > 300 {
		v.OverallState = "attention"
	} else if v.CompleteSteps == len(v.Steps) {
		v.OverallState = "healthy"
	}
	for _, checkpoint := range v.Operational.Checkpoints {
		if checkpoint.GapCode != "" {
			v.OverallState = "attention"
		}
	}
	for _, card := range v.ProviderCards {
		if card.State == "attention" {
			v.OverallState = "attention"
		}
	}
	v.TrendBars, v.TrendTotal, v.TrendMax = makeTrendBars(v.Overview.RunTrend)
	if len(v.Overview.RunTrend) > 0 {
		v.TrendStart = v.Overview.RunTrend[0].Date
		v.TrendEnd = v.Overview.RunTrend[len(v.Overview.RunTrend)-1].Date
	}
	a.renderUI(w, 200, "overview", v)
}

func strconvI(n int) string { return fmt.Sprintf("%d", n) }

func makeTrendBars(days []identity.RunTrendDay) ([]trendBar, int64, int64) {
	var total, peak int64
	for _, day := range days {
		total += day.Count
		if day.Count > peak {
			peak = day.Count
		}
	}
	bars := make([]trendBar, 0, len(days))
	for i, day := range days {
		height := 0
		if peak > 0 && day.Count > 0 {
			height = int(day.Count * 132 / peak)
			if height < 5 {
				height = 5
			}
		}
		bars = append(bars, trendBar{Date: day.Date, Count: day.Count, X: 35 + i*45, Y: 174 - height, Height: height})
	}
	return bars, total, peak
}

func buildProviderCards(connections []identity.Connection) []providerCard {
	cards := []providerCard{{Key: "stripe", State: "disconnected"}, {Key: "hubspot", State: "disconnected"}, {Key: "slack", State: "disconnected"}}
	for _, c := range connections {
		for i := range cards {
			if cards[i].Key != c.Provider {
				continue
			}
			cards[i].Connections = append(cards[i].Connections, c)
			if c.Status == "active" {
				if c.HealthStatus == "verified" && (c.AuthState == "" || c.AuthState == "ready") {
					if cards[i].State != "attention" {
						cards[i].State = "verified"
					}
				} else {
					cards[i].State = "attention"
				}
			}
		}
	}
	return cards
}

func (a *application) connectionsScreen(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Connections")
	if !ok {
		return
	}
	var err error
	v.Connections, _, err = a.identity.ListConnections(r.Context(), principal(r), v.Tenant, 100, "")
	if err != nil {
		appError(w, r, err)
		return
	}
	v.ProviderCards = buildProviderCards(v.Connections)
	a.renderUI(w, 200, "connections", v)
}

func (a *application) stripeSetupScreen(w http.ResponseWriter, r *http.Request) {
	if a.cfg.DemoMode {
		apiError(w, r, http.StatusForbidden, "demo_provider_changes_disabled")
		return
	}
	v, ok := a.uiBase(w, r, "Connections")
	if !ok {
		return
	}
	if !v.IsOwner {
		apiError(w, r, 403, "forbidden")
		return
	}
	if id := r.URL.Query().Get("reconnect"); id != "" {
		c, err := a.identity.GetConnection(r.Context(), principal(r), v.Tenant, id)
		if err != nil || c.Provider != "stripe" || c.Mode != "test" {
			apiError(w, r, 404, "not_found")
			return
		}
		v.CandidateID = id
		v.Preview.CustomerKey = c.ProviderAccountID
	}
	ref, err := identity.NewStripeWebhookRef()
	if err != nil {
		apiError(w, r, 503, "setup_unavailable")
		return
	}
	v.StripeRef = ref
	v.WebhookPath = "/webhooks/stripe/" + ref
	a.renderUI(w, 200, "stripe_setup", v)
}

func (a *application) stripeSetupSubmit(w http.ResponseWriter, r *http.Request) {
	if a.cfg.DemoMode {
		apiError(w, r, http.StatusForbidden, "demo_provider_changes_disabled")
		return
	}
	v, ok := a.uiBase(w, r, "Connections")
	if !ok {
		return
	}
	if !v.IsOwner {
		apiError(w, r, 403, "forbidden")
		return
	}
	if err := r.ParseForm(); err != nil {
		apiError(w, r, 400, "invalid_request")
		return
	}
	input := identity.StripeTestSetup{ExpectedAccountID: r.FormValue("expected_account_id"), RestrictedKey: r.FormValue("restricted_key"), WebhookSecret: r.FormValue("webhook_signing_secret"), WebhookRef: r.FormValue("webhook_ref"), ReconnectID: r.FormValue("reconnect_id")}
	_, err := a.identity.ConnectStripeTest(r.Context(), principal(r), v.Tenant, input, ingress.CorrelationID(r.Context()))
	if err != nil {
		a.log.Warn("Stripe setup rejected", "request_id", ingress.CorrelationID(r.Context()))
		v.Error = "Verification failed. Check Test Mode, account ID and four restricted read permissions. No credentials were stored."
		ref, refErr := identity.NewStripeWebhookRef()
		if refErr != nil {
			apiError(w, r, 503, "setup_unavailable")
			return
		}
		v.StripeRef = ref
		v.WebhookPath = "/webhooks/stripe/" + ref
		v.CandidateID = input.ReconnectID
		v.Preview.CustomerKey = input.ExpectedAccountID
		a.renderUI(w, 400, "stripe_setup", v)
		return
	}
	http.Redirect(w, r, "/ui/tenants/"+v.Tenant+"/connections?notice=connected", http.StatusSeeOther)
}

func (a *application) disconnectScreen(w http.ResponseWriter, r *http.Request) {
	if a.cfg.DemoMode {
		apiError(w, r, http.StatusForbidden, "demo_provider_changes_disabled")
		return
	}
	v, ok := a.uiBase(w, r, "Connections")
	if !ok {
		return
	}
	if !v.IsOwner {
		apiError(w, r, 403, "forbidden")
		return
	}
	c, err := a.identity.GetConnection(r.Context(), principal(r), v.Tenant, chi.URLParam(r, "connection"))
	if err != nil {
		appError(w, r, err)
		return
	}
	var changed bool
	if c.Provider == "stripe" {
		changed, err = a.identity.DisconnectStripeTest(r.Context(), principal(r), v.Tenant, c.ID, ingress.CorrelationID(r.Context()))
	} else {
		changed, _, err = a.identity.DisconnectProvider(r.Context(), principal(r), v.Tenant, c.ID, ingress.CorrelationID(r.Context()))
	}
	if err != nil {
		appError(w, r, err)
		return
	}
	if !changed {
		apiError(w, r, 404, "not_found")
		return
	}
	http.Redirect(w, r, "/ui/tenants/"+v.Tenant+"/connections?notice=disconnected", http.StatusSeeOther)
}

func billingForm(r *http.Request) business.Config {
	c := business.Config{Template: business.Template, StripeConnectionID: r.FormValue("stripe"), HubSpotConnectionID: r.FormValue("hubspot"), SlackConnectionID: r.FormValue("slack"), SlackChannelID: r.FormValue("channel"), CreationPolicy: r.FormValue("policy"), ActivationTime: time.Now().UTC()}
	for _, line := range strings.Split(strings.ReplaceAll(r.FormValue("prices"), "\r", ""), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		for _, id := range strings.Split(line, ",") {
			c.TrackedPriceIDs = append(c.TrackedPriceIDs, strings.TrimSpace(id))
		}
	}
	return c
}
func verifiedBillingConnections(cfg business.Config, connections []identity.Connection) bool {
	for provider, id := range map[string]string{"stripe": cfg.StripeConnectionID, "hubspot": cfg.HubSpotConnectionID, "slack": cfg.SlackConnectionID} {
		found := false
		for _, c := range connections {
			if c.ID == id && c.Provider == provider && c.Mode == "test" && c.Status == "active" && c.HealthStatus == "verified" && (c.AuthState == "" || c.AuthState == "ready") {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (a *application) automationScreen(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Automation")
	if !ok {
		return
	}
	if err := a.populateAutomation(r, &v); err != nil {
		appError(w, r, err)
		return
	}
	a.renderUI(w, 200, "automation", v)
}
func (a *application) populateAutomation(r *http.Request, v *uiView) error {
	var err error
	v.Connections, _, err = a.identity.ListConnections(r.Context(), principal(r), v.Tenant, 100, "")
	if err != nil {
		return err
	}
	v.Revisions, err = a.identity.ListBillingRevisions(r.Context(), principal(r), v.Tenant, 20)
	if err != nil {
		return err
	}
	v.ManagedProperties = []string{"relaydesk_customer_key", "relaydesk_subscription_id", "relaydesk_subscription_status", "relaydesk_payment_state", "relaydesk_latest_invoice_id", "relaydesk_cancel_at_period_end", "relaydesk_effectively_canceled", "relaydesk_projection_hash"}
	v.NextRevision = nextRevision(v.Revisions)
	for i := range v.Revisions {
		if v.Revisions[i].Status == "active" {
			v.ActiveRevision = &v.Revisions[i]
			break
		}
	}
	if v.FieldErrors == nil {
		v.AutomationDraft.Policy = "explicit_only"
		if v.ActiveRevision != nil {
			cfg := v.ActiveRevision.Config
			v.AutomationDraft = automationDraft{Stripe: cfg.StripeConnectionID, HubSpot: cfg.HubSpotConnectionID,
				Slack: cfg.SlackConnectionID, Prices: strings.Join(cfg.TrackedPriceIDs, "\n"), Channel: cfg.SlackChannelID, Policy: cfg.CreationPolicy}
		}
	}
	return nil
}
func (a *application) automationError(w http.ResponseWriter, r *http.Request, v uiView, message string) {
	v.AutomationDraft = automationDraft{Stripe: r.FormValue("stripe"), HubSpot: r.FormValue("hubspot"), Slack: r.FormValue("slack"),
		Prices: r.FormValue("prices"), Channel: r.FormValue("channel"), Policy: r.FormValue("policy")}
	v.FieldErrors = automationFieldErrors(billingForm(r))
	if err := a.populateAutomation(r, &v); err != nil {
		appError(w, r, err)
		return
	}
	if message == "automation_invalid_connections" {
		for provider, id := range map[string]string{"stripe": v.AutomationDraft.Stripe, "hubspot": v.AutomationDraft.HubSpot, "slack": v.AutomationDraft.Slack} {
			found := false
			for _, c := range v.Connections {
				if selectableConnection(c, provider, id) {
					found = true
					break
				}
			}
			if !found {
				v.FieldErrors[provider] = "automation_connection_unavailable"
			}
		}
	}
	v.Error = message
	a.renderUI(w, 400, "automation", v)
}
func (a *application) automationPreviewScreen(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Automation")
	if !ok {
		return
	}
	if !v.CanOperate {
		apiError(w, r, 403, "forbidden")
		return
	}
	if err := r.ParseForm(); err != nil {
		apiError(w, r, 400, "invalid_request")
		return
	}
	cfg := billingForm(r)
	if cfg.Validate() != nil || len(automationFieldErrors(cfg)) != 0 {
		a.automationError(w, r, v, "automation_invalid_form")
		return
	}
	connections, _, err := a.identity.ListConnections(r.Context(), principal(r), v.Tenant, 100, "")
	if err != nil {
		appError(w, r, err)
		return
	}
	if !verifiedBillingConnections(cfg, connections) {
		a.automationError(w, r, v, "automation_invalid_connections")
		return
	}
	if err := a.populateAutomation(r, &v); err != nil {
		appError(w, r, err)
		return
	}
	v.Connections = connections
	v.Detail.Config = cfg
	a.renderUI(w, 200, "automation_preview", v)
}

var runFilters = []uiFilter{{"", "All"}, {"succeeded", "Succeeded"}, {"in_progress", "Pending / running"}, {"retry_wait", "Retry waiting"}, {"blocked", "Blocked"}, {"needs_attention", "Needs attention"}, {"unknown", "Unknown"}, {"dead", "Dead / DLQ"}}

func (a *application) runsScreen(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Runs")
	if !ok {
		return
	}
	v.Filter = r.URL.Query().Get("status")
	v.Filters = runFilters
	var err error
	v.RunCounts, err = a.identity.RunStatusCounts(r.Context(), principal(r), v.Tenant)
	if err != nil {
		appError(w, r, err)
		return
	}
	for _, count := range v.RunCounts {
		v.RunTotal += count
	}
	v.Runs, v.NextCursor, err = a.identity.ListRunsFiltered(r.Context(), principal(r), v.Tenant, 25, r.URL.Query().Get("cursor"), v.Filter)
	if err != nil {
		appError(w, r, err)
		return
	}
	a.renderUI(w, 200, "runs", v)
}
func (a *application) runScreen(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Run details")
	if !ok {
		return
	}
	var err error
	v.Detail, err = a.identity.BusinessRun(r.Context(), principal(r), v.Tenant, chi.URLParam(r, "run"))
	if err != nil {
		appError(w, r, err)
		return
	}
	if v.Detail.Config.HubSpotConnectionID != "" {
		c, e := a.identity.GetConnection(r.Context(), principal(r), v.Tenant, v.Detail.Config.HubSpotConnectionID)
		if e == nil {
			v.HubSpotPortal = c.ProviderAccountID
		}
	}
	v.RunStages, v.RunTimeline, v.CRMNoWrite = buildRunPresentation(v.Detail)
	enabled, err := a.identity.OutboundExecutionEnabled(r.Context(), principal(r), v.Tenant)
	if err != nil {
		appError(w, r, err)
		return
	}
	v.RestoreBlocked = !enabled
	a.renderUI(w, 200, "run_detail", v)
}
func (a *application) mappingScreen(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Run details")
	if !ok {
		return
	}
	var err error
	v.Detail, err = a.identity.BusinessRun(r.Context(), principal(r), v.Tenant, chi.URLParam(r, "run"))
	if err != nil {
		appError(w, r, err)
		return
	}
	if v.Detail.CustomerKey == "" || v.Detail.Config.HubSpotConnectionID == "" {
		apiError(w, r, 409, "mapping_unavailable")
		return
	}
	v.Link, v.HasLink, err = a.identity.ContactLink(r.Context(), principal(r), v.Tenant, v.Detail.Config.HubSpotConnectionID, v.Detail.CustomerKey)
	if err != nil {
		appError(w, r, err)
		return
	}
	v.CandidateID = r.URL.Query().Get("contact_id")
	if v.CandidateID != "" && v.CanOperate {
		v.Preview, err = a.identity.PreviewContact(r.Context(), principal(r), v.Tenant, v.Detail.Config.HubSpotConnectionID, v.Detail.CustomerKey, v.CandidateID)
		if err != nil {
			if errors.Is(err, identity.ErrContactMissing) {
				v.Error = "Contact missing or archived. Do not guess the identity."
			} else if errors.Is(err, identity.ErrContactConflict) {
				v.Error = "Contact identity conflicts with this Stripe customer."
			} else {
				v.Error = "Contact preview unavailable; try again."
			}
		} else {
			v.HasPreview = true
		}
	}
	a.renderUI(w, 200, "mapping", v)
}
func (a *application) mappingSubmit(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Run details")
	if !ok {
		return
	}
	if !v.CanOperate {
		apiError(w, r, 403, "forbidden")
		return
	}
	if err := r.ParseForm(); err != nil {
		apiError(w, r, 400, "invalid_request")
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if len(reason) < 3 || len(reason) > 1000 {
		apiError(w, r, 400, "invalid_reason")
		return
	}
	d, err := a.identity.BusinessRun(r.Context(), principal(r), v.Tenant, chi.URLParam(r, "run"))
	if err != nil {
		appError(w, r, err)
		return
	}
	if d.CustomerKey == "" || d.Config.HubSpotConnectionID == "" {
		apiError(w, r, 409, "mapping_unavailable")
		return
	}
	err = a.identity.LinkContactReviewed(r.Context(), principal(r), v.Tenant, d.Config.HubSpotConnectionID, d.CustomerKey, r.FormValue("contact_id"), reason, ingress.CorrelationID(r.Context()))
	if err != nil {
		a.log.Warn("Contact mapping rejected", "request_id", ingress.CorrelationID(r.Context()))
		apiError(w, r, 409, "mapping_conflict")
		return
	}
	http.Redirect(w, r, "/ui/tenants/"+v.Tenant+"/runs/"+d.ID+"?notice=linked", http.StatusSeeOther)
}

func (a *application) connectStripeTestAPI(w http.ResponseWriter, r *http.Request) {
	if a.cfg.DemoMode {
		apiError(w, r, http.StatusForbidden, "demo_provider_changes_disabled")
		return
	}
	var input identity.StripeTestSetup
	if !decodeJSON(w, r, &input) {
		return
	}
	id, err := a.identity.ConnectStripeTest(r.Context(), principal(r), chi.URLParam(r, "tenant"), input, ingress.CorrelationID(r.Context()))
	if err != nil {
		if errors.Is(err, identity.ErrForbidden) {
			appError(w, r, err)
		} else {
			apiError(w, r, 400, "stripe_setup_rejected")
		}
		return
	}
	writeJSON(w, 201, map[string]string{"connection_id": id, "status": "verified"})
}
func (a *application) overviewAPI(w http.ResponseWriter, r *http.Request) {
	v, err := a.identity.WorkspaceOverview(r.Context(), principal(r), chi.URLParam(r, "tenant"))
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 200, v)
}
func (a *application) billingRevisionsAPI(w http.ResponseWriter, r *http.Request) {
	v, err := a.identity.ListBillingRevisions(r.Context(), principal(r), chi.URLParam(r, "tenant"), 20)
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": v})
}
func (a *application) contactLinkAPI(w http.ResponseWriter, r *http.Request) {
	v, found, err := a.identity.ContactLink(r.Context(), principal(r), chi.URLParam(r, "tenant"), r.URL.Query().Get("hubspot_connection_id"), r.URL.Query().Get("customer_key"))
	if err != nil {
		appError(w, r, err)
		return
	}
	if !found {
		apiError(w, r, 404, "not_found")
		return
	}
	writeJSON(w, 200, v)
}
func (a *application) contactPreviewAPI(w http.ResponseWriter, r *http.Request) {
	v, err := a.identity.PreviewContact(r.Context(), principal(r), chi.URLParam(r, "tenant"), r.URL.Query().Get("hubspot_connection_id"), r.URL.Query().Get("customer_key"), r.URL.Query().Get("contact_id"))
	if err != nil {
		apiError(w, r, 409, "contact_preview_unavailable")
		return
	}
	writeJSON(w, 200, v)
}
