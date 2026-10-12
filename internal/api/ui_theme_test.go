package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lenoir-dev/LenoirDesk/internal/config"
	"github.com/lenoir-dev/LenoirDesk/internal/identity"
)

func TestThemePreferenceAndLocalLinks(t *testing.T) {
	a := &application{cfg: config.Config{PublicOrigin: "https://app.example.test"}}
	req := httptest.NewRequest(http.MethodGet, "https://app.example.test/ui/tenants/abc?theme=system&lang=ru", nil)
	w := httptest.NewRecorder()
	if got := a.pageTheme(w, req); got != "system" {
		t.Fatalf("theme=%q", got)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != themeCookie || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatal("theme cookie security attributes invalid")
	}
	if got := themeURL(req, "dark"); got != "/ui/tenants/abc?lang=ru&theme=dark" {
		t.Fatalf("theme route=%q", got)
	}
	withCookie := httptest.NewRequest(http.MethodGet, "/", nil)
	withCookie.AddCookie(&http.Cookie{Name: themeCookie, Value: "light"})
	if got := a.pageTheme(httptest.NewRecorder(), withCookie); got != "light" {
		t.Fatalf("persisted theme=%q", got)
	}
	if got := a.pageTheme(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/?theme=unexpected", nil)); got != "dark" {
		t.Fatalf("invalid theme fallback=%q", got)
	}
}

func TestTrendBarsAndProviderStatesAreTruthful(t *testing.T) {
	days := []identity.RunTrendDay{{Date: "2026-10-10", Count: 0}, {Date: "2026-10-11", Count: 4}}
	bars, total, peak := makeTrendBars(days)
	if total != 4 || peak != 4 || len(bars) != 2 || bars[0].Height != 0 || bars[1].Height != 132 {
		t.Fatalf("trend total=%d peak=%d bars=%+v", total, peak, bars)
	}
	cards := buildProviderCards([]identity.Connection{
		{Provider: "stripe", Status: "active", HealthStatus: "verified"},
		{Provider: "hubspot", Status: "active", HealthStatus: "invalid", AuthState: "blocked_auth"},
	})
	if len(cards) != 3 || cards[0].State != "verified" || cards[1].State != "attention" || cards[2].State != "disconnected" {
		t.Fatalf("provider states=%+v", cards)
	}
}
