package providers

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

func TestWriteTransportFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		outcome Outcome
	}{
		{"dial", &net.OpError{Op: "dial", Err: errors.New("refused")}, SafeRetryable},
		{"dns", &net.DNSError{Err: "no such host", Name: "example.test"}, SafeRetryable},
		{"read_or_transmission", io.ErrUnexpectedEOF, Ambiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: failingTransport{err: tc.err}}
			_, _, slackErr := (Slack{BaseURL: "https://example.test", Token: "synthetic", HTTP: client}).Post(context.Background(), "C123", "test")
			if got := SafeClass(slackErr).Outcome; got != tc.outcome {
				t.Fatalf("Slack outcome = %s, want %s", got, tc.outcome)
			}
			_, hubErr := (HubSpot{BaseURL: "https://example.test", Token: "synthetic", HTTP: client}).Upsert(context.Background(), "key", map[string]string{"relaydesk_customer_key": "key"})
			if got := SafeClass(hubErr).Outcome; got != tc.outcome {
				t.Fatalf("HubSpot outcome = %s, want %s", got, tc.outcome)
			}
		})
	}
}

type staticResponseTransport struct {
	status int
	body   string
}

func (t staticResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: t.status, Body: io.NopCloser(strings.NewReader(t.body)), Header: make(http.Header)}, nil
}

func TestHubSpotUniqueConflictFromValidationError(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"duplicate unique key", `{"category":"VALIDATION_ERROR","message":"Cannot set PropertyValueCoordinates{propertyName=relaydesk_customer_key, value=synthetic} on 2. 1 already has that value."}`, "hubspot_identity_conflict"},
		{"other validation", `{"category":"VALIDATION_ERROR","message":"propertyName=relaydesk_subscription_id is invalid"}`, "hubspot_schema_invalid_config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := HubSpot{BaseURL: "https://example.test", Token: "synthetic", HTTP: &http.Client{Transport: staticResponseTransport{status: 400, body: tc.body}}}
			_, err := adapter.Update(context.Background(), "2", map[string]string{"relaydesk_customer_key": "synthetic"})
			if got := SafeClass(err).Class; got != tc.want {
				t.Fatalf("Update class = %s, want %s", got, tc.want)
			}
		})
	}
}
