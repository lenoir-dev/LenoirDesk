package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v86"
)

var ErrStripeSetup = errors.New("stripe Test Mode setup rejected")

var (
	stripeRestrictedKey = regexp.MustCompile(`^rk_test_[A-Za-z0-9_\-]{16,}$`)
	stripeSigningKey    = regexp.MustCompile(`^whsec_[A-Za-z0-9_\-]{16,}$`)
	stripeAccountID     = regexp.MustCompile(`^acct_[A-Za-z0-9]+$`)
	stripeWebhookRef    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type StripeTestSetup struct {
	ExpectedAccountID string `json:"expected_account_id"`
	RestrictedKey     string `json:"restricted_key"`
	WebhookSecret     string `json:"webhook_signing_secret"`
	WebhookRef        string `json:"webhook_ref"`
	ReconnectID       string `json:"reconnect_id,omitempty"`
}

func NewStripeWebhookRef() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ConnectStripeTest accepts only a restricted Test Mode key and a destination
// signing secret supplied by an owner. Verification uses fixed read-only URLs;
// the response body and both credentials are never returned to the caller.
func (s *Service) ConnectStripeTest(ctx context.Context, p Principal, tenant string, input StripeTestSetup, correlation string) (string, error) {
	if _, err := uuid.Parse(tenant); err != nil || !stripeAccountID.MatchString(input.ExpectedAccountID) ||
		!stripeRestrictedKey.MatchString(input.RestrictedKey) || !stripeSigningKey.MatchString(input.WebhookSecret) ||
		!stripeWebhookRef.MatchString(input.WebhookRef) || len(correlation) > 128 {
		return "", ErrStripeSetup
	}
	var existing *string
	connectionID := uuid.NewString()
	if input.ReconnectID != "" {
		if _, err := uuid.Parse(input.ReconnectID); err != nil {
			return "", ErrStripeSetup
		}
		connectionID = input.ReconnectID
		existing = &connectionID
	}
	// The owner check precedes network access. The SQL function repeats it in
	// the commit transaction, so a concurrent membership revocation fails shut.
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "members_write")
	if err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	if err = s.verifyStripeRestrictedKey(ctx, input.RestrictedKey, input.ExpectedAccountID); err != nil {
		return "", err
	}
	keyCipher, keyNonce, err := s.box.Seal([]byte(input.RestrictedKey), tenant, connectionID, "stripe", 1)
	if err != nil {
		return "", err
	}
	signCipher, signNonce, err := s.box.Seal([]byte(input.WebhookSecret), tenant, connectionID, "stripe", 1)
	if err != nil {
		return "", err
	}
	refHash := sha256.Sum256([]byte(input.WebhookRef))
	var committed string
	err = s.appDB.Pool.QueryRow(ctx, `SELECT stripe_test_connection_commit($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		p.TokenHash, tenant, input.ExpectedAccountID, existing, connectionID, refHash[:],
		keyCipher, keyNonce, signCipher, signNonce, correlation).Scan(&committed)
	if err != nil {
		return "", err
	}
	return committed, nil
}

func (s *Service) verifyStripeRestrictedKey(ctx context.Context, key, expected string) error {
	client := s.ProviderHTTP
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	for _, path := range []string{"/v1/account", "/v1/customers?limit=1", "/v1/subscriptions?limit=1", "/v1/invoices?limit=1"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.stripe.com"+path, nil)
		if err != nil {
			return ErrStripeSetup
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Stripe-Version", stripe.APIVersion)
		resp, err := client.Do(req)
		if err != nil {
			return ErrStripeSetup
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK || len(body) > 64*1024 {
			return ErrStripeSetup
		}
		if path == "/v1/account" {
			var account struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(body, &account) != nil || account.ID != expected {
				return ErrStripeSetup
			}
		}
	}
	return nil
}

func (s *Service) DisconnectStripeTest(ctx context.Context, p Principal, tenant, connection, correlation string) (bool, error) {
	if _, err := uuid.Parse(tenant); err != nil {
		return false, ErrForbidden
	}
	if _, err := uuid.Parse(connection); err != nil {
		return false, ErrStripeSetup
	}
	var changed bool
	err := s.appDB.Pool.QueryRow(ctx, `SELECT stripe_test_connection_disconnect($1,$2,$3,$4)`,
		p.TokenHash, tenant, connection, correlation).Scan(&changed)
	return changed, err
}
