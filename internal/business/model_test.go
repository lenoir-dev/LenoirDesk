package business

import (
	"testing"
	"time"
)

func TestProjectionHashStableAndSensitive(t *testing.T) {
	p := Projection{CustomerKey: "stripe:acct_1:test:cus_1", PriceIDs: []string{"price_b", "price_a"}, PaymentState: "paid"}
	q := p
	q.PriceIDs = []string{"price_a", "price_b"}
	if p.Hash() != q.Hash() {
		t.Fatal("unordered price IDs changed canonical hash")
	}
	q.PaymentState = "payment_failed"
	if p.Hash() == q.Hash() {
		t.Fatal("business state change did not change hash")
	}
}

func TestConfigValidation(t *testing.T) {
	c := Config{Template: Template, StripeConnectionID: "a", HubSpotConnectionID: "b", SlackConnectionID: "c", SlackChannelID: "C123", TrackedPriceIDs: []string{"price_123"}, CreationPolicy: "unique_upsert", ActivationTime: time.Now().UTC()}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.TrackedPriceIDs = []string{"price_123", "price_123"}
	if c.Validate() == nil {
		t.Fatal("duplicate price accepted")
	}
}
