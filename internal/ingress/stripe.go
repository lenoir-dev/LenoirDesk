package ingress

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	"github.com/lenoir-dev/LenoirDesk/internal/crypto"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
	"github.com/lenoir-dev/LenoirDesk/internal/telemetry"
)

const MaxPayload = 256 * 1024

var supported = map[string]bool{"invoice.paid": true, "invoice.payment_failed": true, "customer.subscription.created": true, "customer.subscription.updated": true, "customer.subscription.deleted": true}

type Handler struct {
	Store *store.Store
	Box   *crypto.Box
	Log   *slog.Logger
}
type stripeEnvelope struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	Type       string `json:"type"`
	APIVersion string `json:"api_version"`
	Livemode   *bool  `json:"livemode"`
	Account    string `json:"account"`
	Created    int64  `json:"created"`
	Data       struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

type webhookWriter struct {
	http.ResponseWriter
	status int
}

func (w *webhookWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *webhookWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func Correlation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			http.Error(w, "request unavailable", http.StatusInternalServerError)
			return
		}
		id := hex.EncodeToString(buf)
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), correlationKey{}, id)))
	})
}

type correlationKey struct{}

func CorrelationID(ctx context.Context) string {
	v, _ := ctx.Value(correlationKey{}).(string)
	return v
}
func correlation(ctx context.Context) string { return CorrelationID(ctx) }
func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
}
func (h Handler) Stripe(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	tracked := &webhookWriter{ResponseWriter: w}
	w = tracked
	duplicate := false
	ctx, span := otel.Tracer("relaydesk/ingress").Start(r.Context(), "stripe.webhook.accept")
	defer span.End()
	defer func() { telemetry.Webhook(ctx, tracked.status, time.Since(started), duplicate) }()
	r = r.WithContext(ctx)
	ref := chi.URLParam(r, "connection_ref")
	if len(ref) != 64 {
		writeError(w, http.StatusNotFound, "connection_not_found")
		return
	}
	if _, err := hex.DecodeString(ref); err != nil {
		writeError(w, http.StatusNotFound, "connection_not_found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxPayload))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_body")
		}
		return
	}
	binding, err := h.Store.LookupStripe(r.Context(), ref)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "connection_not_found")
		return
	}
	if err != nil {
		h.Log.Error("connection lookup failed", "correlation_id", correlation(r.Context()), "error", err)
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
		return
	}
	secret, err := h.Box.Open(binding.Ciphertext, binding.Nonce, binding.TenantID, binding.ConnectionID, "stripe", binding.KeyVersion)
	if err != nil {
		h.Log.Error("connection secret could not be opened", "connection_id", binding.ConnectionID)
		writeError(w, http.StatusServiceUnavailable, "connection_unavailable")
		return
	}
	if err = webhook.ValidatePayloadWithTolerance(body, r.Header.Get("Stripe-Signature"), string(secret), 5*time.Minute); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_signature")
		return
	}
	var event stripeEnvelope
	if err = json.Unmarshal(body, &event); err != nil || event.Object != "event" || event.ID == "" || event.Type == "" || event.Livemode == nil {
		writeError(w, http.StatusBadRequest, "malformed_event")
		return
	}
	if (binding.Mode == "live") != *event.Livemode || (event.Account != "" && event.Account != binding.AccountID) {
		writeError(w, http.StatusBadRequest, "account_mismatch")
		return
	}
	class, reason := "supported", ""
	if !supported[event.Type] {
		class, reason = "ignored", "unsupported_type"
	}
	if event.APIVersion != stripe.APIVersion {
		class, reason = "ignored", "unsupported_api_version"
	}
	if class == "supported" {
		var object struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		}
		expected := "subscription"
		if event.Type == "invoice.paid" || event.Type == "invoice.payment_failed" {
			expected = "invoice"
		}
		if err = json.Unmarshal(event.Data.Object, &object); err != nil || object.ID == "" || object.Object != expected {
			writeError(w, http.StatusBadRequest, "malformed_event")
			return
		}
	}
	span.SetAttributes(attribute.String("stripe.event_type", event.Type), attribute.String("relaydesk.classification", class))
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	source := "webhook"
	// The durable effect tombstone is retained, but a newly observed invoice fact
	// outside the supported replay window must never cause a historical Slack send.
	if event.Type == "invoice.paid" && (event.Created <= 0 || time.Unix(event.Created, 0).Before(time.Now().Add(-90*24*time.Hour))) {
		source = "historical"
	}
	created, err := h.Store.Accept(ctx, store.Incoming{Binding: binding, EventID: event.ID, EventType: event.Type, APIVersion: event.APIVersion, CorrelationID: correlation(ctx), Traceparent: carrier.Get("traceparent"), Classification: class, Reason: reason, Raw: body, Source: source})
	if errors.Is(err, store.ErrCapacity) {
		telemetry.Event(ctx, "admission_rejected", "stripe")
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "tenant_capacity_exhausted")
		return
	}
	if errors.Is(err, store.ErrIgnoredCapacity) {
		telemetry.Event(ctx, "admission_rejected", "stripe")
		w.Header().Set("Retry-After", "3600")
		writeError(w, http.StatusServiceUnavailable, "ignored_event_capacity_exhausted")
		return
	}
	if err != nil {
		h.Log.Error("webhook acceptance failed", "correlation_id", correlation(r.Context()), "error", err)
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
		return
	}
	if created {
		w.WriteHeader(http.StatusAccepted)
	} else {
		duplicate = true
		w.WriteHeader(http.StatusOK)
	}
}
