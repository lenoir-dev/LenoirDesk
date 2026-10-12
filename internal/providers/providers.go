package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/stripe/stripe-go/v86"
)

// Failure is a safe, user-visible class. Never put response bodies or tokens in it.
type Outcome string

const (
	ConfirmedSuccess Outcome = "confirmed_success"
	SafeRetryable    Outcome = "safe_retryable"
	Permanent        Outcome = "permanent"
	BlockedAuth      Outcome = "blocked_auth"
	BlockedConfig    Outcome = "blocked_config"
	Ambiguous        Outcome = "ambiguous"
)

type Failure struct {
	Class      string
	Outcome    Outcome
	RetryAfter time.Duration
	Ambiguous  bool
}

func (f *Failure) Error() string                  { return f.Class }
func fail(class string) error                     { return &Failure{Class: class, Outcome: Permanent} }
func blocked(class string, outcome Outcome) error { return &Failure{Class: class, Outcome: outcome} }
func stripeReadFailure(err error) error {
	var already *Failure
	if errors.As(err, &already) {
		return already
	}
	var api *stripe.Error
	if errors.As(err, &api) {
		switch api.HTTPStatusCode {
		case 401:
			return blocked("stripe_read_auth", BlockedAuth)
		case 403:
			return blocked("stripe_read_permission", BlockedConfig)
		case 400, 404, 409, 422:
			return blocked("stripe_read_config_or_conflict", BlockedConfig)
		case 429:
			return &Failure{Class: "stripe_read_rate_limited", Outcome: SafeRetryable, RetryAfter: time.Second}
		}
	}
	return &Failure{Class: "stripe_read_retry", Outcome: SafeRetryable}
}

// Endpoints are trusted process configuration. They are never supplied by a tenant.
type Endpoints struct {
	Stripe, HubSpot, Slack string
	Simulation             bool
}

func (e Endpoints) Validate() error {
	for _, endpoint := range []string{e.Stripe, e.HubSpot, e.Slack} {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("invalid trusted provider endpoint")
		}
		if e.Simulation {
			if u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
				return errors.New("simulation endpoints must stay local")
			}
		} else if u.Scheme != "https" {
			return errors.New("real provider endpoints require HTTPS")
		}
	}
	if !e.Simulation && (e.Stripe != "https://api.stripe.com" || e.HubSpot != "https://api.hubapi.com" || e.Slack != "https://slack.com") {
		return errors.New("real provider hosts are fixed")
	}
	return nil
}

type Stripe struct {
	BaseURL string
	HTTP    *http.Client
}

func (s Stripe) Snapshot(ctx context.Context, token, account, mode, customerID string, tracked []string) (business.Projection, error) {
	key, err := business.CustomerKey(account, mode, customerID)
	if err != nil {
		return business.Projection{}, err
	}
	cfg := &stripe.BackendConfig{URL: stripe.String(s.BaseURL), HTTPClient: s.HTTP, MaxNetworkRetries: stripe.Int64(0), LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull}}
	client := stripe.NewClient(token, stripe.WithBackends(stripe.NewBackendsWithConfig(cfg)))
	customer, err := client.V1Customers.Retrieve(ctx, customerID, nil)
	if err != nil {
		return business.Projection{}, stripeReadFailure(err)
	}
	if customer == nil || customer.ID != customerID || customer.Deleted || customer.Livemode != (mode == "live") {
		return business.Projection{}, fail("stripe_identity_conflict")
	}
	params := &stripe.SubscriptionListParams{ListParams: stripe.ListParams{Context: ctx, Limit: stripe.Int64(100)}, Customer: stripe.String(customerID), Status: stripe.String("all")}
	iter := client.V1Subscriptions.List(ctx, params)
	matched := []*stripe.Subscription{}
	trackedSet := map[string]bool{}
	for _, v := range tracked {
		trackedSet[v] = true
	}
	for sub, pageErr := range iter.All(ctx) {
		if pageErr != nil {
			return business.Projection{}, stripeReadFailure(pageErr)
		}
		if sub == nil || sub.Customer == nil || sub.Customer.ID != customerID || sub.Livemode != (mode == "live") {
			return business.Projection{}, fail("stripe_identity_conflict")
		}
		qualifies := false
		if sub.Items != nil {
			for _, item := range sub.Items.Data {
				if item != nil && item.Price != nil && trackedSet[item.Price.ID] {
					qualifies = true
				}
			}
		}
		if qualifies {
			matched = append(matched, sub)
			if len(matched) > 1 {
				return business.Projection{}, fail("multiple_matching_subscriptions")
			}
		}
	}
	if iter.Err() != nil {
		return business.Projection{}, stripeReadFailure(iter.Err())
	}
	if len(matched) == 0 {
		return business.Projection{}, fail("no_matching_subscription")
	}
	sub := matched[0]
	p := business.Projection{CustomerKey: key, SubscriptionID: sub.ID, SubscriptionStatus: string(sub.Status), PaymentState: "unknown", CancelAtPeriodEnd: sub.CancelAtPeriodEnd, EffectivelyCanceled: sub.Status == stripe.SubscriptionStatusCanceled || sub.EndedAt > 0}
	if sub.Items != nil {
		for _, item := range sub.Items.Data {
			if item != nil && item.Price != nil && trackedSet[item.Price.ID] {
				p.PriceIDs = append(p.PriceIDs, item.Price.ID)
			}
		}
	}
	if sub.LatestInvoice != nil && sub.LatestInvoice.ID != "" {
		invoice, readErr := client.V1Invoices.Retrieve(ctx, sub.LatestInvoice.ID, nil)
		if readErr != nil {
			return business.Projection{}, stripeReadFailure(readErr)
		}
		if invoice == nil || invoice.Customer == nil || invoice.Customer.ID != customerID || invoice.Livemode != (mode == "live") {
			return business.Projection{}, fail("stripe_invoice_identity_conflict")
		}
		if invoice.Parent == nil || invoice.Parent.SubscriptionDetails == nil || invoice.Parent.SubscriptionDetails.Subscription == nil || invoice.Parent.SubscriptionDetails.Subscription.ID != sub.ID {
			return business.Projection{}, fail("stripe_invoice_subscription_conflict")
		}
		p.LatestInvoiceID = invoice.ID
		p.Currency = string(invoice.Currency)
		p.AmountPaidMinor = invoice.AmountPaid
		if sub.Status != stripe.SubscriptionStatusTrialing {
			switch invoice.Status {
			case stripe.InvoiceStatusPaid:
				p.PaymentState = "paid"
			case stripe.InvoiceStatusOpen:
				if invoice.Attempted {
					p.PaymentState = "payment_failed"
				}
			}
		}
	}
	return p, nil
}
func (s Stripe) PaidInvoice(ctx context.Context, token, invoiceID, customerID, subscriptionID, mode string) error {
	cfg := &stripe.BackendConfig{URL: stripe.String(s.BaseURL), HTTPClient: s.HTTP, MaxNetworkRetries: stripe.Int64(0), LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull}}
	client := stripe.NewClient(token, stripe.WithBackends(stripe.NewBackendsWithConfig(cfg)))
	invoice, err := client.V1Invoices.Retrieve(ctx, invoiceID, nil)
	if err != nil {
		return stripeReadFailure(err)
	}
	if invoice == nil || invoice.Status != stripe.InvoiceStatusPaid || invoice.Livemode != (mode == "live") || invoice.Customer == nil || invoice.Customer.ID != customerID || invoice.Parent == nil || invoice.Parent.SubscriptionDetails == nil || invoice.Parent.SubscriptionDetails.Subscription == nil || invoice.Parent.SubscriptionDetails.Subscription.ID != subscriptionID {
		return fail("invoice_paid_fact_unverified")
	}
	return nil
}

type Contact struct {
	ID         string            `json:"id"`
	Archived   bool              `json:"archived"`
	Properties map[string]string `json:"properties"`
}

// HubSpot reports a duplicate value for a unique Contact property as HTTP 400
// VALIDATION_ERROR, not consistently as 409/CONFLICT. Require both the exact
// managed identity property and the duplicate-value phrase before treating it
// as an identity conflict; other validation errors remain configuration errors.
func hubSpotUniqueConflict(body []byte) bool {
	var payload struct {
		Category string `json:"category"`
		Message  string `json:"message"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.Category == "VALIDATION_ERROR" &&
		strings.Contains(payload.Message, "propertyName=relaydesk_customer_key") &&
		strings.Contains(payload.Message, "already has that value")
}

type HubSpot struct {
	BaseURL, Token string
	HTTP           *http.Client
}

var managedKeys = []string{"relaydesk_customer_key", "relaydesk_subscription_id", "relaydesk_subscription_status", "relaydesk_payment_state", "relaydesk_latest_invoice_id", "relaydesk_cancel_at_period_end", "relaydesk_effectively_canceled", "relaydesk_projection_hash", "relaydesk_synced_at"}

func (h HubSpot) Read(ctx context.Context, keyOrID string, unique bool) (Contact, bool, error) {
	u := h.BaseURL + "/crm/v3/objects/contacts/" + url.PathEscape(keyOrID)
	q := url.Values{}
	if unique {
		q.Set("idProperty", "relaydesk_customer_key")
	}
	for _, name := range managedKeys {
		q.Add("properties", name)
	}
	u += "?" + q.Encode()
	body, status, _, err := h.call(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Contact{}, false, err
	}
	if status == 404 {
		return Contact{}, false, nil
	}
	if status != 200 {
		return Contact{}, false, classify(status, "hubspot_read", false, "")
	}
	var c Contact
	if json.Unmarshal(body, &c) != nil || c.ID == "" || c.Archived {
		return Contact{}, false, fail("hubspot_invalid_contact")
	}
	return c, true, nil
}
func (h HubSpot) Upsert(ctx context.Context, key string, fields map[string]string) (Contact, error) {
	payload := map[string]any{"inputs": []any{map[string]any{"id": key, "idProperty": "relaydesk_customer_key", "properties": fields}}}
	raw, _ := json.Marshal(payload)
	body, status, header, err := h.call(ctx, http.MethodPost, h.BaseURL+"/crm/v3/objects/contacts/batch/upsert", raw)
	if err != nil {
		var admission *Failure
		if errors.As(err, &admission) {
			return Contact{}, admission
		}
		if definitelyNotSent(err) {
			return Contact{}, &Failure{Class: "hubspot_connect_retry", Outcome: SafeRetryable}
		}
		return Contact{}, &Failure{Class: "hubspot_write_ambiguous", Outcome: Ambiguous, Ambiguous: true}
	}
	if status == 207 {
		var partial struct {
			Errors []struct {
				Category string `json:"category"`
				Status   string `json:"status"`
			} `json:"errors"`
		}
		if json.Unmarshal(body, &partial) == nil {
			for _, item := range partial.Errors {
				if item.Category == "CONFLICT" {
					return Contact{}, fail("hubspot_identity_conflict")
				}
				if item.Category == "VALIDATION_ERROR" || item.Status == "INVALID_REQUEST" {
					return Contact{}, blocked("hubspot_schema_invalid_config", BlockedConfig)
				}
			}
		}
		return Contact{}, &Failure{Class: "hubspot_write_ambiguous", Outcome: Ambiguous, Ambiguous: true}
	}
	if status != 200 {
		if status == 409 {
			return Contact{}, fail("hubspot_identity_conflict")
		}
		if status == 400 && hubSpotUniqueConflict(body) {
			return Contact{}, fail("hubspot_identity_conflict")
		}
		if status == 400 || status == 422 {
			return Contact{}, blocked("hubspot_schema_invalid_config", BlockedConfig)
		}
		return Contact{}, classify(status, "hubspot_write", true, header.Get("Retry-After"))
	}
	var out struct {
		Status  string            `json:"status"`
		Results []Contact         `json:"results"`
		Errors  []json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(body, &out) != nil || out.Status != "COMPLETE" || len(out.Results) != 1 || len(out.Errors) > 0 || out.Results[0].ID == "" || out.Results[0].Archived {
		return Contact{}, &Failure{Class: "hubspot_write_ambiguous", Outcome: Ambiguous, Ambiguous: true}
	}
	return out.Results[0], nil
}
func (h HubSpot) Update(ctx context.Context, id string, fields map[string]string) (Contact, error) {
	raw, _ := json.Marshal(map[string]any{"properties": fields})
	body, status, header, err := h.call(ctx, http.MethodPatch, h.BaseURL+"/crm/v3/objects/contacts/"+url.PathEscape(id), raw)
	if err != nil {
		var admission *Failure
		if errors.As(err, &admission) {
			return Contact{}, admission
		}
		if definitelyNotSent(err) {
			return Contact{}, &Failure{Class: "hubspot_connect_retry", Outcome: SafeRetryable}
		}
		return Contact{}, &Failure{Class: "hubspot_write_ambiguous", Outcome: Ambiguous, Ambiguous: true}
	}
	if status != 200 {
		if status == 409 {
			return Contact{}, fail("hubspot_identity_conflict")
		}
		if status == 400 && hubSpotUniqueConflict(body) {
			return Contact{}, fail("hubspot_identity_conflict")
		}
		if status == 400 || status == 422 {
			return Contact{}, fail("hubspot_schema_invalid_config")
		}
		return Contact{}, classify(status, "hubspot_write", true, header.Get("Retry-After"))
	}
	var c Contact
	if json.Unmarshal(body, &c) != nil || c.ID != id || c.Archived {
		return Contact{}, &Failure{Class: "hubspot_write_ambiguous", Outcome: Ambiguous, Ambiguous: true}
	}
	return c, nil
}
func (h HubSpot) call(ctx context.Context, method, u string, body []byte) ([]byte, int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, resp.Header, err
}

type Slack struct {
	BaseURL, Token string
	HTTP           *http.Client
}

func (s Slack) Post(ctx context.Context, channel, text string) (string, string, error) {
	raw, _ := json.Marshal(map[string]any{"channel": channel, "text": text, "unfurl_links": false, "unfurl_media": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/api/chat.postMessage", bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		var admission *Failure
		if errors.As(err, &admission) {
			return "", "", admission
		}
		if definitelyNotSent(err) {
			return "", "", &Failure{Class: "slack_connect_retry", Outcome: SafeRetryable}
		}
		return "", "", &Failure{Class: "slack_result_unknown", Outcome: Ambiguous, Ambiguous: true}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", &Failure{Class: "slack_result_unknown", Outcome: Ambiguous, Ambiguous: true}
	}
	if resp.StatusCode != 200 {
		return "", "", classify(resp.StatusCode, "slack_post", true, resp.Header.Get("Retry-After"))
	}
	var result struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if json.Unmarshal(body, &result) != nil {
		return "", "", &Failure{Class: "slack_result_unknown", Outcome: Ambiguous, Ambiguous: true}
	}
	if !result.OK {
		switch result.Error {
		case "invalid_auth", "account_inactive":
			return "", "", blocked("slack_auth", BlockedAuth)
		case "channel_not_found", "not_in_channel":
			return "", "", fail("slack_permission_or_channel")
		case "missing_scope", "no_permission":
			return "", "", blocked("slack_permission_or_channel", BlockedConfig)
		default:
			return "", "", &Failure{Class: "slack_result_unknown", Outcome: Ambiguous, Ambiguous: true}
		}
	}
	if result.Channel != channel || result.TS == "" {
		return "", "", &Failure{Class: "slack_result_unknown", Outcome: Ambiguous, Ambiguous: true}
	}
	return result.Channel, result.TS, nil
}
func classify(status int, prefix string, wasWrite bool, retryAfter string) error {
	if status == 401 || status == 403 {
		if status == 401 {
			return blocked(prefix+"_auth", BlockedAuth)
		}
		return blocked(prefix+"_permission", BlockedConfig)
	}
	if status == 400 || status == 404 || status == 409 || status == 422 {
		return blocked(prefix+"_config_or_conflict", BlockedConfig)
	}
	if status == 429 {
		duration := time.Second
		if n, err := strconv.Atoi(retryAfter); err == nil && n > 0 && n <= 3600 {
			duration = time.Duration(n) * time.Second
		}
		return &Failure{Class: prefix + "_rate_limited", Outcome: SafeRetryable, RetryAfter: duration}
	}
	if wasWrite {
		return &Failure{Class: prefix + "_ambiguous", Outcome: Ambiguous, Ambiguous: true}
	}
	return &Failure{Class: prefix + "_retry", Outcome: SafeRetryable}
}
func SafeClass(err error) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	return &Failure{Class: fmt.Sprintf("internal_%T", err), Outcome: SafeRetryable}
}

// Dial and DNS failures happen before the HTTP transport can transmit a
// request. Timeouts, connection resets and response-read errors remain
// ambiguous: the provider may already have applied the write.
func definitelyNotSent(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
