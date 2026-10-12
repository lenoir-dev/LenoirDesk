package identity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/oauth2"

	"github.com/lenoir-dev/LenoirDesk/internal/providerconn"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
	"github.com/lenoir-dev/LenoirDesk/internal/telemetry"
)

const ProviderFlowLifetime = 10 * time.Minute

var ErrProviderFlow = errors.New("invalid provider OAuth flow")
var ErrProviderMismatch = errors.New("provider account identity mismatch")
var ErrProviderAdmissionLimited = errors.New("provider OAuth admission limited")

func (s *Service) providerClient() providerconn.Client {
	return providerconn.Client{Config: providerconn.Config{
		HubSpotID: s.config.HubSpotClientID, HubSpotSecret: s.config.HubSpotClientSecret,
		SlackID: s.config.SlackClientID, SlackSecret: s.config.SlackClientSecret,
	}, HTTP: s.ProviderHTTP}
}

func (s *Service) BeginProvider(ctx context.Context, p Principal, tenant, provider, expected, reconnect string) (string, string, error) {
	if _, err := uuid.Parse(tenant); err != nil || len(expected) < 1 || len(expected) > 128 || strings.ContainsAny(expected, " \t\n") {
		return "", "", ErrProviderFlow
	}
	var reconnectID *string
	if reconnect != "" {
		if _, err := uuid.Parse(reconnect); err != nil {
			return "", "", ErrProviderFlow
		}
		reconnectID = &reconnect
	}
	if provider != "hubspot" && provider != "slack" {
		return "", "", ErrProviderFlow
	}
	redirect := s.config.PublicOrigin + "/oauth/" + provider + "/callback"
	state, err := randomToken()
	if err != nil {
		return "", "", err
	}
	verifier := ""
	var cipher, nonce []byte
	if provider == "hubspot" {
		verifier = oauth2.GenerateVerifier()
		cipher, nonce, err = s.box.SealPurpose([]byte(verifier), "provider-oauth|"+state)
		if err != nil {
			return "", "", err
		}
	}
	redirectURL, err := s.providerClient().AuthorizationURL(provider, state, redirect, verifier)
	if err != nil {
		return "", "", err
	}
	_, err = s.db.Pool.Exec(ctx, `SELECT provider_oauth_begin($1,$2,$3,$4,$5,$6,$7,now()+interval '10 minutes',$8,$9)`,
		hash(state), p.TokenHash, tenant, provider, expected, reconnectID, redirect, cipher, nonce)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "P5302" {
			return "", "", ErrProviderAdmissionLimited
		}
		return "", "", err
	}
	return redirectURL, state, nil
}

func (s *Service) CompleteProvider(ctx context.Context, p Principal, provider, state, code, correlation string) (string, error) {
	if (provider != "hubspot" && provider != "slack") || len(state) != 43 || code == "" || len(code) > 4096 {
		return "", ErrProviderFlow
	}
	redirect := s.config.PublicOrigin + "/oauth/" + provider + "/callback"
	var tenant, expected string
	var reconnect *string
	var cipher, nonce []byte
	err := s.db.Pool.QueryRow(ctx, `SELECT tenant_id,expected_account_id,reconnect_id,verifier_ciphertext,verifier_nonce
 FROM provider_oauth_consume($1,$2,$3,$4)`, hash(state), p.TokenHash, provider, redirect).
		Scan(&tenant, &expected, &reconnect, &cipher, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrProviderFlow
	}
	if err != nil {
		return "", err
	}
	verifier := ""
	if provider == "hubspot" {
		plain, openErr := s.box.OpenPurpose(cipher, nonce, "provider-oauth|"+state)
		if openErr != nil {
			return "", ErrProviderFlow
		}
		verifier = string(plain)
	}
	issued, account, err := s.providerClient().Exchange(ctx, provider, code, redirect, verifier)
	if err != nil {
		return "", err
	}
	if account != expected {
		return "", ErrProviderMismatch
	}
	newID := uuid.NewString()
	generation := int64(1)
	if reconnect != nil {
		current, getErr := s.GetConnection(ctx, p, tenant, *reconnect)
		if getErr != nil || current.Provider != provider || current.ProviderAccountID != account || current.Mode != "test" {
			return "", ErrProviderMismatch
		}
		newID = *reconnect
		generation = current.Generation + 1
	}
	raw, err := json.Marshal(issued)
	if err != nil {
		return "", err
	}
	cipher, nonce, err = s.box.Seal(raw, tenant, newID, provider, 1)
	if err != nil {
		return "", err
	}
	var result string
	err = s.db.Pool.QueryRow(ctx, `SELECT provider_connection_commit($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		p.TokenHash, tenant, provider, account, expected, reconnect, newID, generation, cipher, nonce, nullableExpiry(issued), issued.Rotation, correlation).Scan(&result)
	if err == nil && reconnect != nil {
		telemetry.Event(ctx, "reconnect", provider)
	}
	return result, err
}

func nullableExpiry(t providerconn.Tokens) *time.Time {
	if t.ExpiresAt.IsZero() {
		return nil
	}
	return &t.ExpiresAt
}

func (s *Service) DisconnectProvider(ctx context.Context, p Principal, tenant, connection, correlation string) (bool, bool, error) {
	if _, err := uuid.Parse(tenant); err != nil {
		return false, false, ErrForbidden
	}
	if _, err := uuid.Parse(connection); err != nil {
		return false, false, store.ErrNotFound
	}
	var provider string
	var cipher, nonce []byte
	materialErr := s.db.Pool.QueryRow(ctx, `SELECT provider,ciphertext,nonce FROM provider_revoke_material($1,$2,$3)`,
		p.TokenHash, tenant, connection).Scan(&provider, &cipher, &nonce)
	if materialErr != nil && !errors.Is(materialErr, pgx.ErrNoRows) {
		return false, false, materialErr
	}
	var changed bool
	err := s.db.Pool.QueryRow(ctx, `SELECT provider_connection_disconnect($1,$2,$3,$4)`, p.TokenHash, tenant, connection, correlation).Scan(&changed)
	if err != nil || !changed || materialErr != nil {
		return changed, false, err
	}
	telemetry.Event(ctx, "disconnect", provider)
	plain, err := s.box.Open(cipher, nonce, tenant, connection, provider, 1)
	if err != nil {
		return true, false, nil
	}
	var tokens providerconn.Tokens
	if json.Unmarshal(plain, &tokens) != nil {
		return true, false, nil
	}
	revokeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	return true, s.providerClient().Revoke(revokeCtx, provider, tokens) == nil, nil
}
