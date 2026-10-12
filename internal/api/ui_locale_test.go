package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/config"
)

func TestLanguageChoiceIsLimitedAndKeepsLocalRoute(t *testing.T) {
	a := &application{cfg: config.Config{PublicOrigin: "https://app.example.test"}}
	req := httptest.NewRequest(http.MethodGet, "https://app.example.test/ui/tenants/abc/runs?status=dead&lang=ru", nil)
	w := httptest.NewRecorder()
	if got := a.pageLanguage(w, req); got != "ru" {
		t.Fatalf("language=%q", got)
	}
	if cookie := w.Result().Cookies()[0]; cookie.Name != languageCookie || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("language cookie security attributes invalid")
	}
	if got := languageURL(req, "en"); got != "/ui/tenants/abc/runs?lang=en&status=dead" {
		t.Fatalf("language route=%q", got)
	}
	if got := languageURL(req, "invalid"); !strings.HasPrefix(got, "/ui/tenants/abc/runs?lang=en") {
		t.Fatalf("unknown language route=%q", got)
	}
	withCookie := httptest.NewRequest(http.MethodGet, "/ui/tenants/abc", nil)
	withCookie.AddCookie(&http.Cookie{Name: languageCookie, Value: "ru"})
	if got := a.pageLanguage(httptest.NewRecorder(), withCookie); got != "ru" {
		t.Fatalf("persisted language=%q", got)
	}
}

func TestTouchedInterfaceCopyHasEnglishFallback(t *testing.T) {
	if got := tr("ru", "Overview"); got != "Обзор" {
		t.Fatalf("russian navigation=%q", got)
	}
	if got := tr("en", "Overview"); got != "Overview" {
		t.Fatalf("english navigation=%q", got)
	}
	if got := tr("ru", "provider-specific identifier"); got != "provider-specific identifier" {
		t.Fatalf("unreviewed content must remain explicit, got %q", got)
	}
}

func TestLocalizedOperationalTimeAndRussianCount(t *testing.T) {
	instant := time.Date(2026, 10, 11, 21, 3, 7, 0, time.FixedZone("offset", -7*3600))
	if got := displayTime("ru", instant, true); got != "12.10.2026 04:03:07 UTC" {
		t.Fatalf("RU UTC time=%q", got)
	}
	if got := displayTime("en", instant, false); got != "2026-10-12 04:03 UTC" {
		t.Fatalf("EN UTC time=%q", got)
	}
	if got := displayDay("ru", "2026-10-12"); got != "12.10.2026" {
		t.Fatalf("RU chart day=%q", got)
	}
	for count, want := range map[int64]string{0: "0 запусков", 1: "1 запуск", 2: "2 запуска", 5: "5 запусков", 11: "11 запусков", 21: "21 запуск", 24: "24 запуска", 111: "111 запусков", 1234: "1\u00a0234 запуска"} {
		if got := displayCount("ru", count, "run"); got != want {
			t.Errorf("%d: %q != %q", count, got, want)
		}
	}
	if got := displayCount("en", 1234, "run"); got != "1,234 runs" {
		t.Fatalf("EN count=%q", got)
	}
}

func TestTimelineOutcomeDoesNotExposeRawStatusInRussian(t *testing.T) {
	for code, want := range map[string]string{
		"succeeded": "Успешно", "unknown": "Результат неизвестен",
		"confirm_unknown":     "Доставку подтвердил оператор",
		"crm_projection · #2": "Контакт HubSpot · #2",
	} {
		if got := timelineNote("ru", code); got != want {
			t.Errorf("%q: %q != %q", code, got, want)
		}
	}
}

func TestRunAuditAndDecisionsHaveReadableLabels(t *testing.T) {
	if got := auditLabel("ru", "business.manual_retry", "success"); got != "Оператор запросил повтор CRM · выполнено" {
		t.Fatalf("localized audit=%q", got)
	}
	if got := timelineNote("ru", "leave_unknown"); got != "Неизвестная доставка оставлена без решения" {
		t.Fatalf("localized decision=%q", got)
	}
}

func TestWorkspacePageCountRussianPlural(t *testing.T) {
	for count, want := range map[int]string{1: "1 область на этой странице", 2: "2 области на этой странице", 5: "5 областей на этой странице", 11: "11 областей на этой странице", 21: "21 область на этой странице"} {
		if got := workspacePageCount("ru", count); got != want {
			t.Errorf("%d: %q != %q", count, got, want)
		}
	}
}

func TestLocalizedOperationalDurations(t *testing.T) {
	for _, example := range []struct {
		lang    string
		seconds int64
		want    string
	}{
		{"ru", 0, "0 секунд"},
		{"ru", 21, "21 секунда"},
		{"ru", 85, "1 минута 25 секунд"},
		{"ru", 3600, "1 час"},
		{"ru", 11*86400 + 2*3600, "11 дней 2 часа"},
		{"en", 85, "1 minute 25 seconds"},
	} {
		if got := displayDuration(example.lang, example.seconds); got != example.want {
			t.Errorf("%s/%d: %q != %q", example.lang, example.seconds, got, example.want)
		}
	}
}
