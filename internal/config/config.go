package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Config struct {
	Environment         string
	HTTPAddr            string
	DatabaseURL         string
	AppDatabaseURL      string
	AuthDatabaseURL     string
	NATSURL             string
	RedisURL            string
	EncryptionKey       []byte
	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
	PublicOrigin        string
	OIDCIssuer          string
	OIDCClientID        string
	OIDCClientSecret    string
	OIDCRedirectURL     string
	OIDCBackchannelAddr string
	ProviderMode        string
	DemoMode            bool
	StripeAPIURL        string
	HubSpotAPIURL       string
	SlackAPIURL         string
	HubSpotClientID     string
	HubSpotClientSecret string
	SlackClientID       string
	SlackClientSecret   string
}

func Load() (Config, error) {
	c := Config{
		Environment:         os.Getenv("RELAYDESK_ENV"),
		HTTPAddr:            os.Getenv("RELAYDESK_HTTP_ADDR"),
		DatabaseURL:         os.Getenv("RELAYDESK_DATABASE_URL"),
		AppDatabaseURL:      os.Getenv("RELAYDESK_APP_DATABASE_URL"),
		AuthDatabaseURL:     os.Getenv("RELAYDESK_AUTH_DATABASE_URL"),
		NATSURL:             os.Getenv("RELAYDESK_NATS_URL"),
		RedisURL:            os.Getenv("RELAYDESK_REDIS_URL"),
		ReadTimeout:         5 * time.Second,
		WriteTimeout:        10 * time.Second,
		PublicOrigin:        os.Getenv("RELAYDESK_PUBLIC_ORIGIN"),
		OIDCIssuer:          os.Getenv("RELAYDESK_OIDC_ISSUER"),
		OIDCClientID:        os.Getenv("RELAYDESK_OIDC_CLIENT_ID"),
		OIDCClientSecret:    os.Getenv("RELAYDESK_OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:     os.Getenv("RELAYDESK_OIDC_REDIRECT_URL"),
		OIDCBackchannelAddr: os.Getenv("RELAYDESK_OIDC_BACKCHANNEL_ADDR"),
		ProviderMode:        os.Getenv("RELAYDESK_PROVIDER_MODE"),
		DemoMode:            os.Getenv("RELAYDESK_INTERACTIVE_DEMO") == "1",
		StripeAPIURL:        os.Getenv("RELAYDESK_STRIPE_API_URL"),
		HubSpotAPIURL:       os.Getenv("RELAYDESK_HUBSPOT_API_URL"),
		SlackAPIURL:         os.Getenv("RELAYDESK_SLACK_API_URL"),
		HubSpotClientID:     os.Getenv("RELAYDESK_HUBSPOT_CLIENT_ID"),
		HubSpotClientSecret: os.Getenv("RELAYDESK_HUBSPOT_CLIENT_SECRET"),
		SlackClientID:       os.Getenv("RELAYDESK_SLACK_CLIENT_ID"),
		SlackClientSecret:   os.Getenv("RELAYDESK_SLACK_CLIENT_SECRET"),
	}
	switch c.Environment {
	case "development", "test", "production":
	default:
		return Config{}, errors.New("RELAYDESK_ENV must be development, test, or production")
	}
	if c.HTTPAddr == "" || !strings.Contains(c.HTTPAddr, ":") {
		return Config{}, errors.New("RELAYDESK_HTTP_ADDR is required")
	}
	urls := map[string]string{"RELAYDESK_DATABASE_URL": c.DatabaseURL, "RELAYDESK_NATS_URL": c.NATSURL, "RELAYDESK_REDIS_URL": c.RedisURL}
	parsed := map[string]*url.URL{}
	for name, value := range urls {
		u, err := url.Parse(value)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return Config{}, fmt.Errorf("%s must be a URL", name)
		}
		parsed[name] = u
	}
	dbURL := parsed["RELAYDESK_DATABASE_URL"]
	if dbURL.Scheme != "postgres" && dbURL.Scheme != "postgresql" || dbURL.User == nil {
		return Config{}, errors.New("RELAYDESK_DATABASE_URL must use postgres scheme and a role")
	}
	natsURL := parsed["RELAYDESK_NATS_URL"]
	if natsURL.Scheme != "nats" && natsURL.Scheme != "tls" {
		return Config{}, errors.New("RELAYDESK_NATS_URL must use nats or tls scheme")
	}
	if _, err := redis.ParseURL(c.RedisURL); err != nil {
		return Config{}, errors.New("RELAYDESK_REDIS_URL is invalid")
	}
	if c.Environment == "production" && (dbURL.Query().Get("sslmode") != "verify-full" || natsURL.Scheme != "tls" || parsed["RELAYDESK_REDIS_URL"].Scheme != "rediss") {
		return Config{}, errors.New("production requires verified PostgreSQL TLS, NATS TLS, and Redis TLS")
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("RELAYDESK_ENCRYPTION_KEY_B64"))
	if err != nil || len(key) != 32 {
		return Config{}, errors.New("RELAYDESK_ENCRYPTION_KEY_B64 must encode 32 bytes")
	}
	c.EncryptionKey = key
	if c.ProviderMode != "" && c.ProviderMode != "simulation" && c.ProviderMode != "test" {
		return Config{}, errors.New("RELAYDESK_PROVIDER_MODE must be simulation, test, or empty")
	}
	if c.ProviderMode != "" && c.Environment == "production" {
		return Config{}, errors.New("provider test execution is prohibited in production")
	}
	if c.DemoMode && c.Environment != "test" {
		return Config{}, errors.New("interactive demo mode requires test environment")
	}
	if c.ProviderMode == "test" && (c.HubSpotClientID == "" || c.HubSpotClientSecret == "" ||
		c.SlackClientID == "" || c.SlackClientSecret == "") {
		return Config{}, errors.New("test provider execution requires HubSpot and Slack OAuth client credentials")
	}
	return c, nil
}

func (c Config) ValidateAPI() error {
	if (c.HubSpotClientID == "") != (c.HubSpotClientSecret == "") ||
		(c.SlackClientID == "") != (c.SlackClientSecret == "") {
		return errors.New("provider OAuth client ID and secret must be configured together")
	}
	for _, item := range []struct{ name, value, role string }{
		{"RELAYDESK_APP_DATABASE_URL", c.AppDatabaseURL, "relaydesk_app"},
		{"RELAYDESK_AUTH_DATABASE_URL", c.AuthDatabaseURL, "relaydesk_auth"},
	} {
		u, err := url.Parse(item.value)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.User == nil || u.User.Username() != item.role {
			return fmt.Errorf("%s must use the %s role", item.name, item.role)
		}
		if c.Environment == "production" && u.Query().Get("sslmode") != "verify-full" {
			return fmt.Errorf("production %s requires verified TLS", item.name)
		}
	}
	if c.OIDCIssuer == "" || c.OIDCClientID == "" || c.OIDCClientSecret == "" ||
		c.OIDCRedirectURL == "" || c.PublicOrigin == "" {
		return errors.New("OIDC issuer, client, redirect URL, and public origin are required for API")
	}
	origin, err := url.Parse(c.PublicOrigin)
	if err != nil || origin.Host == "" || origin.User != nil || origin.Path != "" ||
		origin.RawQuery != "" || origin.Fragment != "" {
		return errors.New("RELAYDESK_PUBLIC_ORIGIN must be an origin URL")
	}
	redirect, err := url.Parse(c.OIDCRedirectURL)
	if err != nil || redirect.String() != c.PublicOrigin+"/auth/callback" {
		return errors.New("OIDC redirect URL must exactly match public origin plus /auth/callback")
	}
	issuer, err := url.Parse(c.OIDCIssuer)
	if err != nil || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" || len(c.OIDCIssuer) > 512 {
		return errors.New("RELAYDESK_OIDC_ISSUER is invalid")
	}
	if c.Environment == "production" {
		if origin.Scheme != "https" || issuer.Scheme != "https" || c.OIDCBackchannelAddr != "" {
			return errors.New("production requires HTTPS origin/issuer and no development backchannel override")
		}
	} else if origin.Scheme != "http" && origin.Scheme != "https" || issuer.Scheme != "http" && issuer.Scheme != "https" {
		return errors.New("development OIDC URLs require http or https")
	}
	if c.OIDCBackchannelAddr != "" && (c.Environment == "production" || issuer.Hostname() != "127.0.0.1") {
		return errors.New("OIDC backchannel override is restricted to local development")
	}
	return nil
}
