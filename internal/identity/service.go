package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/lenoir-dev/LenoirDesk/internal/config"
	"github.com/lenoir-dev/LenoirDesk/internal/crypto"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

const (
	SessionLifetime = 12 * time.Hour
	SessionIdle     = 30 * time.Minute
	FlowLifetime    = 10 * time.Minute
)

var ErrInvalidFlow = errors.New("invalid or expired OIDC flow")
var ErrUnauthenticated = errors.New("session unavailable")
var ErrAdmissionLimited = errors.New("OIDC admission limited")
var ErrInvalidAdmissionSource = errors.New("invalid network peer")

type Principal struct {
	UserID    string
	Email     string
	Issuer    string
	TokenHash []byte
	ExpiresAt time.Time
}

type Service struct {
	db       *store.Store
	appDB    *store.Store
	box      *crypto.Box
	key      []byte
	config   config.Config
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
	// ProviderHTTP is an optional HTTP test transport; production leaves it nil.
	ProviderHTTP *http.Client
}

func New(ctx context.Context, db, appDB *store.Store, box *crypto.Box, cfg config.Config) (*Service, error) {
	if err := cfg.ValidateAPI(); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if cfg.OIDCBackchannelAddr != "" {
		upstream := transport.DialContext
		if upstream == nil {
			upstream = (&net.Dialer{Timeout: 3 * time.Second}).DialContext
		}
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if address == "127.0.0.1:5556" {
				address = cfg.OIDCBackchannelAddr
			}
			return upstream(ctx, network, address)
		}
	}
	client := &http.Client{Transport: transport, Timeout: 6 * time.Second}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), cfg.OIDCIssuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery failed: %w", err)
	}
	csrfKey := hmac.New(sha256.New, cfg.EncryptionKey)
	_, _ = csrfKey.Write([]byte("relaydesk-csrf-key-v1"))
	return &Service{
		db: db, appDB: appDB, box: box, key: csrfKey.Sum(nil), config: cfg,
		oauth: oauth2.Config{ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
			RedirectURL: cfg.OIDCRedirectURL, Endpoint: provider.Endpoint(),
			Scopes: []string{oidc.ScopeOpenID, "email"}},
		verifier: provider.VerifierContext(oidc.ClientContext(context.Background(), client),
			&oidc.Config{ClientID: cfg.OIDCClientID}),
		client: client,
	}, nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// SourceFingerprint binds admission to the TCP peer only. Forwarded headers are
// deliberately ignored until a trusted proxy contract is explicitly configured.
// HMAC prevents the database from retaining raw or directly reversible IPs.
func (s *Service) SourceFingerprint(remoteAddr string) ([]byte, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return nil, ErrInvalidAdmissionSource
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil, ErrInvalidAdmissionSource
	}
	mac := hmac.New(sha256.New, s.config.EncryptionKey)
	_, _ = mac.Write([]byte("oidc-admission-source-v1|"))
	_, _ = mac.Write(ip.Unmap().AsSlice())
	return mac.Sum(nil), nil
}

func (s *Service) Begin(ctx context.Context, remoteAddr string) (redirect, state string, err error) {
	source, err := s.SourceFingerprint(remoteAddr)
	if err != nil {
		return "", "", err
	}
	state, err = randomToken()
	if err != nil {
		return "", "", err
	}
	nonce, err := randomToken()
	if err != nil {
		return "", "", err
	}
	verifier := oauth2.GenerateVerifier()
	ciphertext, iv, err := s.box.SealPurpose([]byte(verifier), "oidc-flow|"+state)
	if err != nil {
		return "", "", err
	}
	_, err = s.db.Pool.Exec(ctx, `SELECT oidc_admit_begin($1,$2,$3,$4,$5,now()+interval '10 minutes')`,
		source, hash(state), ciphertext, iv, hash(nonce))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "P5301" {
			return "", "", ErrAdmissionLimited
		}
		return "", "", err
	}
	return s.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), state, nil
}

// CleanupExpiredFlows runs at startup and every minute in each API instance.
// The SQL function limits each table to 500 deletions per call.
func (s *Service) CleanupExpiredFlows(ctx context.Context) (int, error) {
	var removed int
	err := s.db.Pool.QueryRow(ctx, `SELECT oauth_flow_cleanup(500)`).Scan(&removed)
	return removed, err
}

// Complete consumes the flow before exchanging the authorization code, so every
// callback attempt is one-shot even if the provider or PostgreSQL later fails.
func (s *Service) Complete(ctx context.Context, state, code, oldSession string) (newSession string, principal Principal, err error) {
	if len(state) != 43 || code == "" || len(code) > 4096 {
		return "", Principal{}, ErrInvalidFlow
	}
	var ciphertext, iv, nonceHash []byte
	err = s.db.Pool.QueryRow(ctx, `SELECT verifier_ciphertext,verifier_nonce,nonce_hash FROM oidc_consume($1)`, hash(state)).Scan(&ciphertext, &iv, &nonceHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", Principal{}, ErrInvalidFlow
	}
	if err != nil {
		return "", Principal{}, err
	}
	verifier, err := s.box.OpenPurpose(ciphertext, iv, "oidc-flow|"+state)
	if err != nil {
		return "", Principal{}, ErrInvalidFlow
	}
	exchangeCtx, cancel := context.WithTimeout(oidc.ClientContext(ctx, s.client), 6*time.Second)
	defer cancel()
	token, err := s.oauth.Exchange(exchangeCtx, code, oauth2.VerifierOption(string(verifier)))
	if err != nil {
		var denied *oauth2.RetrieveError
		if errors.As(err, &denied) && denied.Response != nil && denied.Response.StatusCode < 500 {
			return "", Principal{}, ErrInvalidFlow
		}
		return "", Principal{}, err
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return "", Principal{}, ErrInvalidFlow
	}
	idToken, err := s.verifier.Verify(oidc.ClientContext(ctx, s.client), rawIDToken)
	if err != nil {
		var networkErr net.Error
		if errors.As(err, &networkErr) {
			return "", Principal{}, err
		}
		return "", Principal{}, ErrInvalidFlow
	}
	var claims struct {
		Nonce string `json:"nonce"`
		Email string `json:"email"`
		AZP   string `json:"azp"`
	}
	if err := idToken.Claims(&claims); err != nil || claims.Nonce == "" ||
		!hmac.Equal(hash(claims.Nonce), nonceHash) ||
		(len(idToken.Audience) > 1 && claims.AZP != s.config.OIDCClientID) ||
		(claims.AZP != "" && claims.AZP != s.config.OIDCClientID) ||
		idToken.Subject == "" || len(idToken.Subject) > 512 || len(claims.Email) > 320 {
		return "", Principal{}, ErrInvalidFlow
	}
	var userID string
	err = s.db.Pool.QueryRow(ctx, `SELECT oidc_user($1,$2,$3)`, idToken.Issuer, idToken.Subject, claims.Email).Scan(&userID)
	if err != nil {
		return "", Principal{}, err
	}
	newSession, err = randomToken()
	if err != nil {
		return "", Principal{}, err
	}
	var oldHash []byte
	if oldSession != "" {
		oldHash = hash(oldSession)
	}
	var expires time.Time
	err = s.db.Pool.QueryRow(ctx, `SELECT session_rotate($1,$2,$3,now()+interval '12 hours'), now()+interval '12 hours'`,
		oldHash, hash(newSession), userID).Scan(new(any), &expires)
	if err != nil {
		return "", Principal{}, err
	}
	return newSession, Principal{UserID: userID, Email: claims.Email, Issuer: idToken.Issuer,
		TokenHash: hash(newSession), ExpiresAt: expires}, nil
}

func (s *Service) Current(ctx context.Context, token string) (Principal, error) {
	if len(token) != 43 { // 32 random bytes in unpadded base64url.
		return Principal{}, ErrUnauthenticated
	}
	var p Principal
	p.TokenHash = hash(token)
	err := s.appDB.Pool.QueryRow(ctx, `SELECT user_id,coalesce(email,''),oidc_issuer,expires_at FROM session_current($1)`, p.TokenHash).
		Scan(&p.UserID, &p.Email, &p.Issuer, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthenticated
	}
	return p, err
}

func (s *Service) Revoke(ctx context.Context, token string) error {
	if len(token) != 43 {
		return nil
	}
	_, err := s.appDB.Pool.Exec(ctx, `SELECT session_revoke($1)`, hash(token))
	return err
}

func (s *Service) CSRF(p Principal) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("relaydesk-csrf-v1|"))
	_, _ = mac.Write(p.TokenHash)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Service) CheckCSRF(p Principal, supplied string) bool {
	want := s.CSRF(p)
	return len(supplied) == len(want) && hmac.Equal([]byte(supplied), []byte(want))
}
