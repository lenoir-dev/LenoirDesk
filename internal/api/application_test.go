package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lenoir-dev/LenoirDesk/internal/config"
)

func TestProductionCookiesAreHostBound(t *testing.T) {
	a := application{cfg: config.Config{Environment: "production", PublicOrigin: "https://relaydesk.example"}}
	if a.sessionCookieName() != "__Host-relaydesk" {
		t.Fatal("production session cookie has no __Host prefix")
	}
	w := httptest.NewRecorder()
	a.setCookie(w, a.sessionCookieName(), "opaque", "/", 60)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies=%d", len(cookies))
	}
	c := cookies[0]
	if !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unsafe production cookie attributes: secure=%v httponly=%v path=%q domain=%q samesite=%v",
			c.Secure, c.HttpOnly, c.Path, c.Domain, c.SameSite)
	}
}
