package identity

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

type RecentEvent struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	ReceivedAt time.Time `json:"received_at"`
}

type RunTrendDay struct {
	Date  string `json:"date"` // UTC calendar date, YYYY-MM-DD
	Count int64  `json:"count"`
}

type Overview struct {
	RunCounts       map[string]int64 `json:"run_counts"`
	Backlog         int64            `json:"backlog"`
	LastSuccessful  *time.Time       `json:"last_successful,omitempty"`
	RecentEvents    []RecentEvent    `json:"recent_events"`
	ActiveRevisions int64            `json:"active_revisions"`
	RunTrend        []RunTrendDay    `json:"run_trend"`
}

func (s *Service) WorkspaceOverview(ctx context.Context, p Principal, tenant string) (Overview, error) {
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "read")
	if err != nil {
		return Overview{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out := Overview{RunCounts: map[string]int64{}}
	rows, err := tx.Query(ctx, `SELECT status,count(*) FROM runs WHERE tenant_id=$1 GROUP BY status`, tenant)
	if err != nil {
		return Overview{}, err
	}
	for rows.Next() {
		var status string
		var count int64
		if err = rows.Scan(&status, &count); err != nil {
			rows.Close()
			return Overview{}, err
		}
		out.RunCounts[status] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Overview{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM business_actions WHERE tenant_id=$1 AND status IN ('pending','queued','running','retry_wait','paused')`, tenant).Scan(&out.Backlog); err != nil {
		return Overview{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT max(finished_at) FROM runs WHERE tenant_id=$1 AND status='succeeded'`, tenant).Scan(&out.LastSuccessful); err != nil {
		return Overview{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM automation_revisions WHERE tenant_id=$1 AND template_key=$2 AND status='active'`, tenant, business.Template).Scan(&out.ActiveRevisions); err != nil {
		return Overview{}, err
	}
	rows, err = tx.Query(ctx, `SELECT event_id,event_type,received_at FROM webhook_events WHERE tenant_id=$1 ORDER BY received_at DESC,id DESC LIMIT 8`, tenant)
	if err != nil {
		return Overview{}, err
	}
	for rows.Next() {
		var item RecentEvent
		if err = rows.Scan(&item.ID, &item.Type, &item.ReceivedAt); err != nil {
			rows.Close()
			return Overview{}, err
		}
		out.RecentEvents = append(out.RecentEvents, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Overview{}, err
	}
	// The existing (tenant_id, created_at) history index bounds this fixed
	// fourteen-day window. Zero days are filled in Go rather than inferred by
	// the browser, so the graph cannot imply events where none were persisted.
	end := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	start := end.AddDate(0, 0, -14)
	trendRows, err := tx.Query(ctx, `SELECT (created_at AT TIME ZONE 'UTC')::date, count(*)
 FROM runs WHERE tenant_id=$1 AND created_at >= $2 AND created_at < $3
 GROUP BY 1 ORDER BY 1`, tenant, start, end)
	if err != nil {
		return Overview{}, err
	}
	counts := make(map[string]int64, 14)
	for trendRows.Next() {
		var day time.Time
		var count int64
		if err = trendRows.Scan(&day, &count); err != nil {
			trendRows.Close()
			return Overview{}, err
		}
		counts[day.Format("2006-01-02")] = count
	}
	err = trendRows.Err()
	trendRows.Close()
	if err != nil {
		return Overview{}, err
	}
	for i := 0; i < 14; i++ {
		date := start.AddDate(0, 0, i).Format("2006-01-02")
		out.RunTrend = append(out.RunTrend, RunTrendDay{Date: date, Count: counts[date]})
	}
	return out, tx.Commit(ctx)
}

type BillingRevision struct {
	ID          string          `json:"id"`
	StripeID    string          `json:"stripe_connection_id"`
	Revision    int             `json:"revision"`
	Status      string          `json:"status"`
	ActivatedAt time.Time       `json:"activated_at"`
	Config      business.Config `json:"config"`
	Paused      bool            `json:"paused"`
}

func (s *Service) ListBillingRevisions(ctx context.Context, p Principal, tenant string, limit int) ([]BillingRevision, error) {
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "read")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if limit < 1 || limit > 50 {
		limit = 20
	}
	rows, err := tx.Query(ctx, `SELECT r.id,r.connection_id,r.revision,r.status,r.activated_at,r.config,CASE WHEN r.status='active' THEN coalesce(g.paused,false) ELSE false END
 FROM automation_revisions r LEFT JOIN automation_gates g ON g.tenant_id=r.tenant_id AND g.stripe_connection_id=r.connection_id
 WHERE r.tenant_id=$1 AND r.template_key=$2 ORDER BY r.activated_at DESC,r.id DESC LIMIT $3`, tenant, business.Template, limit)
	if err != nil {
		return nil, err
	}
	var items []BillingRevision
	for rows.Next() {
		var item BillingRevision
		var raw []byte
		if err = rows.Scan(&item.ID, &item.StripeID, &item.Revision, &item.Status, &item.ActivatedAt, &raw, &item.Paused); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal(raw, &item.Config); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return items, tx.Commit(ctx)
}

type ContactLink struct {
	CustomerKey         string    `json:"customer_key"`
	HubSpotConnectionID string    `json:"hubspot_connection_id"`
	ContactID           string    `json:"contact_id"`
	Source              string    `json:"source"`
	CreatedAt           time.Time `json:"created_at"`
}

func (s *Service) ContactLink(ctx context.Context, p Principal, tenant, hubspot, key string) (ContactLink, bool, error) {
	if _, err := uuid.Parse(hubspot); err != nil || !customerKeyPattern.MatchString(key) {
		return ContactLink{}, false, store.ErrNotFound
	}
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "read")
	if err != nil {
		return ContactLink{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var item ContactLink
	err = tx.QueryRow(ctx, `SELECT customer_key,hubspot_connection_id,contact_id,source,created_at FROM contact_links
 WHERE tenant_id=$1 AND hubspot_connection_id=$2 AND customer_key=$3`, tenant, hubspot, key).
		Scan(&item.CustomerKey, &item.HubSpotConnectionID, &item.ContactID, &item.Source, &item.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ContactLink{}, false, nil
		}
		return ContactLink{}, false, err
	}
	return item, true, tx.Commit(ctx)
}

type AuditEntry struct {
	Operation  string    `json:"operation"`
	TargetType string    `json:"target_type"`
	TargetID   string    `json:"target_id"`
	Result     string    `json:"result"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Service) RecentAudit(ctx context.Context, p Principal, tenant string, limit int) ([]AuditEntry, error) {
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenant, "read")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if limit < 1 || limit > 50 {
		limit = 20
	}
	rows, err := tx.Query(ctx, `SELECT operation,target_type,target_id,result,created_at FROM audit_log
 WHERE tenant_id=$1 ORDER BY created_at DESC,id DESC LIMIT $2`, tenant, limit)
	if err != nil {
		return nil, err
	}
	var items []AuditEntry
	for rows.Next() {
		var item AuditEntry
		if err = rows.Scan(&item.Operation, &item.TargetType, &item.TargetID, &item.Result, &item.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return items, tx.Commit(ctx)
}
