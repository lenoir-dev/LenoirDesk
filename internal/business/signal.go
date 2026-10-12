package business

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Signal struct {
	EventID, Type, CustomerID, InvoiceID, SubscriptionID string
	Created                                              time.Time
}

func ParseSignal(raw []byte) (Signal, error) {
	var envelope struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Created int64  `json:"created"`
		Data    struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.ID == "" || envelope.Created <= 0 || len(envelope.Data.Object) == 0 {
		return Signal{}, errors.New("missing Stripe signal identity")
	}
	var object struct {
		ID       string          `json:"id"`
		Customer json.RawMessage `json:"customer"`
		Parent   struct {
			SubscriptionDetails struct {
				Subscription json.RawMessage `json:"subscription"`
			} `json:"subscription_details"`
		} `json:"parent"`
	}
	if json.Unmarshal(envelope.Data.Object, &object) != nil || object.ID == "" {
		return Signal{}, errors.New("invalid Stripe signal object")
	}
	customer := externalObjectID(object.Customer)
	if !strings.HasPrefix(customer, "cus_") {
		return Signal{}, errors.New("missing Stripe customer")
	}
	s := Signal{EventID: envelope.ID, Type: envelope.Type, CustomerID: customer, Created: time.Unix(envelope.Created, 0).UTC()}
	switch envelope.Type {
	case "invoice.paid", "invoice.payment_failed":
		if !strings.HasPrefix(object.ID, "in_") {
			return Signal{}, errors.New("invoice ID required")
		}
		s.InvoiceID = object.ID
		s.SubscriptionID = externalObjectID(object.Parent.SubscriptionDetails.Subscription)
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		if !strings.HasPrefix(object.ID, "sub_") {
			return Signal{}, errors.New("subscription ID required")
		}
		s.SubscriptionID = object.ID
	default:
		return Signal{}, fmt.Errorf("unsupported signal type")
	}
	return s, nil
}
func externalObjectID(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return id
	}
	var obj struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.ID
	}
	return ""
}
