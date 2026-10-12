package providerconn

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSlackExchangeStaticRotatingAndAccountValidation(t *testing.T) {
	for _, tc := range []struct {
		name, tokenBody, authBody, wantRotation string
		wantErr                                 bool
	}{
		{"static", `{"ok":true,"access_token":"xoxb-test","token_type":"bot","scope":"chat:write","team":{"id":"T123"}}`, `{"ok":true,"team_id":"T123"}`, "static", false},
		{"rotating", `{"ok":true,"access_token":"xoxe-test","refresh_token":"xoxe-refresh","expires_in":43200,"token_type":"bot","scope":"chat:write","team":{"id":"T123"}}`, `{"ok":true,"team_id":"T123"}`, "rotating", false},
		{"missing scope", `{"ok":true,"access_token":"xoxb-test","token_type":"bot","scope":"channels:read","team":{"id":"T123"}}`, `{"ok":true,"team_id":"T123"}`, "", true},
		{"wrong workspace", `{"ok":true,"access_token":"xoxb-test","token_type":"bot","scope":"chat:write","team":{"id":"T123"}}`, `{"ok":true,"team_id":"T999"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Client{Config: Config{SlackID: "client", SlackSecret: "secret"}}
			c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := tc.authBody
				if strings.HasSuffix(r.URL.Path, "/oauth.v2.access") {
					body = tc.tokenBody
				} else if r.URL.Path != "/api/auth.test" {
					t.Fatalf("unexpected endpoint %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			tokens, account, err := c.Exchange(context.Background(), "slack", "code", "http://127.0.0.1/callback", "")
			if tc.wantErr {
				if err == nil {
					t.Fatal("invalid Slack grant accepted")
				}
				return
			}
			if err != nil || account != "T123" || tokens.Rotation != tc.wantRotation {
				t.Fatalf("unexpected exchange: account=%s rotation=%s err=%v", account, tokens.Rotation, err)
			}
		})
	}
}
