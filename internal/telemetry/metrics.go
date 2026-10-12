package telemetry

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var meter = otel.Meter("relaydesk/provider")
var events, _ = meter.Int64Counter("relaydesk_provider_events_total")
var waits, _ = meter.Float64Histogram("relaydesk_provider_wait_seconds")
var queueAge, _ = meter.Float64Histogram("relaydesk_queue_age_seconds")
var providerLatency, _ = meter.Float64Histogram("relaydesk_provider_request_seconds")
var providerRequests, _ = meter.Int64Counter("relaydesk_provider_requests_total")
var webhookLatency, _ = meter.Float64Histogram("relaydesk_webhook_accept_seconds")
var webhookRequests, _ = meter.Int64Counter("relaydesk_webhook_requests_total")
var inboxDuplicates, _ = meter.Int64Counter("relaydesk_inbox_duplicates_total")
var actionOutcomes, _ = meter.Int64Counter("relaydesk_business_action_outcomes_total")
var crmDuration, _ = meter.Float64Histogram("relaydesk_crm_sync_seconds")
var workerInFlight atomic.Int64
var workerOutboundEnabled atomic.Int64
var operationalGauge, _ = meter.Int64ObservableGauge("relaydesk_operational_value")
var workerOutboundGauge, _ = meter.Int64ObservableGauge("relaydesk_worker_outbound_enabled")
var sample = &atomic.Pointer[OperationalSnapshot]{}

// OperationalSnapshot contains aggregate values only. Categories are a fixed
// enum, so Prometheus cardinality is independent of tenant and object count.
type OperationalSnapshot struct {
	At     time.Time
	Values map[string]int64
}

var operationalMeasures = []string{
	"due_actions", "oldest_due_seconds", "unpublished_outbox", "oldest_outbox_seconds",
	"active_leases", "expired_leases", "stalled_actions", "dead_actions", "unknown_actions",
	"oldest_unknown_seconds", "retries_24h", "oldest_dead_seconds", "oldest_pending_job_seconds",
	"coverage_gaps", "oldest_reconcile_lag_seconds", "unhealthy_connections", "storage_bytes",
	"succeeded_actions_24h", "failed_actions_24h", "sample_age_seconds",
	"broker_connected",
	"worker_inflight", "worker_capacity",
}

func init() {
	_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(workerOutboundGauge, workerOutboundEnabled.Load())
		return nil
	}, workerOutboundGauge)
	_, _ = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		s := sample.Load()
		if s == nil {
			return nil
		}
		for _, name := range operationalMeasures {
			value := s.Values[name]
			switch name {
			case "sample_age_seconds":
				value = int64(time.Since(s.At).Seconds())
			case "worker_inflight":
				value = workerInFlight.Load()
			case "worker_capacity":
				value = 4
			}
			o.ObserveInt64(operationalGauge, value, metric.WithAttributes(attribute.String("measure", name)))
		}
		return nil
	}, operationalGauge)
}

func WorkerEnter() { workerInFlight.Add(1) }
func WorkerLeave() { workerInFlight.Add(-1) }
func SetWorkerOutboundEnabled(enabled bool) {
	if enabled {
		workerOutboundEnabled.Store(1)
	} else {
		workerOutboundEnabled.Store(0)
	}
}

func CRMElapsed(ctx context.Context, d time.Duration) {
	crmDuration.Record(ctx, d.Seconds())
}

func ActionOutcome(ctx context.Context, kind, status, class string) {
	if kind != "crm_projection" && kind != "slack_invoice_paid" {
		kind = "other"
	}
	switch status {
	case "succeeded", "retry_wait", "dead", "unknown", "blocked_auth", "blocked_config", "needs_attention":
	default:
		status = "other"
	}
	category := "none"
	if class != "" {
		switch {
		case status == "blocked_auth" || class == "credential_revoked":
			category = "auth"
		case class == "retry_exhausted":
			category = "retry_exhausted"
		case strings.Contains(class, "rate_limited") || strings.Contains(class, "quota"):
			category = "quota"
		case strings.Contains(class, "identity") || strings.Contains(class, "mapping"):
			category = "identity"
		case strings.Contains(class, "unavailable") || strings.Contains(class, "timeout"):
			category = "availability"
		default:
			category = "other"
		}
	}
	actionOutcomes.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind),
		attribute.String("status", status), attribute.String("category", category)))
}

func UpdateOperational(values map[string]int64) {
	copyValues := make(map[string]int64, len(operationalMeasures))
	for _, name := range operationalMeasures {
		copyValues[name] = values[name]
	}
	sample.Store(&OperationalSnapshot{At: time.Now(), Values: copyValues})
}

func ProviderRequest(ctx context.Context, provider string, status int, duration time.Duration) {
	if provider != "stripe" && provider != "hubspot" && provider != "slack" {
		provider = "other"
	}
	class := "transport"
	if status >= 200 && status < 300 {
		class = "2xx"
	} else if status >= 400 && status < 500 {
		class = "4xx"
	} else if status >= 500 {
		class = "5xx"
	}
	attrs := metric.WithAttributes(attribute.String("provider", provider), attribute.String("status_class", class))
	providerLatency.Record(ctx, duration.Seconds(), attrs)
	providerRequests.Add(ctx, 1, attrs)
}

func Webhook(ctx context.Context, status int, duration time.Duration, duplicate bool) {
	class := "accepted"
	if status >= 500 {
		class = "unavailable"
	} else if status >= 400 {
		class = "rejected"
	}
	attrs := metric.WithAttributes(attribute.String("result", class))
	webhookLatency.Record(ctx, duration.Seconds(), attrs)
	webhookRequests.Add(ctx, 1, attrs)
	if duplicate {
		inboxDuplicates.Add(ctx, 1)
	}
}

// Labels are deliberately bounded. Account, tenant, token and request IDs
// belong in protected database history, never in metric dimensions.
func Event(ctx context.Context, kind, provider string) {
	switch kind {
	case "oauth_callback_failure", "refresh_success", "refresh_failure", "blocked_auth",
		"http_429", "redis_outage", "breaker_open", "breaker_half_open",
		"admission_rejected", "disconnect", "reconnect":
	default:
		return
	}
	if provider != "stripe" && provider != "hubspot" && provider != "slack" {
		provider = "other"
	}
	events.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind), attribute.String("provider", provider)))
}

func Wait(ctx context.Context, provider string, delay time.Duration) {
	if delay <= 0 {
		return
	}
	if provider != "stripe" && provider != "hubspot" && provider != "slack" {
		provider = "other"
	}
	waits.Record(ctx, delay.Seconds(), metric.WithAttributes(attribute.String("provider", provider)))
}

func QueueAge(ctx context.Context, category string, seconds int64) {
	if category != "due_action" && category != "unpublished_outbox" && category != "unknown_action" {
		return
	}
	if seconds < 0 {
		seconds = 0
	}
	queueAge.Record(ctx, float64(seconds), metric.WithAttributes(attribute.String("category", category)))
}
