package api

import (
	"strings"
	"testing"
)

func TestProviderLinksAreConstrainedToKnownDestinations(t *testing.T) {
	if got := safeStripeEventURL("evt_ABC123"); got != "https://dashboard.stripe.com/test/events/evt_ABC123" {
		t.Fatalf("Stripe link: %q", got)
	}
	if got := safeHubSpotContactURL("123456789", "123456"); got != "https://app.hubspot.com/contacts/123456789/record/0-1/123456" {
		t.Fatalf("HubSpot link: %q", got)
	}
	if got := safeSlackMessageURL("C0123456789", "1791649343.166299"); got != "https://app.slack.com/archives/C0123456789/p1791649343166299" {
		t.Fatalf("Slack link: %q", got)
	}
	for _, got := range []string{
		safeStripeEventURL("evt_bad/../../live"),
		safeHubSpotContactURL("123456789@evil.example", "123"),
		safeHubSpotContactURL("123456789", "12?x=1"),
		safeSlackMessageURL("C0123456789/evil", "1791649343.166299"),
		safeSlackMessageURL("C0123456789", "javascript:1"),
	} {
		if got != "" {
			t.Fatalf("unexpected external link: %q", got)
		}
	}
}

func TestUnknownProviderErrorCannotBecomeBrowserContent(t *testing.T) {
	for _, raw := range []string{"token=synthetic-private-value", "https://private.example/body", strings.Repeat("x", 100)} {
		if got := safeErrorLabel(raw); strings.Contains(got, raw) {
			t.Fatalf("unsafe error label: %q", got)
		}
	}
}
