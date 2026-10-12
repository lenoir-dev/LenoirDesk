package providerconn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const hubTokenURL = "https://api.hubapi.com/oauth/2026-03/token"
const slackTokenURL = "https://slack.com/api/oauth.v2.access"

var ErrInvalidProvider = errors.New("provider OAuth validation failed")
var ErrInvalidGrant = errors.New("provider OAuth grant invalid")
var ErrUncertainRefresh = errors.New("provider refresh outcome uncertain; reconnect required")

type Config struct {
	HubSpotID, HubSpotSecret, SlackID, SlackSecret string
}

type Client struct {
	Config Config
	HTTP   *http.Client
}

type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Rotation     string    `json:"rotation"`
}

func (c Client) credentials(provider string) (string, string, error) {
	switch provider {
	case "hubspot":
		if c.Config.HubSpotID != "" && c.Config.HubSpotSecret != "" {
			return c.Config.HubSpotID, c.Config.HubSpotSecret, nil
		}
	case "slack":
		if c.Config.SlackID != "" && c.Config.SlackSecret != "" {
			return c.Config.SlackID, c.Config.SlackSecret, nil
		}
	}
	return "", "", ErrInvalidProvider
}

func (c Client) AuthorizationURL(provider, state, redirect, verifier string) (string, error) {
	id, _, err := c.credentials(provider)
	if err != nil {
		return "", err
	}
	v := url.Values{"client_id": {id}, "redirect_uri": {redirect}, "state": {state}}
	switch provider {
	case "hubspot":
		v.Set("scope", "oauth crm.objects.contacts.read crm.objects.contacts.write")
		challenge := sha256.Sum256([]byte(verifier))
		v.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
		v.Set("code_challenge_method", "S256")
		return "https://app.hubspot.com/oauth/authorize?" + v.Encode(), nil
	case "slack":
		v.Set("scope", "chat:write")
		return "https://slack.com/oauth/v2/authorize?" + v.Encode(), nil
	}
	return "", ErrInvalidProvider
}

func (c Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return &http.Client{Transport: t, Timeout: 6 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (c Client) form(ctx context.Context, endpoint string, v url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(v.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		return ErrInvalidGrant
	}
	if resp.StatusCode != 200 {
		return ErrInvalidProvider
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(out)
}

type hubResponse struct {
	AccessToken, RefreshToken, TokenType string
	ExpiresIn                            int64
	HubID                                int64
	Scopes                               []string
}

// HubSpot's JSON uses snake_case; keep the wire representation explicit.
func (h *hubResponse) UnmarshalJSON(raw []byte) error {
	var v struct {
		AccessToken  string   `json:"access_token"`
		RefreshToken string   `json:"refresh_token"`
		TokenType    string   `json:"token_type"`
		ExpiresIn    int64    `json:"expires_in"`
		HubID        int64    `json:"hub_id"`
		Scopes       []string `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*h = hubResponse{v.AccessToken, v.RefreshToken, v.TokenType, v.ExpiresIn, v.HubID, v.Scopes}
	return nil
}

type slackResponse struct {
	OK           bool   `json:"ok"`
	Error        string `json:"error"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	Team         struct {
		ID string `json:"id"`
	} `json:"team"`
}

func hasScopes(granted []string, required ...string) bool {
	set := make(map[string]bool, len(granted))
	for _, s := range granted {
		set[strings.TrimSpace(s)] = true
	}
	for _, s := range required {
		if !set[s] {
			return false
		}
	}
	return true
}

func (c Client) Exchange(ctx context.Context, provider, code, redirect, verifier string) (Tokens, string, error) {
	id, secret, err := c.credentials(provider)
	if err != nil || code == "" || len(code) > 4096 {
		return Tokens{}, "", ErrInvalidProvider
	}
	v := url.Values{"client_id": {id}, "client_secret": {secret}, "code": {code}, "redirect_uri": {redirect}, "grant_type": {"authorization_code"}}
	switch provider {
	case "hubspot":
		v.Set("code_verifier", verifier)
		var out hubResponse
		if err = c.form(ctx, hubTokenURL, v, &out); err != nil {
			return Tokens{}, "", err
		}
		if out.AccessToken == "" || out.RefreshToken == "" || out.HubID <= 0 || out.ExpiresIn < 60 ||
			!hasScopes(out.Scopes, "crm.objects.contacts.read", "crm.objects.contacts.write") {
			return Tokens{}, "", ErrInvalidProvider
		}
		account := strconv.FormatInt(out.HubID, 10)
		if err = c.hubAccount(ctx, out.AccessToken, account); err != nil {
			return Tokens{}, "", err
		}
		return Tokens{out.AccessToken, out.RefreshToken, time.Now().Add(time.Duration(out.ExpiresIn) * time.Second), "rotating"}, account, nil
	case "slack":
		var out slackResponse
		if err = c.form(ctx, slackTokenURL, v, &out); err != nil {
			return Tokens{}, "", err
		}
		if !out.OK || out.AccessToken == "" || out.TokenType != "bot" || out.Team.ID == "" ||
			!hasScopes(strings.Split(out.Scope, ","), "chat:write") {
			return Tokens{}, "", ErrInvalidProvider
		}
		if err = c.slackIdentity(ctx, out.AccessToken, out.Team.ID); err != nil {
			return Tokens{}, "", err
		}
		t := Tokens{AccessToken: out.AccessToken, Rotation: "static"}
		if out.RefreshToken != "" && out.ExpiresIn >= 60 {
			t.RefreshToken, t.ExpiresAt, t.Rotation = out.RefreshToken, time.Now().Add(time.Duration(out.ExpiresIn)*time.Second), "rotating"
		}
		return t, out.Team.ID, nil
	}
	return Tokens{}, "", ErrInvalidProvider
}

func (c Client) getJSON(ctx context.Context, endpoint, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ErrInvalidProvider
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(out)
}

func (c Client) hubAccount(ctx context.Context, token, expected string) error {
	var out struct {
		PortalID    int64  `json:"portalId"`
		AccountType string `json:"accountType"`
	}
	if err := c.getJSON(ctx, "https://api.hubapi.com/account-info/v3/details", token, &out); err != nil {
		return err
	}
	if strconv.FormatInt(out.PortalID, 10) != expected || out.AccountType != "DEVELOPER_TEST" {
		return ErrInvalidProvider
	}
	return nil
}

func (c Client) slackIdentity(ctx context.Context, token, expected string) error {
	var out struct {
		OK     bool   `json:"ok"`
		TeamID string `json:"team_id"`
	}
	if err := c.getJSON(ctx, "https://slack.com/api/auth.test", token, &out); err != nil {
		return err
	}
	if !out.OK || out.TeamID != expected {
		return ErrInvalidProvider
	}
	return nil
}

func (c Client) Refresh(ctx context.Context, provider, account string, old Tokens) (Tokens, error) {
	id, secret, err := c.credentials(provider)
	if err != nil || old.Rotation != "rotating" || old.RefreshToken == "" {
		return Tokens{}, ErrInvalidProvider
	}
	v := url.Values{"client_id": {id}, "client_secret": {secret}, "grant_type": {"refresh_token"}, "refresh_token": {old.RefreshToken}}
	switch provider {
	case "hubspot":
		var out hubResponse
		if err = c.form(ctx, hubTokenURL, v, &out); err != nil {
			return Tokens{}, err
		}
		if strconv.FormatInt(out.HubID, 10) != account || out.AccessToken == "" || out.ExpiresIn < 60 ||
			!hasScopes(out.Scopes, "crm.objects.contacts.read", "crm.objects.contacts.write") {
			return Tokens{}, ErrInvalidProvider
		}
		if err = c.hubAccount(ctx, out.AccessToken, account); err != nil {
			return Tokens{}, err
		}
		refresh := out.RefreshToken
		if refresh == "" {
			refresh = old.RefreshToken // HubSpot can return only a new access token.
		}
		return Tokens{out.AccessToken, refresh, time.Now().Add(time.Duration(out.ExpiresIn) * time.Second), "rotating"}, nil
	case "slack":
		var out slackResponse
		if err = c.form(ctx, slackTokenURL, v, &out); err != nil {
			return Tokens{}, err
		}
		if !out.OK {
			if out.Error == "invalid_grant" || out.Error == "token_revoked" || out.Error == "invalid_refresh_token" {
				return Tokens{}, ErrInvalidGrant
			}
			return Tokens{}, ErrInvalidProvider
		}
		if out.Team.ID != account || out.AccessToken == "" || out.RefreshToken == "" || out.ExpiresIn < 60 ||
			!hasScopes(strings.Split(out.Scope, ","), "chat:write") {
			return Tokens{}, ErrInvalidProvider
		}
		if err = c.slackIdentity(ctx, out.AccessToken, account); err != nil {
			return Tokens{}, err
		}
		return Tokens{out.AccessToken, out.RefreshToken, time.Now().Add(time.Duration(out.ExpiresIn) * time.Second), "rotating"}, nil
	}
	return Tokens{}, fmt.Errorf("%w: unknown provider", ErrInvalidProvider)
}

func (c Client) Revoke(ctx context.Context, provider string, tokens Tokens) error {
	endpoint := ""
	v := url.Values{}
	switch provider {
	case "hubspot":
		id, secret, err := c.credentials(provider)
		if err != nil || tokens.RefreshToken == "" {
			return ErrInvalidProvider
		}
		endpoint = "https://api.hubapi.com/oauth/2026-03/token/revoke"
		v = url.Values{"client_id": {id}, "client_secret": {secret}, "token": {tokens.RefreshToken}, "token_type_hint": {"refresh_token"}}
	case "slack":
		if tokens.AccessToken == "" {
			return ErrInvalidProvider
		}
		endpoint = "https://slack.com/api/auth.revoke"
	default:
		return ErrInvalidProvider
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(v.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if provider == "slack" {
		req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ErrInvalidProvider
	}
	if provider == "slack" {
		var out struct {
			OK      bool `json:"ok"`
			Revoked bool `json:"revoked"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 16*1024)).Decode(&out) != nil || !out.OK || !out.Revoked {
			return ErrInvalidProvider
		}
		if tokens.RefreshToken != "" {
			return c.Revoke(ctx, "slack", Tokens{AccessToken: tokens.RefreshToken})
		}
	}
	return nil
}
