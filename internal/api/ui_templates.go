package api

import (
	"embed"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/identity"
)

//go:embed assets/*
var uiAssets embed.FS

func serveUIAsset(w http.ResponseWriter, r *http.Request) {
	file := strings.TrimPrefix(r.URL.Path, "/assets/")
	contentType := map[string]string{
		"htmx.min.js":           "text/javascript; charset=utf-8",
		"controls.js":           "text/javascript; charset=utf-8",
		"historical-picker.js":  "text/javascript; charset=utf-8",
		"vanilla-calendar.js":   "text/javascript; charset=utf-8",
		"vanilla-calendar.css":  "text/css; charset=utf-8",
		"historical-picker.css": "text/css; charset=utf-8",
		"app.css":               "text/css; charset=utf-8",
		"lenoirdesk-mark.svg":   "image/svg+xml",
	}[file]
	if contentType == "" {
		http.NotFound(w, r)
		return
	}
	raw, err := uiAssets.ReadFile("assets/" + file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(raw)
}

var (
	stripeEventID = regexp.MustCompile(`^evt_[A-Za-z0-9]+$`)
	hubspotID     = regexp.MustCompile(`^[0-9]{1,32}$`)
	slackChannel  = regexp.MustCompile(`^[CG][A-Z0-9]+$`)
	slackTS       = regexp.MustCompile(`^[0-9]{10,}\.[0-9]{6}$`)
	safeErrorCode = regexp.MustCompile(`^[a-z0-9_:-]+$`)
)

func safeStripeEventURL(id string) string {
	if !stripeEventID.MatchString(id) {
		return ""
	}
	return "https://dashboard.stripe.com/test/events/" + id
}
func safeHubSpotContactURL(portal, id string) string {
	if !hubspotID.MatchString(portal) || !hubspotID.MatchString(id) {
		return ""
	}
	return "https://app.hubspot.com/contacts/" + portal + "/record/0-1/" + id
}
func safeSlackMessageURL(channel, ts string) string {
	if !slackChannel.MatchString(channel) || !slackTS.MatchString(ts) {
		return ""
	}
	return "https://app.slack.com/archives/" + channel + "/p" + strings.ReplaceAll(ts, ".", "")
}
func statusLabel(s string) string {
	switch s {
	case "succeeded":
		return "Succeeded"
	case "pending", "queued":
		return "Planned"
	case "running":
		return "Executing"
	case "retry_wait":
		return "Waiting to retry"
	case "blocked_auth":
		return "Connection needs attention"
	case "blocked_config":
		return "Configuration blocked"
	case "needs_attention":
		return "Needs attention"
	case "unknown":
		return "Outcome unknown"
	case "dead":
		return "Dead letter"
	case "paused":
		return "Paused"
	case "active":
		return "Active"
	case "verified":
		return "Verified"
	case "disconnected":
		return "Disconnected"
	case "invalid":
		return "Invalid"
	case "degraded":
		return "Degraded"
	case "unverified":
		return "Unverified"
	default:
		return strings.ReplaceAll(s, "_", " ")
	}
}
func safeErrorLabel(s string) string {
	if s == "" {
		return "No recorded error"
	}
	switch {
	case strings.Contains(s, "identity"), strings.Contains(s, "mapped_contact"):
		return "Contact identity needs review"
	case strings.Contains(s, "auth"), strings.Contains(s, "credential"):
		return "Reconnect this provider"
	case strings.Contains(s, "quota"), strings.Contains(s, "rate_limited"):
		return "Provider rate limit; retry is scheduled"
	case strings.Contains(s, "circuit"), strings.Contains(s, "connect"), strings.Contains(s, "timeout"):
		return "Provider temporarily unavailable"
	case strings.Contains(s, "slack_result_unknown"):
		return "Slack may have accepted the message; confirm before retrying"
	default:
		if len(s) > 80 || !safeErrorCode.MatchString(s) {
			return "Provider action failed; use the request reference for support"
		}
		return "See safe error code: " + s
	}
}

func brandInitial(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "L"
	}
	return strings.ToUpper(string([]rune(value)[0]))
}

type onboardingStep struct{ Label, Detail, Link, State string }
type uiFilter struct{ Key, Label string }
type automationDraft struct {
	Stripe, HubSpot, Slack, Prices, Channel, Policy string
}
type historicalDraft struct {
	Connection, From, To, Max, Reason string
}
type trendBar struct {
	Date   string
	Count  int64
	X      int
	Y      int
	Height int
}
type providerCard struct {
	Key         string
	Connections []identity.Connection
	State       string
}
type uiView struct {
	Tenant                 string
	Lang                   string
	Theme                  string
	EnglishURL             string
	RussianURL             string
	LightURL               string
	DarkURL                string
	SystemURL              string
	Workspace              identity.Workspace
	User                   identity.Principal
	CSRF                   string
	Screen                 string
	Notice                 string
	Error                  string
	CanOperate             bool
	IsOwner                bool
	DemoMode               bool
	AllowConnectionChanges bool
	Overview               identity.Overview
	TrendBars              []trendBar
	TrendTotal             int64
	TrendMax               int64
	TrendStart             string
	TrendEnd               string
	AttentionCount         int64
	OverallState           string
	CompleteSteps          int
	ShowOnboarding         bool
	ProviderCards          []providerCard
	Operational            identity.OperationalStatus
	ReconcileState         string
	CoverageGaps           int
	LastCompleteAt         *time.Time
	HasReconcileConnection bool
	Historical             identity.HistoricalReconcileInput
	HistoricalDraft        historicalDraft
	EstimatedPages         int
	Connections            []identity.Connection
	ReconcileConnections   []identity.Connection
	Revisions              []identity.BillingRevision
	ActiveRevision         *identity.BillingRevision
	NextRevision           int
	AutomationDraft        automationDraft
	FieldErrors            map[string]string
	Runs                   []identity.Run
	RunCounts              map[string]int64
	RunTotal               int64
	NextCursor             string
	Filter                 string
	Filters                []uiFilter
	Steps                  []onboardingStep
	Detail                 identity.RunDetail
	RunStages              []runStage
	RunTimeline            []runTimelineItem
	CRMNoWrite             bool
	RestoreBlocked         bool
	HubSpotPortal          string
	StripeRef              string
	WebhookPath            string
	Preview                identity.ContactPreview
	HasPreview             bool
	Link                   identity.ContactLink
	HasLink                bool
	CandidateID            string
	ManagedProperties      []string
}

var uiTemplates = template.Must(template.New("ui").Funcs(template.FuncMap{
	"tr":                tr,
	"initial":           brandInitial,
	"status":            statusLabel,
	"errorLabel":        safeErrorLabel,
	"stripeEventURL":    safeStripeEventURL,
	"hubspotContactURL": safeHubSpotContactURL,
	"slackMessageURL":   safeSlackMessageURL,
	"add":               func(a, b, c, d, e int64) int64 { return a + b + c + d + e },
	"join":              func(values []string) string { return strings.Join(values, ",") },
	"priceLines":        func(values []string) string { return strings.Join(values, "\n") },
	"runStep":           runStepLabel,
	"runStatusHelp":     runStatusHelp,
	"runFilterCount":    runFilterCount,
	"runError":          safeRunError,
	"displayTime":       displayTime,
	"displayDay":        displayDay,
	"timelineNote":      timelineNote,
	"auditLabel":        auditLabel,
	"displayCount":      displayCount,
	"displayDuration":   displayDuration,
	"stageStatus":       stageStatusLabel,
	"hasVerified":       hasVerifiedProvider,
	"policyLabel":       func(lang, policy string) string { return tr(lang, "policy_"+policy) },
	"nextRevision":      func(revisions []identity.BillingRevision) int { return nextRevision(revisions) },
	"connectionAccount": func(lang string, connections []identity.Connection, id string) string {
		for _, c := range connections {
			if c.ID == id {
				return c.ProviderAccountID + " · " + tr(lang, c.Environment)
			}
		}
		return tr(lang, "automation_account_unavailable")
	},
}).Parse(uiTemplateSource + mission03Templates + mission04Templates))

const uiTemplateSource = `
{{define "top"}}<!doctype html><html lang="{{.Lang}}" data-theme="{{.Theme}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="theme-color" content="#30212a"><title>{{tr .Lang .Screen}} · LenoirDesk</title><link rel="icon" type="image/svg+xml" href="/assets/lenoirdesk-mark.svg"><link rel="stylesheet" href="/assets/app.css">{{if eq .Screen "Reconciliation"}}<link rel="stylesheet" href="/assets/vanilla-calendar.css"><link rel="stylesheet" href="/assets/historical-picker.css">{{end}}<script src="/assets/htmx.min.js" defer></script><script src="/assets/controls.js" defer></script>{{if eq .Screen "Reconciliation"}}<script src="/assets/vanilla-calendar.js" defer></script><script src="/assets/historical-picker.js" defer></script>{{end}}</head><body><a class="skip" href="#content">{{tr .Lang "skip_to_content"}}</a><div class="shell"><aside class="side"><a class="brand" href="/" aria-label="LenoirDesk"><img class="brand-mark" src="/assets/lenoirdesk-mark.svg" alt=""><span><span class="brand-word">Lenoir<span>Desk</span></span><span class="brand-caption">{{tr .Lang "brand_descriptor"}}</span></span></a><a class="workspace-switch" href="/" aria-label="{{tr .Lang "change_workspace"}}: {{.Workspace.Name}}"><span class="workspace-initial" aria-hidden="true">{{initial .Workspace.Name}}</span><span><small>{{tr .Lang "current_workspace"}}</small><strong>{{.Workspace.Name}}</strong></span><span class="workspace-chevron" aria-hidden="true">⌄</span></a><p class="nav-caption">{{tr .Lang "workspace_navigation"}}</p><nav aria-label="{{tr .Lang "workspace_navigation"}}"><a href="/ui/tenants/{{.Tenant}}" {{if eq .Screen "Overview"}}aria-current="page"{{end}}><span class="nav-icon" aria-hidden="true">{{template "nav_icon" "overview"}}</span>{{tr .Lang "Overview"}}</a><a href="/ui/tenants/{{.Tenant}}/connections" {{if eq .Screen "Connections"}}aria-current="page"{{end}}><span class="nav-icon" aria-hidden="true">{{template "nav_icon" "connections"}}</span>{{tr .Lang "Connections"}}</a><a href="/ui/tenants/{{.Tenant}}/billing-sync" {{if eq .Screen "Automation"}}aria-current="page"{{end}}><span class="nav-icon" aria-hidden="true">{{template "nav_icon" "automation"}}</span>{{tr .Lang "Automation"}}</a><a href="/ui/tenants/{{.Tenant}}/runs" {{if or (eq .Screen "Runs") (eq .Screen "Run details")}}aria-current="page"{{end}}><span class="nav-icon" aria-hidden="true">{{template "nav_icon" "runs"}}</span>{{tr .Lang "Runs"}}</a><a href="/ui/tenants/{{.Tenant}}/reconciliation" {{if eq .Screen "Reconciliation"}}aria-current="page"{{end}}><span class="nav-icon" aria-hidden="true">{{template "nav_icon" "reconciliation"}}</span>{{tr .Lang "Reconciliation"}}</a></nav><div class="side-footer"><div class="side-user"><span class="avatar" aria-hidden="true">{{initial .User.Email}}</span><span><strong>{{tr .Lang (printf "role_%s" .Workspace.Role)}}</strong><small>{{.User.Email}}</small></span></div><form method="post" action="/auth/logout"><input type="hidden" name="csrf_token" value="{{.CSRF}}"><button type="submit">{{tr .Lang "sign_out"}}</button></form></div></aside><div class="app-area"><header class="topbar"><div class="topbar-context"><small>{{.Workspace.Name}}</small><strong>{{tr .Lang .Screen}}</strong></div><div class="topbar-tools"><nav class="language-switch" aria-label="{{tr .Lang "language"}}"><a href="{{.EnglishURL}}" lang="en" hreflang="en" {{if eq .Lang "en"}}aria-current="true"{{end}}>EN</a><a href="{{.RussianURL}}" lang="ru" hreflang="ru" {{if eq .Lang "ru"}}aria-current="true"{{end}}>RU</a></nav>{{template "theme_switch" .}}<span class="topbar-email">{{.User.Email}}</span><details class="account-menu"><summary aria-label="{{tr .Lang "account"}}">{{initial .User.Email}}</summary><div class="account-popover"><small>{{tr .Lang "workspace_signed_in"}}</small><strong>{{.User.Email}}</strong><form method="post" action="/auth/logout"><input type="hidden" name="csrf_token" value="{{.CSRF}}"><button type="submit" class="secondary">{{tr .Lang "sign_out"}}</button></form></div></details></div></header><main class="main" id="content"><p class="eyebrow">{{.Workspace.Name}} / {{tr .Lang .Screen}}</p>{{if .Error}}<div class="alert error" role="alert">{{tr .Lang .Error}}</div>{{end}}{{if .Notice}}<div class="alert success" role="status">{{tr .Lang .Notice}}</div>{{end}}
{{end}}
{{define "bottom"}}</main></div></div><dialog class="action-dialog" id="action-confirmation" aria-labelledby="action-confirmation-title" aria-describedby="action-confirmation-body action-confirmation-target"><h2 id="action-confirmation-title"></h2><p id="action-confirmation-body"></p><p class="dialog-target" id="action-confirmation-target" hidden><strong>{{tr .Lang "confirm_dialog_target"}}:</strong> <code></code></p><div class="actions"><button type="button" class="secondary" data-dialog-cancel>{{tr .Lang "confirm_dialog_cancel"}}</button><button type="button" data-dialog-accept></button></div></dialog></body></html>{{end}}
{{define "theme_switch"}}<details class="theme-menu"><summary aria-label="{{tr .Lang "appearance"}}" title="{{tr .Lang "appearance"}}"><span aria-hidden="true">◐</span><span class="theme-current">{{tr .Lang .Theme}}</span></summary><div class="theme-popover" role="group" aria-label="{{tr .Lang "appearance"}}"><a href="{{.LightURL}}" {{if eq .Theme "light"}}aria-current="true"{{end}}>{{tr .Lang "light"}}</a><a href="{{.DarkURL}}" {{if eq .Theme "dark"}}aria-current="true"{{end}}>{{tr .Lang "dark"}}</a><a href="{{.SystemURL}}" {{if eq .Theme "system"}}aria-current="true"{{end}}>{{tr .Lang "system"}}</a></div></details>{{end}}
{{define "nav_icon"}}<svg viewBox="0 0 24 24" aria-hidden="true">{{if eq . "overview"}}<rect x="3" y="3" width="7" height="7" rx="1.3"/><rect x="14" y="3" width="7" height="7" rx="1.3"/><rect x="3" y="14" width="7" height="7" rx="1.3"/><rect x="14" y="14" width="7" height="7" rx="1.3"/>{{else if eq . "connections"}}<path d="M8.5 8.5l7 7M8 3a5 5 0 0 1 5 5c0 2.7-2.3 5-5 5a5 5 0 0 1 0-10Zm8 8a5 5 0 0 1 5 5 5 5 0 0 1-10 0c0-2.7 2.3-5 5-5Z"/>{{else if eq . "automation"}}<circle cx="5" cy="5" r="2"/><circle cx="19" cy="5" r="2"/><circle cx="12" cy="19" r="2"/><path d="M7 5h10M5.8 7l5 10M18.2 7l-5 10"/>{{else if eq . "runs"}}<path d="M8 6h13M8 12h13M8 18h13"/><circle cx="4" cy="6" r="1"/><circle cx="4" cy="12" r="1"/><circle cx="4" cy="18" r="1"/>{{else}}<path d="M20 7H5l3-3M4 17h15l-3 3M5 7a7 7 0 0 0 0 10M19 17a7 7 0 0 0 0-10"/>{{end}}</svg>{{end}}

{{define "overview"}}{{template "top" .}}
<section class="dashboard-hero"><div><p class="eyebrow">{{tr .Lang "dashboard_eyebrow"}}</p><h1>{{tr .Lang "overview_title"}}</h1><p class="lead">{{tr .Lang "overview_body"}}</p></div><div class="overall-status {{.OverallState}}" role="status"><span class="status-pulse" aria-hidden="true"></span><div><strong>{{tr .Lang (printf "overall_%s" .OverallState)}}</strong><small>{{tr .Lang (printf "overall_detail_%s" .OverallState)}}</small></div></div></section>
{{if .ShowOnboarding}}<section class="onboarding-panel card"><div class="section-head"><div><p class="eyebrow">{{tr .Lang "onboarding_eyebrow"}}</p><h2>{{tr .Lang "getting_started"}}</h2><p class="muted">{{tr .Lang "onboarding_body"}}</p></div><span class="progress-count">{{.CompleteSteps}} / {{len .Steps}} {{tr .Lang "steps_complete"}}</span></div><div class="progress-track" role="progressbar" aria-label="{{tr .Lang "onboarding_progress"}}" aria-valuemin="0" aria-valuemax="5" aria-valuenow="{{.CompleteSteps}}"><span class="progress-fill step-{{.CompleteSteps}}"></span></div><div class="onboarding-steps">{{range .Steps}}<div class="onboarding-step {{.State}}"><span class="step-index" aria-hidden="true">{{if eq .State "complete"}}✓{{else}}○{{end}}</span><div><strong>{{tr $.Lang .Label}}</strong><span class="badge {{.State}}">{{tr $.Lang .State}}</span><p>{{tr $.Lang .Detail}}</p>{{if .Link}}<a href="{{.Link}}">{{tr $.Lang "open_step"}} →</a>{{end}}</div></div>{{end}}</div></section>{{end}}
<section class="kpi-grid" aria-label="{{tr .Lang "dashboard_metrics"}}"><div class="card kpi-card"><span class="kpi-symbol" aria-hidden="true">✓</span><h2>{{tr .Lang "successful_runs"}}</h2><p class="metric">{{index .Overview.RunCounts "succeeded"}}</p><p class="muted">{{tr .Lang "last_sync"}} {{if .Overview.LastSuccessful}}{{displayTime $.Lang .Overview.LastSuccessful.UTC false}}{{else}}{{tr .Lang "none_yet"}}{{end}}</p></div><div class="card kpi-card"><span class="kpi-symbol" aria-hidden="true">◇</span><h2>{{tr .Lang "active_automations"}}</h2><p class="metric">{{.Overview.ActiveRevisions}}</p><p class="muted">{{tr .Lang "active_automations_body"}}</p></div><div class="card kpi-card"><span class="kpi-symbol" aria-hidden="true">↻</span><h2>{{tr .Lang "backlog"}}</h2><p class="metric">{{.Overview.Backlog}}</p><p class="muted">{{tr .Lang "backlog_body"}}</p></div><div class="card kpi-card {{if gt .AttentionCount 0}}attention{{end}}"><span class="kpi-symbol" aria-hidden="true">!</span><h2>{{tr .Lang "needs_action"}}</h2><p class="metric">{{.AttentionCount}}</p><p><a href="/ui/tenants/{{.Tenant}}/runs">{{tr .Lang "inspect_runs"}} →</a></p></div></section>
{{if gt (index .Overview.RunCounts "unknown") 0}}<div class="alert" role="status">{{tr .Lang "unknown_result_notice"}} <a href="/ui/tenants/{{.Tenant}}/runs?status=unknown">{{tr .Lang "review_unknown_runs"}}</a>.</div>{{end}}
{{range .Operational.Checkpoints}}{{if .GapCode}}<div class="alert error" role="alert">{{tr $.Lang "coverage_gap"}} ({{.Kind}}): {{.GapCode}}. {{tr $.Lang "coverage_gap_body"}} <a href="/ui/tenants/{{$.Tenant}}/reconciliation">{{tr $.Lang "review_coverage"}}</a>.</div>{{end}}{{end}}
{{if gt .Operational.OldestOutboxSeconds 300}}<div class="alert" role="status">{{tr .Lang "outbox_age"}} {{.Operational.OldestOutboxSeconds}}s. {{tr .Lang "outbox_age_body"}} <a href="/ui/tenants/{{.Tenant}}/reconciliation">{{tr .Lang "operations"}}</a>.</div>{{end}}
<div class="dashboard-primary">{{if gt .TrendTotal 0}}<figure class="card trend-card"><div class="section-head"><div><p class="eyebrow">{{tr .Lang "activity_eyebrow"}}</p><h2>{{tr .Lang "run_trend"}}</h2></div><span class="range-chip">{{tr .Lang "last_14_days_utc"}}</span></div><svg class="trend-chart" viewBox="0 0 700 220" role="img" aria-labelledby="trend-title trend-description"><title id="trend-title">{{tr .Lang "run_trend"}}</title><desc id="trend-description">{{displayCount .Lang .TrendTotal "run"}} {{tr .Lang "runs_in_period"}}; {{tr .Lang "peak_day"}} {{.TrendMax}}.</desc><line x1="28" y1="174" x2="664" y2="174" class="chart-axis"/><line x1="28" y1="42" x2="664" y2="42" class="chart-guide"/><text x="29" y="33" class="chart-label">{{.TrendMax}}</text>{{range .TrendBars}}{{if gt .Count 0}}<rect x="{{.X}}" y="{{.Y}}" width="27" height="{{.Height}}" rx="5" tabindex="0" class="chart-bar"><title>{{displayDay $.Lang .Date}} · {{.Count}}</title></rect>{{end}}{{end}}<text x="28" y="202" class="chart-label">{{displayDay .Lang .TrendStart}}</text><text x="660" y="202" text-anchor="end" class="chart-label">{{displayDay .Lang .TrendEnd}} UTC</text></svg><figcaption>{{displayCount .Lang .TrendTotal "run"}} {{tr .Lang "runs_in_period"}} · {{tr .Lang "chart_tooltip_hint"}}</figcaption><div class="sr-only"><table><caption>{{tr .Lang "run_trend"}} · UTC</caption><thead><tr><th>{{tr .Lang "date"}}</th><th>{{tr .Lang "runs_count"}}</th></tr></thead><tbody>{{range .TrendBars}}<tr><td>{{displayDay $.Lang .Date}}</td><td>{{.Count}}</td></tr>{{end}}</tbody></table></div></figure>{{else}}<section class="card trend-empty"><p class="eyebrow">{{tr .Lang "activity_eyebrow"}}</p><h2>{{tr .Lang "no_activity_title"}}</h2><p>{{tr .Lang "no_activity_body"}}</p><a href="/ui/tenants/{{.Tenant}}/connections">{{tr .Lang "review_connections"}} →</a></section>{{end}}<section class="card activity-card"><div class="section-head"><h2>{{tr .Lang "recent_runs"}}</h2><a href="/ui/tenants/{{.Tenant}}/runs">{{tr .Lang "view_all_runs"}} →</a></div>{{if .Runs}}<ol class="activity-list">{{range .Runs}}<li><span class="activity-dot {{.Status}}" aria-hidden="true"></span><div><a href="/ui/tenants/{{$.Tenant}}/runs/{{.ID}}">{{.EventType}}</a><small>{{displayTime $.Lang .CreatedAt.UTC false}}</small></div><span class="badge {{.Status}}">{{tr $.Lang (status .Status)}}</span></li>{{end}}</ol>{{else}}<p class="muted">{{tr .Lang "no_runs"}}</p>{{end}}</section></div>
<div class="dashboard-secondary"><section class="card"><div class="section-head"><div><p class="eyebrow">{{tr .Lang "provider_layer"}}</p><h2>{{tr .Lang "provider_health"}}</h2></div><a href="/ui/tenants/{{.Tenant}}/connections">{{tr .Lang "manage_connections"}} →</a></div><div class="provider-health-list">{{range .ProviderCards}}<div><span class="provider-mini {{.Key}}" aria-hidden="true">{{if eq .Key "stripe"}}S{{else if eq .Key "hubspot"}}H{{else}}#{{end}}</span><strong>{{if eq .Key "stripe"}}Stripe{{else if eq .Key "hubspot"}}HubSpot{{else}}Slack{{end}}</strong><span class="badge {{.State}}">{{tr $.Lang (printf "connection_%s" .State)}}</span></div>{{end}}</div></section><section class="card"><div class="section-head"><div><p class="eyebrow">{{tr .Lang "event_layer"}}</p><h2>{{tr .Lang "recent_business_events"}}</h2></div></div>{{if .Overview.RecentEvents}}<ol class="event-list">{{range .Overview.RecentEvents}}<li><span><strong>{{.Type}}</strong><small><code>{{.ID}}</code></small></span><time>{{displayTime $.Lang .ReceivedAt.UTC false}}</time></li>{{end}}</ol>{{else}}<p class="muted">{{tr .Lang "no_events"}}</p>{{end}}</section></div>
{{if not .ShowOnboarding}}<details class="card onboarding-summary"><summary><strong>{{tr .Lang "onboarding_status"}}</strong><span>{{.CompleteSteps}} / {{len .Steps}} {{tr .Lang "steps_complete"}}</span></summary><div class="onboarding-steps">{{range .Steps}}<div class="onboarding-step {{.State}}"><span class="step-index" aria-hidden="true">{{if eq .State "complete"}}✓{{else}}○{{end}}</span><div><strong>{{tr $.Lang .Label}}</strong><span class="badge {{.State}}">{{tr $.Lang .State}}</span><p>{{tr $.Lang .Detail}}</p>{{if .Link}}<a href="{{.Link}}">{{tr $.Lang "open_step"}} →</a>{{end}}</div></div>{{end}}</div></details>{{end}}
{{template "bottom" .}}{{end}}

{{define "connections"}}{{template "top" .}}
<header class="page-heading"><p class="eyebrow">{{tr .Lang "provider_layer"}}</p><h1>{{tr .Lang "Connections"}}</h1><p class="lead">{{tr .Lang "connections_intro"}}</p></header>
{{if not .Connections}}<div class="connection-intro" role="status"><strong>{{tr .Lang "No providers connected"}}</strong><span>{{tr .Lang "connections_empty_body"}}</span></div>{{end}}
<div class="provider-grid">{{range .ProviderCards}}<article class="provider-card card {{.State}}"><header class="provider-card-head"><span class="provider-emblem {{.Key}}" aria-hidden="true">{{if eq .Key "stripe"}}S{{else if eq .Key "hubspot"}}H{{else}}#{{end}}</span><div><h2>{{if eq .Key "stripe"}}Stripe{{else if eq .Key "hubspot"}}HubSpot{{else}}Slack{{end}}</h2><p>{{tr $.Lang (printf "provider_intro_%s" .Key)}}</p></div><span class="badge {{.State}}">{{tr $.Lang (printf "connection_%s" .State)}}</span></header>
{{if .Connections}}<div class="connection-list">{{range .Connections}}<section class="connection-item"><div class="connection-item-head"><div><small>{{tr $.Lang "verified_account_identity"}}</small><strong><code>{{.ProviderAccountID}}</code></strong></div><span class="connection-state"><span class="badge {{.Status}}">{{tr $.Lang (status .Status)}}</span><span class="badge {{.HealthStatus}}">{{tr $.Lang (status .HealthStatus)}}</span></span></div><dl class="connection-meta"><dt>{{tr $.Lang "environment"}}</dt><dd>{{tr $.Lang .Environment}} · {{tr $.Lang .Mode}}</dd><dt>{{tr $.Lang "last_verified"}}</dt><dd>{{if .VerifiedAt}}{{displayTime $.Lang .VerifiedAt.UTC false}}{{else}}{{tr $.Lang "not_recorded"}}{{end}}</dd><dt>{{tr $.Lang "last_checked"}}</dt><dd>{{if .LastCheckedAt}}{{displayTime $.Lang .LastCheckedAt.UTC false}}{{else}}{{tr $.Lang "not_checked"}}{{end}}</dd><dt>{{tr $.Lang "credential_health"}}</dt><dd>{{if eq .Status "disconnected"}}{{tr $.Lang "credential_revoked"}}{{else if eq .Environment "simulation"}}{{tr $.Lang "local_simulator_fixture"}}{{else if eq .Provider "stripe"}}{{tr $.Lang "restricted_test_key"}}{{else if .AuthState}}{{tr $.Lang (status .AuthState)}}{{else}}{{tr $.Lang "grant_status_unavailable"}}{{end}}{{if .ExpiresAt}} · {{tr $.Lang "expires"}} {{displayTime $.Lang .ExpiresAt.UTC false}}{{end}}</dd></dl>{{if or (ne .HealthStatus "verified") (eq .AuthState "blocked_auth")}}<p class="connection-remedy">{{tr $.Lang "connection_remedy"}}</p>{{end}}<details class="provider-help"><summary>{{tr $.Lang "required_access"}}</summary><p>{{tr $.Lang (printf "required_access_%s" .Provider)}}</p></details>{{if $.AllowConnectionChanges}}<div class="connection-actions">{{if eq .Provider "stripe"}}<a class="button secondary" href="/ui/tenants/{{$.Tenant}}/connections/stripe/new?reconnect={{.ID}}">{{tr $.Lang "replace_credentials"}}</a>{{else}}<form method="post" action="/ui/tenants/{{$.Tenant}}/connections/oauth/{{.Provider}}"><input type="hidden" name="csrf_token" value="{{$.CSRF}}"><input type="hidden" name="expected_account_id" value="{{.ProviderAccountID}}"><input type="hidden" name="reconnect_id" value="{{.ID}}"><button class="secondary">{{tr $.Lang "reconnect_same_account"}}</button></form>{{end}}<details class="danger-zone"><summary>{{tr $.Lang "disconnect"}}</summary><p>{{tr $.Lang "disconnect_warning"}}</p><form method="post" action="/ui/tenants/{{$.Tenant}}/connections/{{.ID}}/disconnect" data-confirm-title="{{tr $.Lang "confirm_disconnect"}}" data-confirm-body="{{tr $.Lang "disconnect_warning"}}" data-confirm-target="{{.ProviderAccountID}}" data-confirm-action="{{tr $.Lang "confirm_disconnect"}}" data-confirm-level="danger"><input type="hidden" name="csrf_token" value="{{$.CSRF}}"><button class="danger">{{tr $.Lang "confirm_disconnect"}}</button></form></details></div>{{end}}</section>{{end}}</div>{{else}}<div class="provider-empty"><span aria-hidden="true">○</span><p>{{tr $.Lang "provider_not_connected"}}</p></div>{{end}}
{{if $.AllowConnectionChanges}}<details class="setup-flow"><summary>{{if .Connections}}{{tr $.Lang (printf "add_another_%s" .Key)}}{{else if eq .Key "stripe"}}{{tr $.Lang "Set up Stripe"}}{{else if eq .Key "hubspot"}}{{tr $.Lang "Connect HubSpot"}}{{else}}{{tr $.Lang "Connect Slack"}}{{end}} <span aria-hidden="true">↗</span></summary><div class="setup-flow-body"><p>{{tr $.Lang (printf "setup_intro_%s" .Key)}}</p>{{if eq .Key "stripe"}}<a class="button" href="/ui/tenants/{{$.Tenant}}/connections/stripe/new">{{tr $.Lang "Set up Stripe"}}</a>{{else}}<form method="post" action="/ui/tenants/{{$.Tenant}}/connections/oauth/{{.Key}}"><input type="hidden" name="csrf_token" value="{{$.CSRF}}"><label for="{{.Key}}-account">{{if eq .Key "hubspot"}}{{tr $.Lang "expected_hubspot_portal"}}{{else}}{{tr $.Lang "expected_slack_workspace"}}{{end}}</label><input id="{{.Key}}-account" name="expected_account_id" {{if eq .Key "hubspot"}}inputmode="numeric" pattern="[0-9]+"{{else}}pattern="T[A-Z0-9]+"{{end}} required><p class="hint">{{tr $.Lang "identity_confirmation_help"}}</p><button>{{if eq .Key "hubspot"}}{{tr $.Lang "Connect HubSpot"}}{{else}}{{tr $.Lang "Connect Slack"}}{{end}}</button></form>{{end}}<details class="provider-help"><summary>{{tr $.Lang "setup_help"}}</summary><p>{{tr $.Lang (printf "setup_help_%s" .Key)}}</p><p><strong>{{tr $.Lang "required_access"}}:</strong> {{tr $.Lang (printf "required_access_%s" .Key)}}</p></details></div></details>{{end}}</article>{{end}}</div>
{{if .DemoMode}}<p class="muted" role="status">{{tr .Lang "demo_connections_readonly"}}</p>{{else if not .IsOwner}}<p class="muted">{{tr .Lang "connection_owner_only"}}</p>{{end}}
{{template "bottom" .}}{{end}}

{{define "stripe_setup"}}{{template "top" .}}
<header class="page-heading"><h1>{{tr .Lang "stripe_setup_title"}}</h1><p class="lead">{{tr .Lang "stripe_setup_intro"}}</p></header>
<div class="alert">{{tr .Lang "stripe_setup_destination"}} <code>invoice.paid</code>, <code>invoice.payment_failed</code>, <code>customer.subscription.created/updated/deleted</code> · <code>2026-08-26.dahlia</code>. {{tr .Lang "stripe_setup_tunnel"}}</div>
<section class="card"><h2>{{tr .Lang "stripe_setup_path_title"}}</h2><p><code>{{.WebhookPath}}</code></p><p class="hint">{{tr .Lang "stripe_setup_path_help"}}</p></section>
<section class="card"><h2>{{tr .Lang "stripe_setup_credentials_title"}}</h2><form method="post" action="/ui/tenants/{{.Tenant}}/connections/stripe"><input type="hidden" name="csrf_token" value="{{.CSRF}}"><input type="hidden" name="webhook_ref" value="{{.StripeRef}}"><input type="hidden" name="reconnect_id" value="{{.CandidateID}}"><label for="stripe-account">{{tr .Lang "stripe_setup_account"}}</label><input id="stripe-account" name="expected_account_id" value="{{.Preview.CustomerKey}}" pattern="acct_[A-Za-z0-9]+" required><span class="hint">{{tr .Lang "stripe_setup_account_help"}}</span><label for="restricted-key">{{tr .Lang "stripe_setup_restricted_key"}}</label><input id="restricted-key" type="password" name="restricted_key" autocomplete="off" required><label for="signing-key">{{tr .Lang "stripe_setup_signing_secret"}}</label><input id="signing-key" type="password" name="webhook_signing_secret" autocomplete="off" required><p><button>{{tr .Lang "stripe_setup_submit"}}</button></p></form></section>{{template "bottom" .}}{{end}}


`
