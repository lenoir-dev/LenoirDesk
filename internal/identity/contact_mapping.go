package identity

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lenoir-dev/LenoirDesk/internal/providerconn"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

var (
	ErrContactMissing  = errors.New("contact missing or archived")
	ErrContactConflict = errors.New("contact identity conflict")
	contactIDPattern   = regexp.MustCompile(`^[0-9]{1,32}$`)
	customerKeyPattern = regexp.MustCompile(`^stripe:(acct_[A-Za-z0-9_-]+):test:cus_[A-Za-z0-9_-]+$`)
)

type ContactPreview struct {
	ID             string `json:"id"`
	CustomerKey    string `json:"customer_key"`
	IdentityStatus string `json:"identity_status"`
}

func (s *Service) PreviewContact(ctx context.Context, p Principal, tenant, hubspot, key, contactID string) (ContactPreview, error) {
	if !contactIDPattern.MatchString(contactID) || !customerKeyPattern.MatchString(key) {
		return ContactPreview{}, ErrContactConflict
	}
	// Keep the membership lock until the credential and provider identity are
	// checked. The actual bind repeats authorization inside its SQL function.
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "operate")
	if err != nil {
		return ContactPreview{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var stripeBound bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM connections WHERE tenant_id=$1 AND provider='stripe'
 AND provider_account_id=$2 AND mode='test' AND status='active')`, tenant, customerKeyPattern.FindStringSubmatch(key)[1]).Scan(&stripeBound); err != nil {
		return ContactPreview{}, err
	}
	if !stripeBound {
		return ContactPreview{}, ErrContactConflict
	}
	var mode, environment, health string
	var cipher, nonce []byte
	var version int
	err = s.appDB.Pool.QueryRow(ctx, `SELECT mode,environment,health,ciphertext,nonce,key_version
 FROM contact_preview_material($1,$2,$3)`, p.TokenHash, tenant, hubspot).
		Scan(&mode, &environment, &health, &cipher, &nonce, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return ContactPreview{}, store.ErrNotFound
	}
	if err != nil {
		return ContactPreview{}, err
	}
	if health != "verified" || mode != "test" || (environment != "test" && environment != "simulation") {
		return ContactPreview{}, ErrContactConflict
	}
	plain, err := s.box.Open(cipher, nonce, tenant, hubspot, "hubspot", version)
	if err != nil {
		return ContactPreview{}, ErrContactConflict
	}
	token := string(plain)
	if environment == "test" {
		var issued providerconn.Tokens
		if json.Unmarshal(plain, &issued) != nil || issued.AccessToken == "" ||
			(issued.Rotation == "rotating" && issued.ExpiresAt.Before(time.Now().Add(15*time.Second))) {
			return ContactPreview{}, ErrContactConflict
		}
		token = issued.AccessToken
	}
	base := "https://api.hubapi.com"
	if environment == "simulation" {
		base = s.config.HubSpotAPIURL
		if base == "" {
			return ContactPreview{}, ErrContactConflict
		}
	}
	client := s.ProviderHTTP
	if client == nil {
		client = s.client
	}
	contact, found, err := (providers.HubSpot{BaseURL: base, Token: token, HTTP: client}).Read(ctx, contactID, false)
	if err != nil {
		return ContactPreview{}, err
	}
	if !found {
		return ContactPreview{}, ErrContactMissing
	}
	out := ContactPreview{ID: contact.ID, CustomerKey: contact.Properties["relaydesk_customer_key"], IdentityStatus: "unclaimed"}
	if out.CustomerKey == key {
		out.IdentityStatus = "matching"
	} else if out.CustomerKey != "" {
		out.IdentityStatus = "conflict"
	}
	return out, tx.Commit(ctx)
}
