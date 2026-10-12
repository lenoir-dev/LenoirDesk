package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestProductionRejectsInsecureTransport(t *testing.T) {
	t.Setenv("RELAYDESK_ENV", "production")
	t.Setenv("RELAYDESK_HTTP_ADDR", ":8080")
	t.Setenv("RELAYDESK_DATABASE_URL", "postgres://role:password@db:5432/relaydesk?sslmode=disable")
	t.Setenv("RELAYDESK_NATS_URL", "nats://nats:4222")
	t.Setenv("RELAYDESK_REDIS_URL", "redis://redis:6379")
	t.Setenv("RELAYDESK_ENCRYPTION_KEY_B64", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32))))
	if _, err := Load(); err == nil {
		t.Fatal("production accepted plaintext dependencies")
	}
	t.Setenv("RELAYDESK_DATABASE_URL", "postgres://role:password@db:5432/relaydesk?sslmode=verify-full")
	t.Setenv("RELAYDESK_NATS_URL", "tls://nats:4222")
	t.Setenv("RELAYDESK_REDIS_URL", "rediss://redis:6379")
	if _, err := Load(); err != nil {
		t.Fatalf("secure configuration rejected: %v", err)
	}
}
