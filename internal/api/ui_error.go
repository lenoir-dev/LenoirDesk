package api

import (
	"html/template"
	"net/http"
)

type uiErrorData struct {
	Lang, Theme, Title, Body, Action, Href string
}

var uiErrorTemplate = template.Must(template.New("ui_error").Funcs(template.FuncMap{"tr": tr}).Parse(`<!doctype html><html lang="{{.Lang}}" data-theme="{{.Theme}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="theme-color" content="#30212a"><title>{{tr .Lang .Title}} · LenoirDesk</title><link rel="icon" type="image/svg+xml" href="/assets/lenoirdesk-mark.svg"><link rel="stylesheet" href="/assets/app.css"></head><body><a class="skip" href="#content">{{tr .Lang "skip_to_content"}}</a><main class="flow-shell" id="content"><a class="brand" href="/" aria-label="LenoirDesk"><img class="brand-mark" src="/assets/lenoirdesk-mark.svg" alt=""><span class="brand-word">Lenoir<span>Desk</span></span></a><section class="entry-panel" role="alert"><p class="eyebrow">LenoirDesk</p><h1>{{tr .Lang .Title}}</h1><p>{{tr .Lang .Body}}</p><a class="button secondary" href="{{.Href}}">{{tr .Lang .Action}} →</a></section></main></body></html>`))

func writeUIError(w http.ResponseWriter, r *http.Request, status int, code string) {
	lang := "en"
	if selected := r.URL.Query().Get("lang"); selected == "ru" {
		lang = "ru"
	} else if selected != "en" {
		if cookie, err := r.Cookie(languageCookie); err == nil && cookie.Value == "ru" {
			lang = "ru"
		}
	}
	theme := "dark"
	if selected := r.URL.Query().Get("theme"); validTheme(selected) {
		theme = selected
	} else if cookie, err := r.Cookie(themeCookie); err == nil && validTheme(cookie.Value) {
		theme = cookie.Value
	}
	data := uiErrorData{Lang: lang, Theme: theme, Title: "ui_error_generic_title",
		Body: "ui_error_generic_body", Action: "ui_error_workspace_action", Href: "/"}
	switch code {
	case "unauthenticated":
		data.Title, data.Body, data.Action, data.Href = "ui_error_session_title", "ui_error_session_body", "ui_error_signin_action", "/auth/login"
	case "forbidden":
		data.Title, data.Body = "ui_error_forbidden_title", "ui_error_forbidden_body"
	case "csrf_invalid":
		data.Title, data.Body = "ui_error_csrf_title", "ui_error_csrf_body"
	case "not_found":
		data.Title, data.Body = "ui_error_not_found_title", "ui_error_not_found_body"
	case "storage_unavailable":
		data.Title, data.Body = "ui_error_unavailable_title", "ui_error_unavailable_body"
	case "setup_unavailable", "provider_connection_unavailable", "contact_preview_unavailable":
		data.Title, data.Body = "ui_error_provider_title", "ui_error_provider_body"
	case "mapping_unavailable", "mapping_conflict":
		data.Title, data.Body = "ui_error_mapping_title", "ui_error_mapping_body"
	case "recovery_conflict":
		data.Title, data.Body = "ui_error_recovery_title", "ui_error_recovery_body"
	case "invalid_request", "invalid_cursor", "invalid_reconciliation_bounds", "invalid_reason", "invalid_billing_configuration", "invalid_recovery_decision", "invalid_pagination", "invalid_name":
		data.Title, data.Body = "ui_error_input_title", "ui_error_input_body"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = uiErrorTemplate.Execute(w, data)
}
