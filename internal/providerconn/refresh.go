package providerconn

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lenoir-dev/LenoirDesk/internal/crypto"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
	"github.com/lenoir-dev/LenoirDesk/internal/telemetry"
)

var ErrRefreshBusy = errors.New("provider refresh is in progress")
var ErrBlockedAuth = errors.New("provider authorization is blocked")

// Refresher uses a PostgreSQL lease and credential-version compare-and-swap.
// A crash or uncertain response leaves the connection blocked until an owner
// reauthorizes it. In particular it never retries a possibly consumed Slack
// refresh token after an unknown transport outcome.
type Refresher struct {
	DB     *store.Store
	Box    *crypto.Box
	Client Client
}

func (r *Refresher) Ensure(ctx context.Context, tenant, connection string) error {
	if r == nil || r.DB == nil || r.Box == nil {
		return ErrBlockedAuth
	}
	lease := uuid.NewString()
	var provider, account string
	var generation, version int64
	var cipher, nonce []byte
	err := r.DB.Pool.QueryRow(ctx, `SELECT provider,account_id,generation,credential_version,ciphertext,nonce
 FROM provider_refresh_claim($1,$2,$3)`, tenant, connection, lease).
		Scan(&provider, &account, &generation, &version, &cipher, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		var state, rotation string
		var expires *time.Time
		err = r.DB.Pool.QueryRow(ctx, `SELECT auth_state,expires_at,rotation_mode FROM provider_refresh_status($1,$2)`, tenant, connection).
			Scan(&state, &expires, &rotation)
		if errors.Is(err, pgx.ErrNoRows) { // Simulation credentials predate provider_token_state.
			return nil
		}
		if err != nil {
			return err
		}
		if state == "refreshing" {
			return ErrRefreshBusy
		}
		if state != "ready" || (rotation == "rotating" && (expires == nil || expires.Before(time.Now().Add(30*time.Second)))) {
			return ErrBlockedAuth
		}
		return nil
	}
	if err != nil {
		return err
	}
	block := func() {
		blockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = r.DB.Pool.Exec(blockCtx, `SELECT provider_refresh_block($1,$2,$3,$4,$5)`, tenant, connection, lease, generation, version)
	}
	plain, err := r.Box.Open(cipher, nonce, tenant, connection, provider, 1)
	if err != nil {
		block()
		return ErrBlockedAuth
	}
	var old Tokens
	if json.Unmarshal(plain, &old) != nil || old.RefreshToken == "" {
		block()
		return ErrBlockedAuth
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	issued, err := r.Client.Refresh(refreshCtx, provider, account, old)
	cancel()
	if err != nil {
		block()
		telemetry.Event(ctx, "refresh_failure", provider)
		if errors.Is(err, ErrInvalidGrant) {
			telemetry.Event(ctx, "blocked_auth", provider)
			return ErrBlockedAuth
		}
		return ErrUncertainRefresh
	}
	raw, err := json.Marshal(issued)
	if err != nil {
		block()
		return ErrUncertainRefresh
	}
	cipher, nonce, err = r.Box.Seal(raw, tenant, connection, provider, 1)
	if err != nil {
		block()
		return ErrUncertainRefresh
	}
	var committed bool
	err = r.DB.Pool.QueryRow(ctx, `SELECT provider_refresh_commit($1,$2,$3,$4,$5,$6,$7,$8)`,
		tenant, connection, lease, generation, version, cipher, nonce, issued.ExpiresAt).Scan(&committed)
	if err != nil || !committed {
		block()
		telemetry.Event(ctx, "refresh_failure", provider)
		return ErrUncertainRefresh
	}
	telemetry.Event(ctx, "refresh_success", provider)
	return nil
}
