//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"

	"github.com/lenoir-dev/LenoirDesk/internal/execution"
)

func breakerTestKey(provider, account string) string {
	h := sha256.New()
	for _, part := range []string{"breaker", provider, account} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return "relaydesk:quota:{rdq}:" + hex.EncodeToString(h.Sum(nil))
}

func TestMission05ProviderBreakerIsolatedHalfOpen(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	breaker := &execution.ProviderBreaker{Client: client}
	accountA, accountB := uuid.NewString(), uuid.NewString()
	for i := 0; i < 3; i++ {
		if err := breaker.After(ctx, "hubspot", accountA, "", 503, false); err != nil {
			t.Fatal(err)
		}
	}
	wait, _, err := breaker.Before(ctx, "hubspot", accountA)
	if err != nil || wait <= 0 {
		t.Fatalf("failed account did not open: %s %v", wait, err)
	}
	if wait, _, err = breaker.Before(ctx, "hubspot", accountB); err != nil || wait != 0 {
		t.Fatalf("other account was blocked: %s %v", wait, err)
	}
	key := breakerTestKey("hubspot", accountA)
	if err := client.HSet(ctx, key, "open_until", 1).Err(); err != nil {
		t.Fatal(err)
	}
	wait, probe, err := breaker.Before(ctx, "hubspot", accountA)
	if err != nil || wait != 0 || probe == "" {
		t.Fatalf("half-open probe unavailable: %s %v", wait, err)
	}
	if wait, _, err = breaker.Before(ctx, "hubspot", accountA); err != nil || wait <= 0 {
		t.Fatalf("second half-open probe admitted: %s %v", wait, err)
	}
	if err = breaker.After(ctx, "hubspot", accountA, probe, 200, false); err != nil {
		t.Fatal(err)
	}
	if wait, _, err = breaker.Before(ctx, "hubspot", accountA); err != nil || wait != 0 {
		t.Fatalf("breaker did not close: %s %v", wait, err)
	}
	_ = client.Del(ctx, key, key+":probe").Err()
}
