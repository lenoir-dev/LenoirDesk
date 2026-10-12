package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/identity"
)

func TestMission03PriceFormParsingAndValidation(t *testing.T) {
	base := url.Values{"stripe": {"stripe-id"}, "hubspot": {"hubspot-id"}, "slack": {"slack-id"},
		"channel": {"C123456"}, "policy": {"explicit_only"}}
	for _, tc := range []struct {
		name, input, errorKey string
		want                  []string
	}{
		{"multiline and commas", " price_one\n\nprice_two, price_three \n", "", []string{"price_one", "price_two", "price_three"}},
		{"duplicate", "price_one\nprice_one", "automation_prices_duplicate_error", nil},
		{"malformed", "price_one, bad", "automation_prices_format_error", nil},
		{"empty comma item", "price_one,,price_two", "automation_prices_format_error", nil},
		{"missing", " \n ", "automation_prices_count_error", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{}
			for key, values := range base {
				form[key] = append([]string(nil), values...)
			}
			form.Set("prices", tc.input)
			r := httptest.NewRequest(http.MethodPost, "/billing-sync/preview", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			cfg := billingForm(r)
			if got := automationFieldErrors(cfg)["prices"]; got != tc.errorKey {
				t.Fatalf("field error %q, want %q", got, tc.errorKey)
			}
			if tc.errorKey == "" {
				if err := cfg.Validate(); err != nil {
					t.Fatal(err)
				}
				if strings.Join(cfg.TrackedPriceIDs, ",") != strings.Join(tc.want, ",") {
					t.Fatalf("parsed prices: %v", cfg.TrackedPriceIDs)
				}
			}
		})
	}
}

func TestMission03TemplatesAndLocalization(t *testing.T) {
	v := uiView{Tenant: "synthetic", Lang: "ru", Theme: "dark", Screen: "Automation", NextRevision: 1,
		Workspace: identity.Workspace{Name: "Тестовая область", Role: "owner"},
		User:      identity.Principal{Email: "demo@example.invalid"}, CanOperate: true,
		AutomationDraft: automationDraft{Policy: "explicit_only"}}
	var rendered bytes.Buffer
	if err := uiTemplates.ExecuteTemplate(&rendered, "automation", v); err != nil {
		t.Fatal(err)
	}
	page := rendered.String()
	for _, marker := range []string{"Синхронизация подписок", "Проверенные подключения", "Проверить настройку", "data-required-message=", "/assets/controls.js"} {
		if !strings.Contains(page, marker) {
			t.Fatalf("Automation missing %q", marker)
		}
	}
	if strings.Contains(page, "Activate this revision") {
		t.Fatal("editing page must not activate a revision")
	}
	v.Screen = "Runs"
	v.RunCounts = map[string]int64{"unknown": 1, "succeeded": 2}
	v.RunTotal = 3
	v.Filters = runFilters
	v.Runs = []identity.Run{{ID: "run-synthetic", Status: "unknown", EventType: "invoice.paid", EventID: "evt_synthetic", Step: "slack_notification", CreatedAt: time.Now().UTC()}}
	rendered.Reset()
	if err := uiTemplates.ExecuteTemplate(&rendered, "runs", v); err != nil {
		t.Fatal(err)
	}
	page = rendered.String()
	for _, marker := range []string{"История выполнения", "Результат неизвестен", "Уведомление Slack", "status=unknown", "evt_synthetic"} {
		if !strings.Contains(page, marker) {
			t.Fatalf("Runs missing %q", marker)
		}
	}
}

func TestMission03RunFilterCounts(t *testing.T) {
	counts := map[string]int64{"succeeded": 5, "pending": 2, "running": 1, "blocked_auth": 3, "blocked_config": 4, "unknown": 1}
	for _, tc := range []struct {
		filter string
		want   int64
	}{{"", 16}, {"in_progress", 3}, {"blocked", 7}, {"unknown", 1}, {"dead", 0}} {
		if got := runFilterCount(counts, tc.filter); got != tc.want {
			t.Fatalf("filter %q = %d, want %d", tc.filter, got, tc.want)
		}
	}
}
