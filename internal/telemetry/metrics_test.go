package telemetry

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrometheusAggregateScrapeHasNoObjectLabels(t *testing.T) {
	t.Setenv("RELAYDESK_METRICS_ADDR", "127.0.0.1:0")
	shutdown, err := Setup()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	UpdateOperational(map[string]int64{"unpublished_outbox": 7, "coverage_gaps": 2})
	SetWorkerOutboundEnabled(false)
	Event(context.Background(), "blocked_auth", "hubspot")
	response := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if response.Code != 200 {
		t.Fatalf("scrape status=%d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{"relaydesk_operational_value", `relaydesk_worker_outbound_enabled{`, `measure="unpublished_outbox"`,
		`measure="coverage_gaps"`, `kind="blocked_auth"`, `provider="hubspot"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing aggregate metric %q", expected)
		}
	}
	if !hasWorkerGauge(body, "0") {
		t.Fatal("disabled worker gauge missing")
	}
	SetWorkerOutboundEnabled(true)
	response = httptest.NewRecorder()
	MetricsHandler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if !hasWorkerGauge(response.Body.String(), "1") {
		t.Fatal("enabled worker gauge missing")
	}
	SetWorkerOutboundEnabled(false)
	for _, forbidden := range []string{"tenant_id=", "customer_id=", "invoice_id=", "access_token="} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("unbounded or confidential metric label %q", forbidden)
		}
	}
}

func hasWorkerGauge(body, value string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "relaydesk_worker_outbound_enabled{") && strings.HasSuffix(line, "} "+value) {
			return true
		}
	}
	return false
}
