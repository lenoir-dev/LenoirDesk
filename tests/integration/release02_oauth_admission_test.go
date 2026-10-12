//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/identity"
)

func waitRelease02Database(t *testing.T) {
	t.Helper()
	mig.Reset() // discard connections terminated by the preceding outage test
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := mig.Ping(ctx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("database did not recover after the intentional outage test: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRelease02OIDCAdmissionFairnessAndCleanup(t *testing.T) {
	waitRelease02Database(t)
	ctx := context.Background()
	var sourceLimit, globalLimit int
	if err := mig.QueryRow(ctx, `SELECT oidc_source_per_minute,oidc_global_per_minute FROM oauth_admission_policy`).Scan(&sourceLimit, &globalLimit); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Exec(ctx, `DELETE FROM oidc_admission_buckets`); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Exec(ctx, `UPDATE oauth_admission_policy SET oidc_source_per_minute=3,oidc_global_per_minute=5`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = mig.Exec(ctx, `UPDATE oauth_admission_policy SET oidc_source_per_minute=$1,oidc_global_per_minute=$2`, sourceLimit, globalLimit)
		_, _ = mig.Exec(ctx, `DELETE FROM oidc_admission_buckets`)
	})
	app := newTestApp(t, newFakeOIDC(t))
	other := newTestApp(t, newFakeOIDC(t)) // independent API instance; same auth DB
	fingerprint, err := app.service.SourceFingerprint("192.0.2.10:43122")
	if err != nil {
		t.Fatal(err)
	}
	sameIP, err := other.service.SourceFingerprint("[::ffff:192.0.2.10]:9999")
	if err != nil || string(fingerprint) != string(sameIP) {
		t.Fatal("IPv4-mapped IPv6 or port changed source identity")
	}
	if _, err := app.service.SourceFingerprint("untrusted"); !errors.Is(err, identity.ErrInvalidAdmissionSource) {
		t.Fatal("invalid peer accepted")
	}
	var admitted, limited atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			service := app.service
			if n%2 == 0 {
				service = other.service
			}
			_, _, beginErr := service.Begin(ctx, "192.0.2.10:43122")
			switch {
			case beginErr == nil:
				admitted.Add(1)
			case errors.Is(beginErr, identity.ErrAdmissionLimited):
				limited.Add(1)
			default:
				t.Errorf("unexpected admission error: %v", beginErr)
			}
		}(i)
	}
	wg.Wait()
	if admitted.Load() != 3 || limited.Load() != 17 {
		t.Fatalf("single-source budget: admitted=%d limited=%d", admitted.Load(), limited.Load())
	}
	if _, _, err := other.service.Begin(ctx, "[2001:db8::15]:48200"); err != nil {
		t.Fatalf("independent IPv6 peer denied under single-source load: %v", err)
	}
	if _, _, err := other.service.Begin(ctx, "192.0.2.11:48200"); err != nil {
		t.Fatalf("independent IPv4 peer denied under single-source load: %v", err)
	}
	if _, _, err := other.service.Begin(ctx, "192.0.2.12:48200"); !errors.Is(err, identity.ErrAdmissionLimited) {
		t.Fatalf("global budget not enforced: %v", err)
	}
	// Headers cannot change a peer's budget, including on the HTTP route.
	req, _ := http.NewRequest("GET", app.server.URL+"/auth/login", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.123")
	resp, err := app.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "60" {
		t.Fatalf("HTTP admission response=%d retry=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if _, err := mig.Exec(ctx, `UPDATE oidc_admission_buckets SET bucket_start=date_trunc('minute',now())-interval '3 minutes'
	 WHERE bucket_start=date_trunc('minute',now())`); err != nil {
		t.Fatal(err)
	}
	if removed, err := app.service.CleanupExpiredFlows(ctx); err != nil || removed < 1 {
		t.Fatalf("admission bucket expiry cleanup: removed=%d err=%v", removed, err)
	}
	if _, _, err := app.service.Begin(ctx, "192.0.2.10:43122"); err != nil {
		t.Fatalf("source budget did not recover after window expiry: %v", err)
	}
}

func TestRelease02ProviderPendingQuotaAndLifecycle(t *testing.T) {
	waitRelease02Database(t)
	ctx := context.Background()
	var ownerLimit, tenantLimit, globalLimit int
	if err := mig.QueryRow(ctx, `SELECT provider_owner_pending,provider_tenant_pending,provider_global_pending
	 FROM oauth_admission_policy`).Scan(&ownerLimit, &tenantLimit, &globalLimit); err != nil {
		t.Fatal(err)
	}
	var preexisting int
	if err := mig.QueryRow(ctx, `SELECT count(*) FROM provider_oauth_flows WHERE expires_at>now()`).Scan(&preexisting); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Exec(ctx, `UPDATE oauth_admission_policy SET provider_owner_pending=2,provider_tenant_pending=2,provider_global_pending=$1`, preexisting+3); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = mig.Exec(ctx, `UPDATE oauth_admission_policy SET provider_owner_pending=$1,provider_tenant_pending=$2,provider_global_pending=$3`, ownerLimit, tenantLimit, globalLimit)
	})
	app := newTestApp(t, newFakeOIDC(t))
	owner := login(t, app, "release02-owner-a", nil)
	_, csrf := me(t, app, owner)
	tenantA := createWorkspace(t, app, owner, csrf, "Release02 A")
	principalA, err := app.service.Current(ctx, owner.Value)
	if err != nil {
		t.Fatal(err)
	}
	other := newTestApp(t, newFakeOIDC(t))
	ownerB := login(t, other, "release02-owner-b", nil)
	_, csrfB := me(t, other, ownerB)
	tenantB := createWorkspace(t, other, ownerB, csrfB, "Release02 B")
	principalB, err := other.service.Current(ctx, ownerB.Value)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = mig.Exec(ctx, `DELETE FROM provider_oauth_flows WHERE tenant_id IN ($1,$2)`, tenantA, tenantB)
	})
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			service := app.service
			if n%2 == 0 {
				service = other.service
			}
			_, _, beginErr := service.BeginProvider(ctx, principalA, tenantA, "slack", "T-TEST", "")
			if beginErr == nil {
				admitted.Add(1)
			} else if !errors.Is(beginErr, identity.ErrProviderAdmissionLimited) {
				t.Errorf("provider begin: %v", beginErr)
			}
		}(i)
	}
	wg.Wait()
	if admitted.Load() != 2 {
		t.Fatalf("atomic owner/tenant quota admitted %d", admitted.Load())
	}
	if _, _, err := other.service.BeginProvider(ctx, principalB, tenantB, "slack", "T-TEST-B", ""); err != nil {
		t.Fatalf("other tenant denied: %v", err)
	}
	if _, _, err := other.service.BeginProvider(ctx, principalB, tenantB, "slack", "T-TEST-B", ""); !errors.Is(err, identity.ErrProviderAdmissionLimited) {
		t.Fatalf("global quota not enforced: %v", err)
	}
	// A bounded sweep must preserve every unexpired pending flow.
	if _, err := mig.Exec(ctx, `UPDATE provider_oauth_flows SET expires_at=now()-interval '1 second'
	 WHERE state_hash=(SELECT state_hash FROM provider_oauth_flows WHERE tenant_id=$1 ORDER BY created_at LIMIT 1)`, tenantA); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := mig.QueryRow(ctx, `SELECT count(*) FROM provider_oauth_flows WHERE tenant_id=$1 AND expires_at>now()`, tenantA).Scan(&before); err != nil || before != 1 {
		t.Fatalf("expected one valid pending flow: count=%d err=%v", before, err)
	}
	cleanupDone := make(chan error, 1)
	go func() { _, cleanupErr := other.service.CleanupExpiredFlows(ctx); cleanupDone <- cleanupErr }()
	_, _, beginErr := app.service.BeginProvider(ctx, principalA, tenantA, "slack", "T-TEST", "")
	if beginErr != nil {
		t.Fatal(beginErr)
	}
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	var pending, expired int
	if err := mig.QueryRow(ctx, `SELECT count(*) FILTER (WHERE expires_at>now()),count(*) FILTER (WHERE expires_at<=now())
	 FROM provider_oauth_flows WHERE tenant_id=$1`, tenantA).Scan(&pending, &expired); err != nil || pending != 2 || expired != 0 {
		t.Fatalf("cleanup/create race: pending=%d expired=%d err=%v", pending, expired, err)
	}
	for i := 0; i < 20; i++ {
		if _, _, err := app.service.BeginProvider(ctx, principalA, tenantA, "slack", "T-TEST", ""); !errors.Is(err, identity.ErrProviderAdmissionLimited) {
			t.Fatalf("repeated attempt %d escaped quota: %v", i, err)
		}
	}
	if err := mig.QueryRow(ctx, `SELECT count(*) FROM provider_oauth_flows WHERE tenant_id=$1`, tenantA).Scan(&pending); err != nil || pending != 2 {
		t.Fatalf("pending rows grew after rejected attempts: %d %v", pending, err)
	}
	// Consume directly using the same one-shot function used by the callback.
	var state []byte
	var redirect string
	if err := mig.QueryRow(ctx, `SELECT state_hash,redirect_uri FROM provider_oauth_flows WHERE tenant_id=$1 LIMIT 1`, tenantA).Scan(&state, &redirect); err != nil {
		t.Fatal(err)
	}
	var consumed string
	if err := authDB.Pool.QueryRow(ctx, `SELECT tenant_id FROM provider_oauth_consume($1,$2,'slack',$3)`, state, principalA.TokenHash,
		redirect).Scan(&consumed); err != nil || consumed != tenantA {
		t.Fatalf("first consume: %s %v", consumed, err)
	}
	if err := authDB.Pool.QueryRow(ctx, `SELECT tenant_id FROM provider_oauth_consume($1,$2,'slack',$3)`, state, principalA.TokenHash,
		redirect).Scan(&consumed); err == nil {
		t.Fatal("provider OAuth callback replay admitted")
	}
}
