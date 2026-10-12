//go:build integration

package integration

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func localizedPage(t *testing.T, app *testApp, path string, cookies ...*http.Cookie) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, app.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	resp, err := app.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d read=%v", path, resp.StatusCode, err)
	}
	return resp, string(body)
}

func TestLenoirDeskLanguageAndShellJourney(t *testing.T) {
	app := newTestApp(t, newFakeOIDC(t))
	landing, html := localizedPage(t, app, "/?lang=ru")
	for _, fragment := range []string{`lang="ru"`, "LenoirDesk", "Каждое бизнес-событие под контролем.", `href="/auth/login"`, `href="/?lang=en"`} {
		if !strings.Contains(html, fragment) {
			t.Fatalf("localized entry missing %q", fragment)
		}
	}
	if strings.Contains(html, "RelayDesk") {
		t.Fatal("legacy brand visible on entry")
	}
	var language *http.Cookie
	for _, cookie := range landing.Cookies() {
		if cookie.Name == "lenoirdesk_lang" {
			language = cookie
		}
	}
	if language == nil || language.Value != "ru" || !language.HttpOnly {
		t.Fatal("Russian preference cookie missing or weak")
	}
	_, html = localizedPage(t, app, "/", language)
	if !strings.Contains(html, "Продолжить вход") {
		t.Fatal("entry preference did not persist")
	}

	owner := login(t, app, "design-owner-"+randomHex(4), nil)
	_, csrf := me(t, app, owner)
	_, html = localizedPage(t, app, "/", owner, language)
	if !strings.Contains(html, "Создайте место для первого сценария") || !strings.Contains(html, "Создать область") {
		t.Fatal("empty workspace selection was not localized")
	}
	tenant := createWorkspace(t, app, owner, csrf, "Design workspace")
	_, html = localizedPage(t, app, "/", owner, language)
	if !strings.Contains(html, tenant) || !strings.Contains(html, "Открыть область") {
		t.Fatal("workspace card or navigation missing")
	}
	_, html = localizedPage(t, app, "/ui/tenants/"+tenant, owner, language)
	for _, fragment := range []string{`lang="ru"`, "LenoirDesk", "Обзор рабочей области", "Подключения", "Автоматизация", "Сверка", "Начало работы", "Выйти", `href="/ui/tenants/` + tenant + `/runs"`} {
		if !strings.Contains(html, fragment) {
			t.Fatalf("localized shell missing %q", fragment)
		}
	}
	if strings.Contains(html, "RelayDesk") {
		t.Fatal("legacy brand visible on overview")
	}
	for _, section := range []string{"connections", "billing-sync", "runs", "reconciliation"} {
		_, html = localizedPage(t, app, "/ui/tenants/"+tenant+"/"+section, owner, language)
		if !strings.Contains(html, `lang="ru"`) || !strings.Contains(html, "LenoirDesk") {
			t.Fatalf("section %s lost localized shell", section)
		}
	}
	english, html := localizedPage(t, app, "/ui/tenants/"+tenant+"?lang=en", owner, language)
	if !strings.Contains(html, `lang="en"`) || !strings.Contains(html, "Workspace overview") {
		t.Fatal("English switch failed")
	}
	if english.Header.Get("Set-Cookie") == "" {
		t.Fatal("English preference was not persisted")
	}
	css, err := app.client.Get(app.server.URL + "/assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	_ = css.Body.Close()
	if css.StatusCode != http.StatusOK || !strings.HasPrefix(css.Header.Get("Content-Type"), "text/css") || !strings.Contains(css.Header.Get("Content-Security-Policy"), "style-src 'self'") {
		t.Fatal("local stylesheet or strict CSP missing")
	}
	logo, err := app.client.Get(app.server.URL + "/assets/lenoirdesk-mark.svg")
	if err != nil {
		t.Fatal(err)
	}
	_ = logo.Body.Close()
	if logo.StatusCode != http.StatusOK || logo.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatal("local brand mark missing")
	}
	logout := postUIForm(t, app, owner, "/auth/logout", url.Values{"csrf_token": {csrf}})
	_ = logout.Body.Close()
	if logout.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout status=%d", logout.StatusCode)
	}
	_, html = localizedPage(t, app, "/", language)
	if !strings.Contains(html, "Продолжить вход") {
		t.Fatal("post-logout entry or language preference failed")
	}
}
