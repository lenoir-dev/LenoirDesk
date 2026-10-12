package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lenoir-dev/LenoirDesk/internal/config"
	"github.com/lenoir-dev/LenoirDesk/internal/identity"
	"github.com/lenoir-dev/LenoirDesk/internal/ingress"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

type application struct {
	cfg      config.Config
	identity *identity.Service
	log      *slog.Logger
}

const flowCookie = "relaydesk_oidc_state"

func (a *application) secureCookie() bool { return strings.HasPrefix(a.cfg.PublicOrigin, "https://") }
func (a *application) sessionCookieName() string {
	if a.secureCookie() {
		return "__Host-relaydesk"
	}
	return "relaydesk_dev_session"
}
func (a *application) setCookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path,
		MaxAge: maxAge, Secure: a.secureCookie(), HttpOnly: true,
		SameSite: http.SameSiteLaxMode})
}
func (a *application) sessionToken(r *http.Request) string {
	c, err := r.Cookie(a.sessionCookieName())
	if err != nil {
		return ""
	}
	return c.Value
}

func (a *application) routes(router chi.Router) {
	router.Get("/assets/{asset}", serveUIAsset)
	router.Get("/auth/login", a.login)
	router.Get("/auth/callback", a.callback)
	router.Post("/auth/logout", a.withSession(a.logout))
	router.Get("/", a.home)
	router.Route("/v1", func(r chi.Router) {
		r.Use(a.requireSession)
		r.Get("/me", a.me)
		r.Get("/tenants", a.tenants)
		r.Post("/tenants", a.createTenant)
		r.Get("/tenants/{tenant}", a.tenant)
		r.Get("/tenants/{tenant}/members", a.members)
		r.Post("/tenants/{tenant}/members", a.grantMember)
		r.Patch("/tenants/{tenant}/members/{user}", a.changeMemberRole)
		r.Delete("/tenants/{tenant}/members/{user}", a.removeMember)
		r.Get("/tenants/{tenant}/connections", a.connections)
		r.Post("/tenants/{tenant}/connections/stripe-test", a.connectStripeTestAPI)
		r.Post("/tenants/{tenant}/connections/oauth/{provider}", a.beginProviderOAuth)
		r.Delete("/tenants/{tenant}/connections/{connection}", a.disconnectProvider)
		r.Get("/tenants/{tenant}/connections/{connection}", a.connection)
		r.Get("/tenants/{tenant}/runs", a.runs)
		r.Get("/tenants/{tenant}/overview", a.overviewAPI)
		r.Get("/tenants/{tenant}/reconciliation", a.reconciliationStatusAPI)
		r.Post("/tenants/{tenant}/reconciliations", a.requestHistoricalAPI)
		r.Get("/tenants/{tenant}/billing-sync/revisions", a.billingRevisionsAPI)
		r.Get("/tenants/{tenant}/runs/{run}", a.runDetail)
		r.Post("/tenants/{tenant}/actions/{action}/retry", a.retryBusinessAction)
		r.Post("/tenants/{tenant}/actions/{action}/resolve", a.resolveBusinessAction)
		r.Get("/tenants/{tenant}/billing-sync/{stripe}", a.billingSync)
		r.Post("/tenants/{tenant}/billing-sync", a.configureBillingSync)
		r.Post("/tenants/{tenant}/billing-sync/{stripe}/pause", a.pauseBillingSync)
		r.Post("/tenants/{tenant}/contact-links", a.linkBillingContact)
		r.Get("/tenants/{tenant}/contact-links", a.contactLinkAPI)
		r.Get("/tenants/{tenant}/contacts/preview", a.contactPreviewAPI)
	})
	router.Get("/oauth/{provider}/callback", a.providerOAuthCallback)
	router.Route("/ui", func(r chi.Router) {
		r.Use(a.requireSession)
		r.Post("/tenants", a.createTenantUI)
		r.Get("/tenants/{tenant}", a.overviewScreen)
		r.Get("/tenants/{tenant}/connections", a.connectionsScreen)
		r.Get("/tenants/{tenant}/connections/stripe/new", a.stripeSetupScreen)
		r.Post("/tenants/{tenant}/connections/stripe", a.stripeSetupSubmit)
		r.Post("/tenants/{tenant}/connections/{connection}/disconnect", a.disconnectScreen)
		r.Post("/tenants/{tenant}/connections/oauth/{provider}", a.beginProviderOAuthUI)
		r.Get("/tenants/{tenant}/runs", a.runsScreen)
		r.Get("/tenants/{tenant}/reconciliation", a.reconciliationScreen)
		r.Post("/tenants/{tenant}/reconciliation/preview", a.reconciliationPreviewScreen)
		r.Post("/tenants/{tenant}/reconciliation", a.reconciliationSubmit)
		r.Get("/tenants/{tenant}/runs/{run}", a.runScreen)
		r.Get("/tenants/{tenant}/runs/{run}/mapping", a.mappingScreen)
		r.Post("/tenants/{tenant}/runs/{run}/mapping", a.mappingSubmit)
		r.Post("/tenants/{tenant}/runs/{run}/actions/{action}/recover", a.recoverBusinessActionUI)
		r.Get("/tenants/{tenant}/billing-sync", a.automationScreen)
		r.Post("/tenants/{tenant}/billing-sync/preview", a.automationPreviewScreen)
		r.Post("/tenants/{tenant}/billing-sync", a.configureBillingSyncUI)
		r.Post("/tenants/{tenant}/billing-sync/pause", a.pauseBillingSyncUI)
	})
}

func (a *application) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func apiError(w http.ResponseWriter, r *http.Request, status int, code string) {
	if strings.HasPrefix(r.URL.Path, "/ui/") {
		writeUIError(w, r, status, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"code": code, "request_id": ingress.CorrelationID(r.Context()),
	}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	defer func() { _ = r.Body.Close() }()
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil || d.Decode(new(any)) != io.EOF {
		apiError(w, r, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func (a *application) login(w http.ResponseWriter, r *http.Request) {
	redirect, state, err := a.identity.Begin(r.Context(), r.RemoteAddr)
	if err != nil {
		if errors.Is(err, identity.ErrAdmissionLimited) {
			w.Header().Set("Retry-After", "60")
			apiError(w, r, http.StatusTooManyRequests, "auth_rate_limited")
			return
		}
		a.log.Error("OIDC login initiation failed", "request_id", ingress.CorrelationID(r.Context()))
		apiError(w, r, http.StatusServiceUnavailable, "auth_unavailable")
		return
	}
	a.setCookie(w, flowCookie, state, "/auth/callback", int(identity.FlowLifetime.Seconds()))
	http.Redirect(w, r, redirect, http.StatusFound)
}

func (a *application) callback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	stateValue, statePresent := query["state"]
	codeValue, codePresent := query["code"]
	stateCookie, cookieErr := r.Cookie(flowCookie)
	a.setCookie(w, flowCookie, "", "/auth/callback", -1)
	if !statePresent || len(stateValue) != 1 || !codePresent || len(codeValue) != 1 ||
		cookieErr != nil || stateCookie.Value == "" ||
		subtle.ConstantTimeCompare([]byte(stateValue[0]), []byte(stateCookie.Value)) != 1 {
		apiError(w, r, http.StatusBadRequest, "invalid_oidc_flow")
		return
	}
	token, _, err := a.identity.Complete(r.Context(), stateValue[0], codeValue[0], a.sessionToken(r))
	if err != nil {
		if errors.Is(err, identity.ErrInvalidFlow) {
			a.log.Warn("OIDC callback rejected", "request_id", ingress.CorrelationID(r.Context()))
			apiError(w, r, http.StatusBadRequest, "invalid_oidc_flow")
		} else {
			a.log.Error("OIDC callback failed", "request_id", ingress.CorrelationID(r.Context()))
			apiError(w, r, http.StatusServiceUnavailable, "auth_unavailable")
		}
		return
	}
	a.setCookie(w, a.sessionCookieName(), token, "/", int(identity.SessionLifetime.Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type principalKey struct{}

func principal(r *http.Request) identity.Principal {
	p, _ := r.Context().Value(principalKey{}).(identity.Principal)
	return p
}
func (a *application) withSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.requireSession(http.HandlerFunc(next)).ServeHTTP(w, r)
	}
}
func (a *application) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.identity.Current(r.Context(), a.sessionToken(r))
		if err != nil {
			if errors.Is(err, identity.ErrUnauthenticated) {
				apiError(w, r, http.StatusUnauthorized, "unauthenticated")
			} else {
				apiError(w, r, http.StatusServiceUnavailable, "storage_unavailable")
			}
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			supplied := r.Header.Get("X-CSRF-Token")
			if supplied == "" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
				r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
				if err := r.ParseForm(); err != nil {
					apiError(w, r, http.StatusBadRequest, "invalid_request")
					return
				}
				supplied = r.FormValue("csrf_token")
			}
			if !a.identity.CheckCSRF(p, supplied) {
				apiError(w, r, http.StatusForbidden, "csrf_invalid")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(contextWithPrincipal(r, p)))
	})
}
func contextWithPrincipal(r *http.Request, p identity.Principal) context.Context {
	return context.WithValue(r.Context(), principalKey{}, p)
}

func (a *application) logout(w http.ResponseWriter, r *http.Request) {
	if err := a.identity.Revoke(r.Context(), a.sessionToken(r)); err != nil {
		apiError(w, r, http.StatusServiceUnavailable, "storage_unavailable")
		return
	}
	a.setCookie(w, a.sessionCookieName(), "", "/", -1)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *application) me(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	writeJSON(w, http.StatusOK, map[string]any{"id": p.UserID, "email": p.Email,
		"issuer": p.Issuer, "csrf_token": a.identity.CSRF(p), "session_expires_at": p.ExpiresAt})
}

func pagination(r *http.Request) (int, string, error) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return 0, "", fmt.Errorf("invalid limit")
		}
	}
	return limit, r.URL.Query().Get("cursor"), nil
}
func appError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrForbidden):
		apiError(w, r, http.StatusForbidden, "forbidden")
	case errors.Is(err, identity.ErrInvalidCursor):
		apiError(w, r, http.StatusBadRequest, "invalid_cursor")
	case errors.Is(err, store.ErrNotFound):
		apiError(w, r, http.StatusNotFound, "not_found")
	default:
		apiError(w, r, http.StatusServiceUnavailable, "storage_unavailable")
	}
}
func (a *application) tenants(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pagination(r)
	if err != nil {
		apiError(w, r, 400, "invalid_pagination")
		return
	}
	items, next, err := a.identity.ListWorkspaces(r.Context(), principal(r), limit, cursor)
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}
func (a *application) createTenant(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 120 {
		apiError(w, r, 400, "invalid_name")
		return
	}
	id, err := a.identity.CreateWorkspace(r.Context(), principal(r), input.Name, ingress.CorrelationID(r.Context()))
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]string{"id": id, "name": input.Name, "role": "owner"})
}
func (a *application) tenant(w http.ResponseWriter, r *http.Request) {
	item, err := a.identity.GetWorkspace(r.Context(), principal(r), chi.URLParam(r, "tenant"))
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 200, item)
}
func (a *application) connections(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pagination(r)
	if err != nil {
		apiError(w, r, 400, "invalid_pagination")
		return
	}
	items, next, err := a.identity.ListConnections(r.Context(), principal(r), chi.URLParam(r, "tenant"), limit, cursor)
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}
func (a *application) runs(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pagination(r)
	if err != nil {
		apiError(w, r, 400, "invalid_pagination")
		return
	}
	items, next, err := a.identity.ListRunsFiltered(r.Context(), principal(r), chi.URLParam(r, "tenant"), limit, cursor, r.URL.Query().Get("status"))
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}
func (a *application) connection(w http.ResponseWriter, r *http.Request) {
	item, err := a.identity.GetConnection(r.Context(), principal(r),
		chi.URLParam(r, "tenant"), chi.URLParam(r, "connection"))
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 200, item)
}
func (a *application) members(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pagination(r)
	if err != nil {
		apiError(w, r, 400, "invalid_pagination")
		return
	}
	items, next, err := a.identity.ListMembers(r.Context(), principal(r), chi.URLParam(r, "tenant"), limit, cursor)
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}
func (a *application) grantMember(w http.ResponseWriter, r *http.Request) {
	var input struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	a.memberMutation(w, r, input.UserID, "grant", input.Role)
}
func (a *application) changeMemberRole(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	a.memberMutation(w, r, chi.URLParam(r, "user"), "role", input.Role)
}
func (a *application) removeMember(w http.ResponseWriter, r *http.Request) {
	a.memberMutation(w, r, chi.URLParam(r, "user"), "remove", "")
}
func (a *application) memberMutation(w http.ResponseWriter, r *http.Request, userID, action, role string) {
	if action != "remove" && role != "owner" && role != "operator" && role != "viewer" {
		apiError(w, r, 400, "invalid_role")
		return
	}
	err := a.identity.ChangeMembership(r.Context(), principal(r), chi.URLParam(r, "tenant"), userID, action, role, ingress.CorrelationID(r.Context()))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch {
			case strings.Contains(pgErr.Message, "last owner"):
				apiError(w, r, 409, "last_owner")
				return
			case strings.Contains(pgErr.Message, "denied"):
				apiError(w, r, 403, "forbidden")
				return
			case pgErr.Code == "23503":
				apiError(w, r, 404, "user_not_found")
				return
			}
		}
		if errors.As(err, &pgErr) && pgErr.Code == "P0001" {
			apiError(w, r, 400, "invalid_membership_change")
		} else {
			apiError(w, r, 503, "storage_unavailable")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var page = template.Must(template.New("page").Funcs(template.FuncMap{"tr": tr, "initial": brandInitial, "workspacePageCount": workspacePageCount}).Parse(`<!doctype html><html lang="{{.Lang}}" data-theme="{{.Theme}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="theme-color" content="#30212a"><title>LenoirDesk · {{if .User}}{{tr .Lang "workspaces"}}{{else}}{{tr .Lang "landing_signin_title"}}{{end}}</title><link rel="icon" type="image/svg+xml" href="/assets/lenoirdesk-mark.svg"><link rel="stylesheet" href="/assets/app.css"><script src="/assets/controls.js" defer></script></head><body><a class="skip" href="#content">{{tr .Lang "skip_to_content"}}</a>{{if .User}}<div class="selection-shell"><header class="selection-header"><a class="brand" href="/" aria-label="LenoirDesk"><img class="brand-mark" src="/assets/lenoirdesk-mark.svg" alt=""><span><span class="brand-word">Lenoir<span>Desk</span></span><span class="brand-caption">{{tr .Lang "brand_descriptor"}}</span></span></a><div class="selection-tools"><nav class="language-switch" aria-label="{{tr .Lang "language"}}"><a href="{{.EnglishURL}}" lang="en" hreflang="en" {{if eq .Lang "en"}}aria-current="true"{{end}}>EN</a><a href="{{.RussianURL}}" lang="ru" hreflang="ru" {{if eq .Lang "ru"}}aria-current="true"{{end}}>RU</a></nav><details class="theme-menu"><summary aria-label="{{tr .Lang "appearance"}}" title="{{tr .Lang "appearance"}}"><span aria-hidden="true">◐</span><span class="theme-current">{{tr .Lang .Theme}}</span></summary><div class="theme-popover" role="group" aria-label="{{tr .Lang "appearance"}}"><a href="{{.LightURL}}" {{if eq .Theme "light"}}aria-current="true"{{end}}>{{tr .Lang "light"}}</a><a href="{{.DarkURL}}" {{if eq .Theme "dark"}}aria-current="true"{{end}}>{{tr .Lang "dark"}}</a><a href="{{.SystemURL}}" {{if eq .Theme "system"}}aria-current="true"{{end}}>{{tr .Lang "system"}}</a></div></details><form method="post" action="/auth/logout"><input type="hidden" name="csrf_token" value="{{.CSRF}}"><button type="submit" class="secondary">{{tr .Lang "sign_out"}}</button></form></div></header><main id="content"><div class="selection-intro"><p class="eyebrow">{{tr .Lang "workspace_eyebrow"}}</p><h1>{{tr .Lang "workspace_title"}}</h1><p>{{tr .Lang "workspace_body"}}</p><p class="hint">{{tr .Lang "workspace_signed_in"}} {{.User.Email}}</p></div>{{if .Error}}<div class="alert error" role="alert">{{tr .Lang .Error}}</div>{{end}}{{if .Workspaces}}<p class="workspace-count">{{workspacePageCount .Lang (len .Workspaces)}}</p><div class="workspace-grid {{if gt (len .Workspaces) 6}}compact{{end}}">{{range .Workspaces}}<a class="workspace-card" href="/ui/tenants/{{.ID}}"><span class="workspace-initial" aria-hidden="true">{{initial .Name}}</span><strong>{{.Name}}</strong><small>{{tr $.Lang (printf "role_%s" .Role)}}</small><span class="open-link">{{tr $.Lang "workspace_open"}} →</span></a>{{end}}</div>{{else}}<div class="workspace-empty card"><span class="workspace-initial" aria-hidden="true">L</span><h2>{{tr .Lang "workspace_empty_title"}}</h2><p class="muted">{{tr .Lang "workspace_empty_body"}}</p></div>{{end}}{{if .NextCursor}}<p><a class="button secondary" href="/?cursor={{.NextCursor}}">{{tr .Lang "workspace_next"}} →</a></p>{{end}}<section class="workspace-create card"><h2>{{tr .Lang "workspace_create_title"}}</h2><form method="post" action="/ui/tenants"><input type="hidden" name="csrf_token" value="{{.CSRF}}"><label for="workspace-name">{{tr .Lang "workspace_name"}}<input id="workspace-name" name="name" required maxlength="120" autocomplete="organization"></label><button type="submit">{{tr .Lang "workspace_create_button"}}</button></form></section></main></div>{{else}}<main class="entry-shell" id="content"><section class="entry-story"><a class="brand" href="/" aria-label="LenoirDesk"><img class="brand-mark" src="/assets/lenoirdesk-mark.svg" alt=""><span><span class="brand-word">Lenoir<span>Desk</span></span><span class="brand-caption">{{tr .Lang "brand_descriptor"}}</span></span></a><p class="entry-kicker">{{tr .Lang "landing_eyebrow"}}</p><h1 class="entry-title">{{tr .Lang "landing_title"}}</h1><p class="entry-body">{{tr .Lang "landing_body"}}</p><div class="entry-signals"><span>{{tr .Lang "landing_signal_one"}}</span><span>{{tr .Lang "landing_signal_two"}}</span><span>{{tr .Lang "landing_signal_three"}}</span></div></section><section class="entry-panel" aria-labelledby="entry-title"><nav class="language-switch" aria-label="{{tr .Lang "language"}}"><a href="{{.EnglishURL}}" lang="en" hreflang="en" {{if eq .Lang "en"}}aria-current="true"{{end}}>EN</a><a href="{{.RussianURL}}" lang="ru" hreflang="ru" {{if eq .Lang "ru"}}aria-current="true"{{end}}>RU</a></nav><details class="theme-menu"><summary aria-label="{{tr .Lang "appearance"}}" title="{{tr .Lang "appearance"}}"><span aria-hidden="true">◐</span><span class="theme-current">{{tr .Lang .Theme}}</span></summary><div class="theme-popover" role="group" aria-label="{{tr .Lang "appearance"}}"><a href="{{.LightURL}}" {{if eq .Theme "light"}}aria-current="true"{{end}}>{{tr .Lang "light"}}</a><a href="{{.DarkURL}}" {{if eq .Theme "dark"}}aria-current="true"{{end}}>{{tr .Lang "dark"}}</a><a href="{{.SystemURL}}" {{if eq .Theme "system"}}aria-current="true"{{end}}>{{tr .Lang "system"}}</a></div></details><img class="brand-mark" src="/assets/lenoirdesk-mark.svg" alt=""><h2 id="entry-title">{{tr .Lang "landing_signin_title"}}</h2><p>{{tr .Lang "landing_signin_body"}}</p>{{if .Error}}<div class="alert error" role="alert">{{tr .Lang .Error}}</div>{{end}}<a class="button" href="/auth/login">{{tr .Lang "landing_signin_button"}} →</a><p class="footnote">{{tr .Lang "landing_signin_note"}}</p></section></main>{{end}}</body></html>`))

type pageData struct {
	Lang        string
	Theme       string
	EnglishURL  string
	RussianURL  string
	LightURL    string
	DarkURL     string
	SystemURL   string
	User        *identity.Principal
	CSRF        string
	Workspaces  []identity.Workspace
	NextCursor  string
	Workspace   *identity.Workspace
	Connections []identity.Connection
	Runs        []identity.Run
	Error       string
}

func (a *application) renderPage(w http.ResponseWriter, r *http.Request, status int, data pageData) {
	data.Lang = a.pageLanguage(w, r)
	data.Theme = a.pageTheme(w, r)
	data.EnglishURL = languageURL(r, "en")
	data.RussianURL = languageURL(r, "ru")
	data.LightURL = themeURL(r, "light")
	data.DarkURL = themeURL(r, "dark")
	data.SystemURL = themeURL(r, "system")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = page.Execute(w, data)
}
func (a *application) home(w http.ResponseWriter, r *http.Request) {
	p, err := a.identity.Current(r.Context(), a.sessionToken(r))
	if err != nil {
		if errors.Is(err, identity.ErrUnauthenticated) {
			a.renderPage(w, r, 200, pageData{})
		} else {
			a.renderPage(w, r, 503, pageData{Error: "Sign-in status unavailable"})
		}
		return
	}
	items, next, err := a.identity.ListWorkspaces(r.Context(), p, 20, r.URL.Query().Get("cursor"))
	if err != nil {
		status := 503
		message := "Workspaces unavailable"
		if errors.Is(err, identity.ErrInvalidCursor) {
			status, message = 400, "Invalid page cursor"
		}
		a.renderPage(w, r, status, pageData{User: &p, CSRF: a.identity.CSRF(p), Error: message})
		return
	}
	a.renderPage(w, r, 200, pageData{User: &p, CSRF: a.identity.CSRF(p), Workspaces: items, NextCursor: next})
}
func (a *application) createTenantUI(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := r.ParseForm(); err != nil {
		a.renderPage(w, r, 400, pageData{User: &p, CSRF: a.identity.CSRF(p), Error: "Invalid form"})
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 120 {
		a.renderPage(w, r, 400, pageData{User: &p, CSRF: a.identity.CSRF(p), Error: "Enter a workspace name (1–120 characters)"})
		return
	}
	id, err := a.identity.CreateWorkspace(r.Context(), p, name, ingress.CorrelationID(r.Context()))
	if err != nil {
		a.renderPage(w, r, 503, pageData{User: &p, CSRF: a.identity.CSRF(p), Error: "Workspace creation failed"})
		return
	}
	http.Redirect(w, r, "/ui/tenants/"+id, http.StatusSeeOther)
}
