package business

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const Template = "subscription_billing_sync.v1"

var externalID = regexp.MustCompile(`^[A-Za-z0-9_\-]{3,160}$`)

// Config is immutable once a revision is activated. Connection identities are
// checked against tenant-owned rows by the application service, not this type.
type Config struct {
	Template            string    `json:"template"`
	StripeConnectionID  string    `json:"stripe_connection_id"`
	HubSpotConnectionID string    `json:"hubspot_connection_id"`
	SlackConnectionID   string    `json:"slack_connection_id"`
	SlackChannelID      string    `json:"slack_channel_id"`
	TrackedPriceIDs     []string  `json:"tracked_price_ids"`
	CreationPolicy      string    `json:"creation_policy"`
	ActivationTime      time.Time `json:"activation_time"`
}

func (c Config) Validate() error {
	if c.Template != Template || c.StripeConnectionID == "" || c.HubSpotConnectionID == "" || c.SlackConnectionID == "" {
		return errors.New("template and three connections are required")
	}
	if !externalID.MatchString(c.SlackChannelID) || len(c.TrackedPriceIDs) == 0 || len(c.TrackedPriceIDs) > 20 {
		return errors.New("a Slack channel and 1–20 tracked prices are required")
	}
	if c.CreationPolicy != "explicit_only" && c.CreationPolicy != "unique_upsert" {
		return errors.New("unsupported Contact creation policy")
	}
	if c.ActivationTime.IsZero() || c.ActivationTime.Location() == nil {
		return errors.New("activation timestamp is required")
	}
	seen := map[string]bool{}
	for _, id := range c.TrackedPriceIDs {
		if !strings.HasPrefix(id, "price_") || !externalID.MatchString(id) || seen[id] {
			return errors.New("tracked price IDs must be distinct Stripe price IDs")
		}
		seen[id] = true
	}
	return nil
}

// Projection contains business state only; observed-at and sync-at timestamps
// never enter the hash, so identical current Stripe state is a CRM no-op.
type Projection struct {
	CustomerKey         string   `json:"customer_key"`
	SubscriptionID      string   `json:"subscription_id"`
	SubscriptionStatus  string   `json:"subscription_status"`
	PaymentState        string   `json:"payment_state"`
	LatestInvoiceID     string   `json:"latest_invoice_id"`
	CancelAtPeriodEnd   bool     `json:"cancel_at_period_end"`
	EffectivelyCanceled bool     `json:"effectively_canceled"`
	PriceIDs            []string `json:"price_ids"`
	Currency            string   `json:"currency"`
	AmountPaidMinor     int64    `json:"amount_paid_minor"`
}

func CustomerKey(account, mode, customer string) (string, error) {
	if !strings.HasPrefix(account, "acct_") || !strings.HasPrefix(customer, "cus_") ||
		!externalID.MatchString(account) || !externalID.MatchString(customer) || (mode != "test" && mode != "live") {
		return "", errors.New("invalid Stripe customer identity")
	}
	return fmt.Sprintf("stripe:%s:%s:%s", account, mode, customer), nil
}

func (p Projection) Hash() string {
	p.PriceIDs = append([]string(nil), p.PriceIDs...)
	sort.Strings(p.PriceIDs)
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (p Projection) Managed() map[string]string {
	return map[string]string{
		"relaydesk_customer_key":         p.CustomerKey,
		"relaydesk_subscription_id":      p.SubscriptionID,
		"relaydesk_subscription_status":  p.SubscriptionStatus,
		"relaydesk_payment_state":        p.PaymentState,
		"relaydesk_latest_invoice_id":    p.LatestInvoiceID,
		"relaydesk_cancel_at_period_end": fmt.Sprint(p.CancelAtPeriodEnd),
		"relaydesk_effectively_canceled": fmt.Sprint(p.EffectivelyCanceled),
		"relaydesk_projection_hash":      p.Hash(),
	}
}

func InvoiceEffectKey(tenant, account, mode, invoice, workspace, channel string) string {
	return strings.Join([]string{tenant, account, mode, invoice, "invoice-paid", workspace, channel}, "|")
}
