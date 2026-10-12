package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/identity"
)

func TestHistoricalPickerKeepsNativeFallbackAndLocalAssets(t *testing.T) {
	for _, lang := range []string{"en", "ru"} {
		v := uiView{Tenant: "synthetic", Lang: lang, Theme: "dark", Screen: "Reconciliation",
			Workspace: identity.Workspace{Name: "Synthetic", Role: "owner"},
			User:      identity.Principal{Email: "owner@example.invalid"}, CanOperate: true,
			HasReconcileConnection: true}
		var rendered bytes.Buffer
		if err := uiTemplates.ExecuteTemplate(&rendered, "reconciliation", v); err != nil {
			t.Fatal(err)
		}
		page := rendered.String()
		for _, marker := range []string{`type="datetime-local" step="1"`, `/assets/vanilla-calendar.js`,
			`/assets/vanilla-calendar.css`, `/assets/historical-picker.js`, `data-picker-invalid=`} {
			if !strings.Contains(page, marker) {
				t.Fatalf("%s reconciliation missing %q", lang, marker)
			}
		}
		for _, marker := range []string{tr(lang, "reconcile_date_utc"), tr(lang, "reconcile_time_utc")} {
			if !strings.Contains(page, marker) {
				t.Fatalf("%s picker missing %q", lang, marker)
			}
		}
	}
	for _, name := range []string{"vanilla-calendar.js", "vanilla-calendar.css", "historical-picker.js", "historical-picker.css"} {
		r := httptest.NewRequest(http.MethodGet, "/assets/"+name, nil)
		w := httptest.NewRecorder()
		serveUIAsset(w, r)
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Fatalf("asset %s: HTTP %d, size %d", name, w.Code, w.Body.Len())
		}
	}
}

func TestHistoricalUTCParsingIncludesLeapDayAndSeconds(t *testing.T) {
	for _, value := range []string{"2024-02-29T23:59:58", "2026-03-08T02:30:47", "2026-11-01T01:30:59"} {
		got, err := parseHistoricalTime(value)
		if err != nil || got.Location() != time.UTC || got.Format("2006-01-02T15:04:05") != value {
			t.Fatalf("%s changed UTC instant: %v, %v", value, got, err)
		}
	}
	if _, err := parseHistoricalTime("2025-02-29T23:59:58"); err == nil {
		t.Fatal("invalid leap day accepted")
	}
}
