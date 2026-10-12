package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMission05BrowserErrorsKeepAPIErrorsJSON(t *testing.T) {
	for _, tc := range []struct {
		path, lang, code, contentType, marker string
		status                                int
	}{
		{"/ui/tenants/example/runs?lang=ru&theme=light", "ru", "unauthenticated", "text/html", "Сеанс завершён", http.StatusUnauthorized},
		{"/ui/tenants/example/connections?lang=en", "en", "forbidden", "text/html", "Access denied", http.StatusForbidden},
		{"/ui/tenants/example/reconciliation?lang=ru", "ru", "csrf_invalid", "text/html", "Запрос не подтверждён", http.StatusForbidden},
		{"/ui/tenants/example/runs/example/mapping?lang=ru", "ru", "mapping_conflict", "text/html", "Связывание контакта заблокировано", http.StatusConflict},
		{"/ui/tenants/example/runs/example?lang=en", "en", "recovery_conflict", "text/html", "Recovery decision not applied", http.StatusConflict},
		{"/ui/tenants/example/connections?lang=en", "en", "setup_unavailable", "text/html", "Provider check unavailable", http.StatusServiceUnavailable},
		{"/v1/tenants/example/runs", "en", "forbidden", "application/json", `"code":"forbidden"`, http.StatusForbidden},
	} {
		t.Run(tc.code+tc.lang, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			apiError(w, r, tc.status, tc.code)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
				t.Fatalf("content type = %q, want %q", got, tc.contentType)
			}
			if !strings.Contains(w.Body.String(), tc.marker) || strings.Contains(w.Body.String(), "client_secret") {
				t.Fatal("error response lost safe localized meaning")
			}
		})
	}
}
