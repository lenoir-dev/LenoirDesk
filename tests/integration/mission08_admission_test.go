//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

func TestMission08OIDCFlowAdmissionBound(t *testing.T) {
	ctx := context.Background()
	tx, err := mig.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `DELETE FROM oidc_flows WHERE expires_at<=now()`); err != nil {
		t.Fatal(err)
	}
	var existing int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM oidc_flows`).Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO oidc_flows(state_hash,verifier_ciphertext,verifier_nonce,nonce_hash,expires_at)
	 SELECT convert_to(lpad(g::text,32,'0'),'UTF8'),convert_to('test','UTF8'),
	 convert_to('123456789012','UTF8'),convert_to(lpad(g::text,32,'0'),'UTF8'),now()+interval '10 minutes'
	 FROM generate_series(1000000,1000000+$1-1) g`, 10000-existing); err != nil {
		t.Fatal(err)
	}
	var filled int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM oidc_flows`).Scan(&filled); err != nil || filled != 10000 {
		t.Fatalf("OIDC test setup expected 10000 visible rows, got %d: %v", filled, err)
	}
	if _, err = tx.Exec(ctx, `SAVEPOINT capacity_test`); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `SELECT oidc_begin($1,$2,$3,$4,now()+interval '10 minutes')`,
		[]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), []byte("ciphertext"),
		[]byte("123456789012"), []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if err == nil {
		var after int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM oidc_flows`).Scan(&after)
		t.Fatalf("OIDC flow admission exceeded 10000 rows; after=%d", after)
	}
	if _, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT capacity_test`); err != nil {
		t.Fatal(err)
	}
}

func TestMission08IgnoredWebhookBoundAndNoRawBody(t *testing.T) {
	f := seed(t, true)
	ctx := context.Background()
	in := store.Incoming{Binding: store.Binding{TenantID: f.tenantID, ConnectionID: f.connectionID,
		AccountID: f.account, Mode: "test", Generation: 1}, EventID: "evt_" + randomHex(12),
		EventType: "customer.created", APIVersion: "2026-08-26.dahlia",
		Classification: "ignored", Reason: "unsupported_type", Raw: []byte("synthetic raw ignored body")}
	created, err := db.Accept(ctx, in)
	if err != nil || !created {
		t.Fatalf("ignored acceptance: created=%v err=%v", created, err)
	}
	var rawLength int
	if err = mig.QueryRow(ctx, `SELECT length(raw_payload) FROM webhook_events WHERE tenant_id=$1 AND event_id=$2`,
		f.tenantID, in.EventID).Scan(&rawLength); err != nil || rawLength != 0 {
		t.Fatalf("ignored raw body retained: length=%d err=%v", rawLength, err)
	}
	if _, err = mig.Exec(ctx, `INSERT INTO webhook_events(tenant_id,connection_id,provider,provider_account_id,
	 mode,event_id,event_type,event_api_version,raw_payload,classification,reason,correlation_id,connection_generation,source)
	 SELECT $1,$2,'stripe',$3,'test','evt_limit_'||g::text,'customer.created',$4,''::bytea,
	 'ignored','unsupported_type','synthetic-capacity-test',1,'webhook' FROM generate_series(1,9999) g`,
		f.tenantID, f.connectionID, f.account, in.APIVersion); err != nil {
		t.Fatal(err)
	}
	in.EventID = "evt_" + randomHex(12)
	created, err = db.Accept(ctx, in)
	if created || !errors.Is(err, store.ErrIgnoredCapacity) {
		t.Fatalf("ignored capacity: created=%v err=%v", created, err)
	}
	// A signed duplicate must remain idempotent even after the cap is reached.
	in.EventID = "evt_limit_1"
	created, err = db.Accept(ctx, in)
	if created || err != nil {
		t.Fatalf("ignored duplicate at cap: created=%v err=%v", created, err)
	}
	// Supported work for the same tenant remains admissible if its backlog allows.
	in.EventID = "evt_" + randomHex(12)
	in.EventType = "invoice.paid"
	in.Classification = "supported"
	in.Raw = []byte(`{"id":"synthetic"}`)
	created, err = db.Accept(ctx, in)
	if err != nil || !created {
		t.Fatalf("supported event blocked by ignored cap: created=%v err=%v", created, err)
	}
}
