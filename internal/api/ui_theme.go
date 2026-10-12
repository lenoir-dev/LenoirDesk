package api

import (
	"net/http"
	"net/url"
	"strings"
)

const themeCookie = "lenoirdesk_theme"

// Theme is presentation-only. Rendering the selected mode on the HTML root
// avoids a light-theme flash before styles load; CSS resolves System locally.
func (a *application) pageTheme(w http.ResponseWriter, r *http.Request) string {
	if selected := r.URL.Query().Get("theme"); validTheme(selected) {
		a.setCookie(w, themeCookie, selected, "/", 365*24*60*60)
		return selected
	}
	if cookie, err := r.Cookie(themeCookie); err == nil && validTheme(cookie.Value) {
		return cookie.Value
	}
	return "dark"
}

func validTheme(theme string) bool {
	return theme == "light" || theme == "dark" || theme == "system"
}

func themeURL(r *http.Request, theme string) string {
	if !validTheme(theme) {
		theme = "dark"
	}
	query := r.URL.Query()
	query.Set("theme", theme)
	path := r.URL.Path
	if path == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		path = "/"
	}
	return (&url.URL{Path: path, RawQuery: query.Encode()}).String()
}
