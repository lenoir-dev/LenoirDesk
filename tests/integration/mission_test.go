//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"

	"github.com/lenoir-dev/LenoirDesk/internal/api"
	"github.com/lenoir-dev/LenoirDesk/internal/config"
	"github.com/lenoir-dev/LenoirDesk/internal/crypto"
	"github.com/lenoir-dev/LenoirDesk/internal/delivery"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

var mig *pgxpool.Pool
var db *store.Store
var appDB *store.Store
var authDB *store.Store
var box *crypto.Box
var testKey []byte
var server *httptest.Server
var workerCancel context.CancelFunc
var workerDone chan struct{}
var natsURL = "nats://127.0.0.1:54222"
var testAppDatabaseURL = "postgres://relaydesk_app@127.0.0.1:55432/relaydesk?sslmode=disable"
var testAuthDatabaseURL = "postgres://relaydesk_auth@127.0.0.1:55432/relaydesk?sslmode=disable"
var testRuntimeDatabaseURL = "postgres://relaydesk_runtime@127.0.0.1:55432/relaydesk?sslmode=disable"

func TestMain(m *testing.M) {
	root, err := filepath.Abs("../..")
	if err != nil {
		panic(err)
	}
	envFile := os.Getenv("RELAYDESK_TEST_ENV_FILE")
	if envFile == "" {
		envFile = ".env"
	}
	if !filepath.IsAbs(envFile) {
		envFile = filepath.Join(root, envFile)
	}
	env, err := readEnv(envFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	pgPort := os.Getenv("RELAYDESK_TEST_PG_PORT")
	if pgPort == "" {
		pgPort = "55432"
	}
	hostAddress := "@127.0.0.1:" + pgPort
	runtimeURL := strings.Replace(env["RELAYDESK_DATABASE_URL"], "@postgres:5432", hostAddress, 1)
	appURL := strings.Replace(env["RELAYDESK_APP_DATABASE_URL"], "@postgres:5432", hostAddress, 1)
	authURL := strings.Replace(env["RELAYDESK_AUTH_DATABASE_URL"], "@postgres:5432", hostAddress, 1)
	migrateURL := strings.Replace(env["RELAYDESK_MIGRATION_DATABASE_URL"], "@postgres:5432", hostAddress, 1)
	testAppDatabaseURL, testAuthDatabaseURL = appURL, authURL
	testRuntimeDatabaseURL = runtimeURL
	ctx := context.Background()
	mig, err = pgxpool.New(ctx, migrateURL)
	if err != nil {
		panic(err)
	}
	db, err = store.Open(ctx, runtimeURL)
	if err != nil {
		panic(err)
	}
	appDB, err = store.Open(ctx, appURL)
	if err != nil {
		panic(err)
	}
	authDB, err = store.Open(ctx, authURL)
	if err != nil {
		panic(err)
	}
	key, err := base64.StdEncoding.DecodeString(env["RELAYDESK_ENCRYPTION_KEY_B64"])
	if err != nil {
		panic(err)
	}
	testKey = key
	box, err = crypto.NewBox(key)
	if err != nil {
		panic(err)
	}
	if isolatedNATS := os.Getenv("RELAYDESK_TEST_NATS_URL"); isolatedNATS != "" {
		natsURL = isolatedNATS
	}
	c := config.Config{HTTPAddr: "127.0.0.1:0", ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}
	server = httptest.NewServer(api.New(c, db, box, slog.New(slog.NewTextHandler(io.Discard, nil)), nil).Handler)
	workerCtx, cancel := context.WithCancel(context.Background())
	workerCancel = cancel
	workerDone = make(chan struct{})
	if os.Getenv("RELAYDESK_M081_LOAD") == "1" || os.Getenv("RELAYDESK_M081_LIVE_RECONCILE") == "1" ||
		os.Getenv("RELAYDESK_M081_TRACE") == "1" {
		// The load test starts its own complete worker. The real read-only
		// reconciliation and trace gates must not activate any worker.
		close(workerDone)
	} else {
		w, nc, workerErr := delivery.New(workerCtx, db, natsURL, slog.New(slog.NewTextHandler(os.Stdout, nil)))
		if workerErr != nil {
			panic(workerErr)
		}
		go func() { defer close(workerDone); w.Run(workerCtx); nc.Close() }()
	}
	code := m.Run()
	cancel()
	<-workerDone
	if mission07MetricsShutdown != nil {
		_ = mission07MetricsShutdown(context.Background())
	}
	server.Close()
	db.Close()
	appDB.Close()
	authDB.Close()
	mig.Close()
	os.Exit(code)
}
func readEnv(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) == 2 {
			out[kv[0]] = kv[1]
		}
	}
	return out, nil
}
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

type fixture struct{ tenantID, connectionID, revisionID, ref, secret, account string }

func seed(t *testing.T, active bool) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{ref: randomHex(32), secret: "whsec_" + randomHex(16), account: "acct_" + randomHex(8)}
	if err := mig.QueryRow(ctx, `INSERT INTO tenants (name) VALUES ('test') RETURNING id`).Scan(&f.tenantID); err != nil {
		t.Fatal(err)
	}
	// Allocate the connection ID first, then bind ciphertext to that exact identity.
	if err := mig.QueryRow(ctx, `SELECT gen_random_uuid()`).Scan(&f.connectionID); err != nil {
		t.Fatal(err)
	}
	encrypted, nonce, err := box.Seal([]byte(f.secret), f.tenantID, f.connectionID, "stripe", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = mig.Exec(ctx, `INSERT INTO connections (id,tenant_id,provider,provider_account_id,mode,status,webhook_ref_hash)
	  VALUES ($1,$2,'stripe',$3,'test','active',decode($4,'hex'))`, f.connectionID, f.tenantID, f.account, hashHex(f.ref)); err != nil {
		t.Fatal(err)
	}
	if _, err = mig.Exec(ctx, `INSERT INTO connection_credentials (tenant_id,connection_id,generation,secret_kind,ciphertext,nonce,key_version)
	  VALUES ($1,$2,1,'webhook_signing',$3,$4,1)`, f.tenantID, f.connectionID, encrypted, nonce); err != nil {
		t.Fatal(err)
	}
	status := "inactive"
	if active {
		status = "active"
	}
	if err = mig.QueryRow(ctx, `INSERT INTO automation_revisions (tenant_id,connection_id,revision,status) VALUES ($1,$2,1,$3) RETURNING id`, f.tenantID, f.connectionID, status).Scan(&f.revisionID); err != nil {
		t.Fatal(err)
	}
	return f
}
func hashHex(s string) string { // same SHA-256 lookup key as production
	sum := sha256sum([]byte(s))
	return hex.EncodeToString(sum)
}
func sha256sum(b []byte) []byte { h := sha256.Sum256(b); return h[:] }
func payload(id string, live bool, account, kind string) []byte {
	objectType := "subscription"
	objectID := "sub_test"
	if strings.HasPrefix(kind, "invoice.") {
		objectType = "invoice"
		objectID = "in_test"
	}
	v := map[string]any{"id": id, "object": "event", "type": kind, "api_version": stripe.APIVersion, "livemode": live, "data": map[string]any{"object": map[string]any{"id": objectID, "object": objectType}}}
	if account != "" {
		v["account"] = account
	}
	b, _ := json.Marshal(v)
	return b
}
func post(t *testing.T, f fixture, body []byte, stamp time.Time, secret string) int {
	t.Helper()
	sig := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: secret, Timestamp: stamp})
	return postWithHeader(t, f, body, sig.Header)
}
func postWithHeader(t *testing.T, f fixture, body []byte, header string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/webhooks/stripe/"+f.ref, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Stripe-Signature", header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
func count(t *testing.T, table, column, tenantID string) int {
	t.Helper()
	var n int
	query := fmt.Sprintf("SELECT count(*) FROM %s WHERE tenant_id=$1", table)
	if column != "" {
		query += " AND " + column
	}
	if err := mig.QueryRow(context.Background(), query, tenantID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func await(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestA_DuplicateWebhook(t *testing.T) {
	f := seed(t, true)
	body := payload("evt_"+randomHex(12), false, "", "invoice.paid")
	var wg sync.WaitGroup
	errs := make(chan int, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- post(t, f, body, time.Now(), f.secret) }()
	}
	wg.Wait()
	close(errs)
	for code := range errs {
		if code != http.StatusAccepted && code != http.StatusOK {
			t.Fatalf("unexpected status: %d", code)
		}
	}
	await(t, func() bool { return count(t, "runs", "", f.tenantID) == 1 })
	if n := count(t, "webhook_events", "", f.tenantID); n != 1 {
		t.Fatalf("events=%d", n)
	}
	if n := count(t, "planning_jobs", "", f.tenantID); n != 1 {
		t.Fatalf("jobs=%d", n)
	}
	if n := count(t, "outbox", "", f.tenantID); n != 1 {
		t.Fatalf("outbox=%d", n)
	}
}
func TestE_InvalidWebhook(t *testing.T) {
	f := seed(t, true)
	good := payload("evt_"+randomHex(12), false, "", "invoice.paid")
	cases := []struct {
		name   string
		body   []byte
		stamp  time.Time
		secret string
		status int
	}{
		{"bad signature", good, time.Now(), "whsec_wrong", 400},
		{"old timestamp", good, time.Now().Add(-10 * time.Minute), f.secret, 400},
		{"wrong mode", payload("evt_"+randomHex(12), true, "", "invoice.paid"), time.Now(), f.secret, 400},
		{"wrong account", payload("evt_"+randomHex(12), false, "acct_other", "invoice.paid"), time.Now(), f.secret, 400},
		{"malformed", []byte("not-json"), time.Now(), f.secret, 400},
		{"missing event object", []byte(`{"id":"evt_missing","object":"event","type":"invoice.paid","api_version":"` + stripe.APIVersion + `","livemode":false}`), time.Now(), f.secret, 400},
		{"too large", bytes.Repeat([]byte("x"), 256*1024+1), time.Now(), f.secret, 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := post(t, f, tc.body, tc.stamp, tc.secret); got != tc.status {
				t.Fatalf("got %d want %d", got, tc.status)
			}
		})
	}
	sig := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: good, Secret: f.secret, Timestamp: time.Now()})
	altered := append(append([]byte(nil), good...), 32)
	if got := postWithHeader(t, f, altered, sig.Header); got != 400 {
		t.Fatalf("altered raw payload: got %d want 400", got)
	}
	if n := count(t, "webhook_events", "", f.tenantID); n != 0 {
		t.Fatalf("invalid events persisted=%d", n)
	}
}
func TestE_UnsupportedSignedEventIsRecordedWithoutWork(t *testing.T) {
	f := seed(t, true)
	body := payload("evt_"+randomHex(12), false, "", "customer.created")
	if got := post(t, f, body, time.Now(), f.secret); got != 202 {
		t.Fatalf("unsupported valid event status=%d", got)
	}
	var class, reason string
	if err := mig.QueryRow(context.Background(), `SELECT classification,reason FROM webhook_events WHERE tenant_id=$1`, f.tenantID).Scan(&class, &reason); err != nil {
		t.Fatal(err)
	}
	if class != "ignored" || reason != "unsupported_type" || count(t, "planning_jobs", "", f.tenantID) != 0 {
		t.Fatalf("unexpected classification=%s reason=%s", class, reason)
	}
}
func TestF_TenantIsolation(t *testing.T) {
	a := seed(t, true)
	b := seed(t, true)
	ctx := context.Background()
	tx, err := store.BeginTenant(ctx, db.Pool, a.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM connections WHERE id=$1`, b.connectionID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cross tenant read: n=%d err=%v", n, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE connections SET status='disconnected' WHERE id=$1`, b.connectionID); err == nil {
		t.Fatal("runtime role unexpectedly allowed connection mutation")
	}
	_ = tx.Rollback(ctx)
	tx, err = store.BeginTenant(ctx, db.Pool, a.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO webhook_events (tenant_id,connection_id,provider,provider_account_id,mode,event_id,event_type,raw_payload,classification,correlation_id)
       VALUES ($1,$2,'stripe',$3,'test','evt_cross','invoice.paid','{}','ignored','test')`, a.tenantID, b.connectionID, b.account); err == nil {
		t.Fatal("cross tenant FK accepted")
	}
	_ = tx.Rollback(ctx)
	tx, err = store.BeginTenant(ctx, db.Pool, b.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM connections WHERE id=$1`, b.connectionID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("pool tenant context failed: n=%d err=%v", n, err)
	}
	_ = tx.Rollback(ctx)
	if err = db.Pool.QueryRow(ctx, `SELECT count(*) FROM connections`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unscoped read: n=%d err=%v", n, err)
	}
	if err = db.Pool.QueryRow(ctx, `SELECT signing_secret_ciphertext FROM connections LIMIT 1`).Scan(new([]byte)); err == nil {
		t.Fatal("runtime role can read encrypted secrets directly")
	}
	var bypass, owner bool
	if err = mig.QueryRow(ctx, `SELECT rolbypassrls, (SELECT pg_get_userbyid(relowner)='relaydesk_runtime' FROM pg_class WHERE relname='connections') FROM pg_roles WHERE rolname='relaydesk_runtime'`).Scan(&bypass, &owner); err != nil || bypass || owner {
		t.Fatalf("runtime DB role privilege: bypass=%v owner=%v err=%v", bypass, owner, err)
	}
	// Even privileged test setup cannot create a cross-tenant job through the declared FK.
	var eventRowID string
	if err = mig.QueryRow(ctx, `INSERT INTO webhook_events (tenant_id,connection_id,provider,provider_account_id,mode,event_id,event_type,raw_payload,classification,correlation_id)
	  VALUES ($1,$2,'stripe',$3,'test',$4,'invoice.paid','{}','ignored','test') RETURNING id`, b.tenantID, b.connectionID, b.account, "evt_"+randomHex(8)).Scan(&eventRowID); err != nil {
		t.Fatal(err)
	}
	if _, err = mig.Exec(ctx, `INSERT INTO planning_jobs (tenant_id,event_row_id,automation_revision_id) VALUES ($1,$2,$3)`, a.tenantID, eventRowID, a.revisionID); err == nil {
		t.Fatal("cross tenant planner link accepted")
	}
}
func TestG_ConfigurationRevision(t *testing.T) {
	f := seed(t, true)
	id := "evt_" + randomHex(12)
	body := payload(id, false, "", "invoice.paid")
	if got := post(t, f, body, time.Now(), f.secret); got != 202 {
		t.Fatalf("first=%d", got)
	}
	_, err := mig.Exec(context.Background(), `UPDATE automation_revisions SET status='inactive' WHERE id=$1`, f.revisionID)
	if err != nil {
		t.Fatal(err)
	}
	var next string
	err = mig.QueryRow(context.Background(), `INSERT INTO automation_revisions (tenant_id,connection_id,revision,status) VALUES ($1,$2,2,'active') RETURNING id`, f.tenantID, f.connectionID).Scan(&next)
	if err != nil {
		t.Fatal(err)
	}
	if got := post(t, f, body, time.Now(), f.secret); got != 200 {
		t.Fatalf("repeat=%d", got)
	}
	var original string
	err = mig.QueryRow(context.Background(), `SELECT automation_revision_id FROM webhook_events WHERE tenant_id=$1`, f.tenantID).Scan(&original)
	if err != nil || original != f.revisionID {
		t.Fatalf("revision changed: %s %v", original, err)
	}
	inactive := seed(t, false)
	old := payload("evt_"+randomHex(12), false, "", "invoice.paid")
	if got := post(t, inactive, old, time.Now(), inactive.secret); got != 202 {
		t.Fatalf("inactive first=%d", got)
	}
	_, err = mig.Exec(context.Background(), `UPDATE automation_revisions SET status='active' WHERE id=$1`, inactive.revisionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := post(t, inactive, old, time.Now(), inactive.secret); got != 200 {
		t.Fatalf("inactive repeat=%d", got)
	}
	if count(t, "planning_jobs", "", inactive.tenantID) != 0 {
		t.Fatal("replay activated old event")
	}
}
func TestD_DuplicateOutboxPublication(t *testing.T) {
	f := seed(t, true)
	body := payload("evt_"+randomHex(12), false, "", "invoice.paid")
	if got := post(t, f, body, time.Now(), f.secret); got != 202 {
		t.Fatal(got)
	}
	await(t, func() bool { return count(t, "runs", "", f.tenantID) == 1 })
	_, err := mig.Exec(context.Background(), `UPDATE outbox SET published_at=NULL,lease_token=NULL,lease_until=NULL WHERE tenant_id=$1`, f.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		var n int
		_ = mig.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE tenant_id=$1 AND published_at IS NOT NULL`, f.tenantID).Scan(&n)
		return n == 1
	})
	if count(t, "runs", "", f.tenantID) != 1 {
		t.Fatal("duplicate run")
	}
	var jobID string
	if err := mig.QueryRow(context.Background(), `SELECT id FROM planning_jobs WHERE tenant_id=$1`, f.tenantID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if err := db.Plan(context.Background(), f.tenantID, jobID, 1); err != nil {
		t.Fatal(err)
	}
	if count(t, "runs", "", f.tenantID) != 1 {
		t.Fatal("repeated planner execution created another run")
	}
}
func compose(t *testing.T, args ...string) {
	t.Helper()
	binary := "docker-compose"
	if _, err := exec.LookPath(binary); err != nil {
		binary = "docker"
		args = append([]string{"compose"}, args...)
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = "../.."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compose %v failed: %s (%v)", args, string(out), err)
	}
}
func TestB_NATSOutage(t *testing.T) {
	f := seed(t, true)
	compose(t, "stop", "nats")
	defer compose(t, "start", "nats")
	body := payload("evt_"+randomHex(12), false, "", "invoice.paid")
	if got := post(t, f, body, time.Now(), f.secret); got != 202 {
		t.Fatalf("NATS outage webhook=%d", got)
	}
	if count(t, "outbox", "", f.tenantID) != 1 {
		t.Fatal("outbox missing")
	}
	compose(t, "start", "nats")
	await(t, func() bool { return count(t, "runs", "", f.tenantID) == 1 })
	if got := post(t, f, body, time.Now(), f.secret); got != 200 {
		t.Fatalf("repeat=%d", got)
	}
	if count(t, "runs", "", f.tenantID) != 1 {
		t.Fatal("duplicate run")
	}
}
func TestC_PostgreSQLOutage(t *testing.T) {
	f := seed(t, true)
	compose(t, "stop", "postgres")
	defer compose(t, "start", "postgres")
	body := payload("evt_"+randomHex(12), false, "", "invoice.paid")
	sig := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: f.secret, Timestamp: time.Now()})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/webhooks/stripe/"+f.ref, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig.Header)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			t.Fatalf("false successful response: %d", resp.StatusCode)
		}
	}
	compose(t, "start", "postgres")
	await(t, func() bool { return db.Ping(context.Background()) == nil })
	if got := post(t, f, body, time.Now(), f.secret); got != 202 {
		t.Fatalf("post recovery=%d", got)
	}
}
