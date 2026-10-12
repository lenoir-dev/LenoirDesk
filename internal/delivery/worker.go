package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/reconciliation"
	"github.com/lenoir-dev/LenoirDesk/internal/retention"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
	"github.com/lenoir-dev/LenoirDesk/internal/telemetry"
)

const subject = "relaydesk.plan.v1"
const actionSubject = "relaydesk.action.v1"

// A relay tick drains a bounded burst instead of imposing a 16-pointer-per-
// 500ms ceiling on both durable queues. The bound limits broker and database
// work when an ingress burst arrives or another worker has been stopped.
const relayBurstBatches = 8

type Envelope struct {
	Version       int    `json:"version"`
	JobID         string `json:"job_id"`
	TenantID      string `json:"tenant_id"`
	Generation    int64  `json:"dispatch_generation"`
	CorrelationID string `json:"correlation_id"`
	Traceparent   string `json:"traceparent,omitempty"`
}
type ActionEnvelope struct {
	Version    int    `json:"version"`
	TenantID   string `json:"tenant_id"`
	ActionID   string `json:"action_id"`
	Generation int64  `json:"dispatch_generation"`
}
type Worker struct {
	Store     *store.Store
	JS        jetstream.JetStream
	Broker    *nats.Conn
	Log       *slog.Logger
	Business  *execution.Executor
	Reconcile *reconciliation.Reconciler
	Retention *retention.Policy
}

func New(ctx context.Context, s *store.Store, natsURL string, log *slog.Logger) (*Worker, *nats.Conn, error) {
	nc, err := nats.Connect(natsURL, nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second), nats.Timeout(3*time.Second))
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	return &Worker{Store: s, JS: js, Broker: nc, Log: log}, nc, nil
}
func (w *Worker) ensure(ctx context.Context) (jetstream.Consumer, error) {
	_, err := w.JS.CreateOrUpdateStream(ctx, jetstream.StreamConfig{Name: "RELAYDESK_WORK", Subjects: []string{subject}, Storage: jetstream.FileStorage, Retention: jetstream.WorkQueuePolicy, Discard: jetstream.DiscardNew, MaxMsgSize: 2048, MaxBytes: 128 << 20, Duplicates: 10 * time.Minute})
	if err != nil {
		return nil, err
	}
	return w.JS.CreateOrUpdateConsumer(ctx, "RELAYDESK_WORK", jetstream.ConsumerConfig{Durable: "PLANNER_V1", FilterSubject: subject, AckPolicy: jetstream.AckExplicitPolicy, AckWait: 30 * time.Second, MaxAckPending: 64})
}
func (w *Worker) Run(ctx context.Context) {
	// Distinct loops let an unavailable broker preserve the PostgreSQL outbox.
	var wg sync.WaitGroup
	wg.Add(2)
	if w.Business != nil {
		wg.Add(2)
		go func() { defer wg.Done(); w.consumeBusiness(ctx) }()
		go func() { defer wg.Done(); w.repairBusiness(ctx) }()
	}
	if w.Reconcile != nil {
		wg.Add(1)
		go func() { defer wg.Done(); w.reconcile(ctx) }()
	}
	if w.Retention != nil {
		wg.Add(1)
		go func() { defer wg.Done(); w.retain(ctx) }()
	}
	go func() { defer wg.Done(); w.relay(ctx) }()
	go func() { defer wg.Done(); w.recover(ctx) }()
	w.consume(ctx)
	wg.Wait()
}

func (w *Worker) retain(ctx context.Context) {
	// Delay the first batch so startup recovery and migration health can settle.
	initial := time.NewTimer(time.Minute)
	defer initial.Stop()
	select {
	case <-ctx.Done():
		return
	case <-initial.C:
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		result, err := retention.Batch(ctx, w.Store, *w.Retention)
		if err != nil {
			w.Log.Warn("retention batch failed", "error", err)
		} else {
			w.Log.Info("retention batch", "raw_redacted", result.RawRedacted,
				"provider_calls_removed", result.ProviderCallsRemoved, "attempts_removed", result.AttemptsRemoved,
				"runs_removed", result.RunsRemoved, "audit_removed", result.AuditRemoved)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) reconcile(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	page := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		page++
		var err error
		if page%3 == 0 {
			_, err = w.Reconcile.StepHistorical(ctx)
		} else {
			_, err = w.Reconcile.Step(ctx)
		}
		if err != nil {
			w.Log.Warn("reconciliation page failed", "error", err)
		}
	}
}
func (w *Worker) relay(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for batch := 0; batch < relayBurstBatches && ctx.Err() == nil; batch++ {
			progress, failed := false, false
			if w.Business != nil {
				queued, err := w.Store.QueueBusiness(ctx)
				if err != nil {
					w.Log.Warn("business scheduler failed", "error", err)
					failed = true
				}
				count, relayFailed := w.relayBusiness(ctx)
				progress, failed = queued > 0 || count > 0, failed || relayFailed
			}
			rows, err := w.Store.ClaimOutbox(ctx, 16)
			if err != nil {
				w.Log.Warn("outbox claim failed", "error", err)
				break
			}
			progress = progress || len(rows) > 0
			for _, r := range rows {
				env := Envelope{Version: 1, JobID: r.JobID, TenantID: r.TenantID, Generation: r.Generation, CorrelationID: r.CorrelationID, Traceparent: r.Traceparent}
				raw, _ := json.Marshal(env)
				msg := nats.NewMsg(subject)
				msg.Data = raw
				msg.Header.Set("Nats-Msg-Id", r.ID)
				publishCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				_, err = w.JS.PublishMsg(publishCtx, msg)
				cancel()
				if err != nil {
					w.Log.Warn("outbox publish failed", "error", err)
					failed = true
					continue
				}
				if err = w.Store.MarkPublished(ctx, r); err != nil {
					w.Log.Warn("outbox mark failed", "error", err)
					failed = true
				}
			}
			if failed || !progress {
				break
			}
		}
	}
}
func (w *Worker) relayBusiness(ctx context.Context) (int, bool) {
	rows, err := w.Store.ClaimBusinessOutbox(ctx)
	if err != nil {
		w.Log.Warn("business outbox claim failed", "error", err)
		return 0, true
	}
	failed := false
	for _, r := range rows {
		raw, _ := json.Marshal(ActionEnvelope{Version: 1, TenantID: r.TenantID, ActionID: r.ActionID, Generation: r.Generation})
		msg := nats.NewMsg(actionSubject)
		msg.Data = raw
		msg.Header.Set("Nats-Msg-Id", r.ID)
		publishCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err = w.JS.PublishMsg(publishCtx, msg)
		cancel()
		if err != nil {
			w.Log.Warn("business outbox publish failed", "error", err)
			failed = true
			continue
		}
		if err = w.Store.MarkBusinessPublished(ctx, r); err != nil {
			w.Log.Warn("business outbox mark failed", "error", err)
			failed = true
		}
	}
	return len(rows), failed
}
func (w *Worker) repairBusiness(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		start := time.Now()
		repaired, err := w.Store.RepairBusiness(ctx)
		if err != nil {
			w.Log.Warn("business repair failed", "error", err)
			continue
		}
		h, err := w.Store.BusinessHealth(ctx)
		if err != nil {
			w.Log.Warn("business health unavailable", "error", err)
			continue
		}
		operational, err := w.Store.OperationalMetrics(ctx)
		if err != nil {
			w.Log.Warn("operational metrics unavailable", "error", err)
			continue
		}
		brokerConnected := int64(0)
		if w.Broker != nil && w.Broker.IsConnected() {
			brokerConnected = 1
		}
		telemetry.UpdateOperational(map[string]int64{
			"broker_connected": brokerConnected,
			"due_actions":      h.DueActions, "oldest_due_seconds": h.OldestDueSeconds,
			"unpublished_outbox": h.UnpublishedOutbox, "oldest_outbox_seconds": h.OldestUnpublishedSeconds,
			"active_leases": h.ActiveLeases, "expired_leases": h.ExpiredLeases,
			"stalled_actions": h.StalledActions, "dead_actions": h.DeadActions,
			"unknown_actions": h.UnknownActions, "oldest_unknown_seconds": h.OldestUnknownSeconds,
			"retries_24h": h.Retries24h, "oldest_dead_seconds": operational.OldestDeadSeconds,
			"oldest_pending_job_seconds":   operational.OldestPendingJobSeconds,
			"coverage_gaps":                operational.CoverageGaps,
			"oldest_reconcile_lag_seconds": operational.OldestReconcileLagSeconds,
			"unhealthy_connections":        operational.UnhealthyConnections,
			"storage_bytes":                operational.StorageBytes,
			"succeeded_actions_24h":        operational.SucceededActions24h,
			"failed_actions_24h":           operational.FailedActions24h,
		})
		telemetry.QueueAge(ctx, "due_action", h.OldestDueSeconds)
		telemetry.QueueAge(ctx, "unpublished_outbox", h.OldestUnpublishedSeconds)
		telemetry.QueueAge(ctx, "unknown_action", h.OldestUnknownSeconds)
		w.Log.Info("business recovery health", "repaired", repaired, "scan_ms", time.Since(start).Milliseconds(),
			"due_actions", h.DueActions, "oldest_due_seconds", h.OldestDueSeconds,
			"unpublished_outbox", h.UnpublishedOutbox, "oldest_unpublished_seconds", h.OldestUnpublishedSeconds,
			"active_leases", h.ActiveLeases, "expired_leases", h.ExpiredLeases,
			"stalled_actions", h.StalledActions, "dead_actions", h.DeadActions,
			"unknown_actions", h.UnknownActions, "oldest_unknown_seconds", h.OldestUnknownSeconds,
			"retries_24h", h.Retries24h)
	}
}
func (w *Worker) ensureBusiness(ctx context.Context) (jetstream.Consumer, error) {
	_, err := w.JS.CreateOrUpdateStream(ctx, jetstream.StreamConfig{Name: "RELAYDESK_ACTIONS",
		Subjects: []string{actionSubject}, Storage: jetstream.FileStorage, Retention: jetstream.WorkQueuePolicy,
		Discard: jetstream.DiscardNew, MaxMsgSize: 2048, MaxBytes: 128 << 20, Duplicates: 10 * time.Minute})
	if err != nil {
		return nil, err
	}
	return w.JS.CreateOrUpdateConsumer(ctx, "RELAYDESK_ACTIONS", jetstream.ConsumerConfig{
		Durable: "EXECUTOR_V1", FilterSubject: actionSubject, AckPolicy: jetstream.AckExplicitPolicy,
		AckWait: 45 * time.Second, MaxAckPending: 32})
}
func (w *Worker) consumeBusiness(ctx context.Context) {
	for ctx.Err() == nil {
		setupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		consumer, err := w.ensureBusiness(setupCtx)
		cancel()
		if err != nil {
			w.Log.Warn("action JetStream setup unavailable", "error", err)
			wait(ctx, time.Second)
			continue
		}
		for ctx.Err() == nil {
			batch, err := consumer.Fetch(4, jetstream.FetchMaxWait(time.Second))
			if err != nil {
				w.Log.Warn("action fetch failed", "error", err)
				break
			}
			var wg sync.WaitGroup
			for msg := range batch.Messages() {
				wg.Add(1)
				go func(m jetstream.Msg) { defer wg.Done(); w.handleBusiness(ctx, m) }(msg)
			}
			wg.Wait()
			if err = batch.Error(); err != nil {
				w.Log.Warn("action batch failed", "error", err)
				break
			}
		}
	}
}
func (w *Worker) handleBusiness(ctx context.Context, msg jetstream.Msg) {
	telemetry.WorkerEnter()
	defer telemetry.WorkerLeave()
	var env ActionEnvelope
	if err := json.Unmarshal(msg.Data(), &env); err != nil || env.Version != 1 || env.TenantID == "" || env.ActionID == "" || env.Generation < 1 {
		w.Log.Error("invalid action envelope")
		_ = msg.Term()
		return
	}
	ack, err := w.Business.ExecutePointer(ctx, env.TenantID, env.ActionID, env.Generation)
	if err != nil {
		w.Log.Warn("action pointer not committed", "action_id", env.ActionID, "error", err)
		return
	}
	if !ack {
		// A still-queued action can be temporarily ineligible because its tenant
		// already has active claims or the same Customer is being projected.
		// Release the consumer slot promptly; PostgreSQL remains authoritative.
		if err = msg.NakWithDelay(2 * time.Second); err != nil {
			w.Log.Warn("action pointer delayed NAK failed", "action_id", env.ActionID, "error", err)
		}
		return
	}
	ackCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err = msg.DoubleAck(ackCtx); err != nil {
		w.Log.Warn("action ACK failed", "action_id", env.ActionID, "error", err)
	}
}
func (w *Worker) recover(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if _, err := w.Store.Recover(ctx); err != nil {
			w.Log.Warn("dispatch recovery failed", "error", err)
		}
	}
}
func (w *Worker) consume(ctx context.Context) {
	for ctx.Err() == nil {
		setupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		consumer, err := w.ensure(setupCtx)
		cancel()
		if err != nil {
			w.Log.Warn("JetStream setup unavailable", "error", err)
			wait(ctx, time.Second)
			continue
		}
		for ctx.Err() == nil {
			batch, err := consumer.Fetch(16, jetstream.FetchMaxWait(time.Second))
			if err != nil {
				w.Log.Warn("planner fetch failed", "error", err)
				break
			}
			for msg := range batch.Messages() {
				w.handle(ctx, msg)
			}
			if err = batch.Error(); err != nil {
				w.Log.Warn("planner batch failed", "error", err)
				break
			}
		}
	}
}
func (w *Worker) handle(ctx context.Context, msg jetstream.Msg) {
	var env Envelope
	if err := json.Unmarshal(msg.Data(), &env); err != nil || env.Version != 1 || env.JobID == "" || env.TenantID == "" || env.Generation < 1 {
		// Invalid transport data must remain visible for investigation. Do not ACK it as work completed.
		w.Log.Error("invalid planning envelope")
		_ = msg.Term()
		return
	}
	if env.Traceparent != "" {
		ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": env.Traceparent})
	}
	ctx, span := otel.Tracer("relaydesk/planner").Start(ctx, "planning.job")
	defer span.End()
	err := w.Store.Plan(ctx, env.TenantID, env.JobID, env.Generation)
	if errors.Is(err, store.ErrNotFound) {
		w.Log.Error("planning job missing", "job_id", env.JobID)
		_ = msg.Term()
		return
	}
	if err != nil {
		w.Log.Warn("planning failed", "job_id", env.JobID, "error", err)
		return
	}
	ackCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err = msg.DoubleAck(ackCtx); err != nil {
		w.Log.Warn("planner ACK failed", "job_id", env.JobID, "error", err)
	}
}
func wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
