package api

import (
	"crypto/subtle"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/lenoir-dev/LenoirDesk/internal/identity"
	"github.com/lenoir-dev/LenoirDesk/internal/ingress"
	"github.com/lenoir-dev/LenoirDesk/internal/providerconn"
	"github.com/lenoir-dev/LenoirDesk/internal/telemetry"
)

func providerFlowCookie(provider string) string { return "relaydesk_" + provider + "_oauth_state" }

type providerPageData struct {
	Lang, Theme, Provider, URL, ID string
}

var providerStartPage = template.Must(template.New("provider-start").Funcs(template.FuncMap{"tr": tr}).Parse(`<!doctype html><html lang="{{.Lang}}" data-theme="{{.Theme}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{tr .Lang "provider_connect_title"}} · LenoirDesk</title><link rel="icon" type="image/svg+xml" href="/assets/lenoirdesk-mark.svg"><link rel="stylesheet" href="/assets/app.css"></head><body><main class="flow-shell"><a class="brand" href="/"><img class="brand-mark" src="/assets/lenoirdesk-mark.svg" alt=""><span class="brand-word">Lenoir<span>Desk</span></span></a><section class="entry-panel"><p class="eyebrow">{{.Provider}}</p><h1>{{tr .Lang "provider_connect_title"}}</h1><p>{{tr .Lang "provider_connect_body"}}</p><a class="button" href="{{.URL}}">{{tr .Lang "provider_connect_button"}} →</a></section></main></body></html>`))
var providerCompletePage = template.Must(template.New("provider-complete").Funcs(template.FuncMap{"tr": tr}).Parse(`<!doctype html><html lang="{{.Lang}}" data-theme="{{.Theme}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{tr .Lang "provider_connected_title"}} · LenoirDesk</title><link rel="icon" type="image/svg+xml" href="/assets/lenoirdesk-mark.svg"><link rel="stylesheet" href="/assets/app.css"></head><body><main class="flow-shell"><a class="brand" href="/"><img class="brand-mark" src="/assets/lenoirdesk-mark.svg" alt=""><span class="brand-word">Lenoir<span>Desk</span></span></a><section class="entry-panel"><p class="eyebrow">{{.Provider}}</p><h1>{{tr .Lang "provider_connected_title"}}</h1><p>{{tr .Lang "provider_connected_body"}} <code>{{.ID}}</code></p><a class="button" href="/">{{tr .Lang "provider_return"}} →</a></section></main></body></html>`))

func (a *application) beginProviderOAuth(w http.ResponseWriter, r *http.Request) {
	if a.cfg.DemoMode {
		apiError(w, r, http.StatusForbidden, "demo_provider_changes_disabled")
		return
	}
	provider := chi.URLParam(r, "provider")
	var input struct {
		ExpectedAccountID string `json:"expected_account_id"`
		ReconnectID       string `json:"reconnect_id"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	redirect, state, err := a.identity.BeginProvider(r.Context(), principal(r), chi.URLParam(r, "tenant"), provider,
		input.ExpectedAccountID, input.ReconnectID)
	if err != nil {
		if errors.Is(err, identity.ErrProviderFlow) {
			apiError(w, r, http.StatusBadRequest, "invalid_provider_flow")
		} else if errors.Is(err, identity.ErrProviderAdmissionLimited) {
			w.Header().Set("Retry-After", "60")
			apiError(w, r, http.StatusTooManyRequests, "provider_oauth_rate_limited")
		} else {
			appError(w, r, err)
		}
		return
	}
	a.setCookie(w, providerFlowCookie(provider), state, "/oauth/"+provider+"/callback", int(identity.ProviderFlowLifetime.Seconds()))
	writeJSON(w, http.StatusCreated, map[string]string{"authorization_url": redirect})
}

// beginProviderOAuthUI starts the same owner-scoped flow from a browser form.
// The session middleware enforces the form's CSRF token before this handler.
func (a *application) beginProviderOAuthUI(w http.ResponseWriter, r *http.Request) {
	if a.cfg.DemoMode {
		apiError(w, r, http.StatusForbidden, "demo_provider_changes_disabled")
		return
	}
	provider := chi.URLParam(r, "provider")
	if provider != "hubspot" && provider != "slack" {
		apiError(w, r, http.StatusNotFound, "not_found")
		return
	}
	if err := r.ParseForm(); err != nil {
		apiError(w, r, http.StatusBadRequest, "invalid_provider_flow")
		return
	}
	redirect, state, err := a.identity.BeginProvider(r.Context(), principal(r), chi.URLParam(r, "tenant"), provider,
		r.FormValue("expected_account_id"), r.FormValue("reconnect_id"))
	if err != nil {
		if errors.Is(err, identity.ErrProviderFlow) {
			apiError(w, r, http.StatusBadRequest, "invalid_provider_flow")
		} else if errors.Is(err, identity.ErrProviderAdmissionLimited) {
			w.Header().Set("Retry-After", "60")
			apiError(w, r, http.StatusTooManyRequests, "provider_oauth_rate_limited")
		} else {
			appError(w, r, err)
		}
		return
	}
	a.setCookie(w, providerFlowCookie(provider), state, "/oauth/"+provider+"/callback", int(identity.ProviderFlowLifetime.Seconds()))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := providerStartPage.Execute(w, providerPageData{Lang: a.pageLanguage(w, r), Theme: a.pageTheme(w, r), Provider: provider, URL: redirect}); err != nil {
		a.log.Error("provider start page failed", "provider", provider)
	}
}

func (a *application) providerOAuthCallback(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if provider != "hubspot" && provider != "slack" {
		apiError(w, r, http.StatusNotFound, "not_found")
		return
	}
	query := r.URL.Query()
	states, stateOK := query["state"]
	codes, codeOK := query["code"]
	cookie, cookieErr := r.Cookie(providerFlowCookie(provider))
	a.setCookie(w, providerFlowCookie(provider), "", "/oauth/"+provider+"/callback", -1)
	if !stateOK || !codeOK || len(states) != 1 || len(codes) != 1 || cookieErr != nil ||
		cookie.Value == "" || subtle.ConstantTimeCompare([]byte(states[0]), []byte(cookie.Value)) != 1 ||
		len(states[0]) != 43 || codes[0] == "" || len(codes[0]) > 4096 {
		apiError(w, r, http.StatusBadRequest, "invalid_provider_flow")
		return
	}
	p, err := a.identity.Current(r.Context(), a.sessionToken(r))
	if err != nil {
		apiError(w, r, http.StatusUnauthorized, "unauthenticated")
		return
	}
	id, err := a.identity.CompleteProvider(r.Context(), p, provider, states[0], codes[0], ingress.CorrelationID(r.Context()))
	if err != nil {
		telemetry.Event(r.Context(), "oauth_callback_failure", provider)
		a.log.Warn("provider OAuth callback rejected", "provider", provider, "request_id", ingress.CorrelationID(r.Context()))
		if errors.Is(err, identity.ErrProviderFlow) || errors.Is(err, identity.ErrProviderMismatch) ||
			errors.Is(err, providerconn.ErrInvalidProvider) || errors.Is(err, providerconn.ErrInvalidGrant) {
			apiError(w, r, http.StatusBadRequest, "invalid_provider_flow")
		} else {
			apiError(w, r, http.StatusServiceUnavailable, "provider_connection_unavailable")
		}
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := providerCompletePage.Execute(w, providerPageData{Lang: a.pageLanguage(w, r), Theme: a.pageTheme(w, r), Provider: provider, ID: id}); err != nil {
			a.log.Error("provider completion page failed", "provider", provider)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"connection_id": id, "status": "verified"})
}

func (a *application) disconnectProvider(w http.ResponseWriter, r *http.Request) {
	if a.cfg.DemoMode {
		apiError(w, r, http.StatusForbidden, "demo_provider_changes_disabled")
		return
	}
	id := chi.URLParam(r, "connection")
	c, err := a.identity.GetConnection(r.Context(), principal(r), chi.URLParam(r, "tenant"), id)
	if err != nil {
		appError(w, r, err)
		return
	}
	if c.Provider == "stripe" {
		changed, disconnectErr := a.identity.DisconnectStripeTest(r.Context(), principal(r), chi.URLParam(r, "tenant"), id, ingress.CorrelationID(r.Context()))
		if disconnectErr != nil {
			appError(w, r, disconnectErr)
			return
		}
		if !changed {
			apiError(w, r, http.StatusNotFound, "not_found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	changed, revoked, err := a.identity.DisconnectProvider(r.Context(), principal(r), chi.URLParam(r, "tenant"), id,
		ingress.CorrelationID(r.Context()))
	if err != nil {
		appError(w, r, err)
		return
	}
	if !changed {
		apiError(w, r, http.StatusNotFound, "not_found")
		return
	}
	if !revoked {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "disconnected", "provider_revocation": "unconfirmed"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
