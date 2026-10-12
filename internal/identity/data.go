package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

var ErrForbidden = errors.New("membership or permission missing")
var ErrInvalidCursor = errors.New("invalid cursor")

type Workspace struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

type Connection struct {
	ID                string     `json:"id"`
	Provider          string     `json:"provider"`
	ProviderAccountID string     `json:"provider_account_id"`
	Mode              string     `json:"mode"`
	Environment       string     `json:"execution_environment"`
	Status            string     `json:"status"`
	HealthStatus      string     `json:"health_status"`
	Generation        int64      `json:"generation"`
	CreatedAt         time.Time  `json:"created_at"`
	VerifiedAt        *time.Time `json:"verified_at,omitempty"`
	LastCheckedAt     *time.Time `json:"last_checked_at,omitempty"`
	AuthState         string     `json:"auth_state,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
}

type Run struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	EventID    string    `json:"event_id"`
	EventType  string    `json:"event_type"`
	CustomerID string    `json:"customer_id,omitempty"`
	InvoiceID  string    `json:"invoice_id,omitempty"`
	Attempts   int       `json:"attempts"`
	Step       string    `json:"step,omitempty"`
}

type Member struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

type pageCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

func parseCursor(encoded string) (*pageCursor, error) {
	if encoded == "" {
		return nil, nil
	}
	if len(encoded) > 512 {
		return nil, ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var c pageCursor
	if err = json.Unmarshal(raw, &c); err != nil || c.CreatedAt.IsZero() {
		return nil, ErrInvalidCursor
	}
	if _, err = uuid.Parse(c.ID); err != nil {
		return nil, ErrInvalidCursor
	}
	return &c, nil
}

func encodeCursor(at time.Time, id string) string {
	raw, _ := json.Marshal(pageCursor{CreatedAt: at, ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func validateLimit(limit int) int {
	if limit < 1 {
		return 20
	}
	if limit > 100 {
		return 100
	}
	return limit
}

func (s *Service) CreateWorkspace(ctx context.Context, p Principal, name, correlationID string) (string, error) {
	var id string
	err := s.appDB.Pool.QueryRow(ctx, `SELECT workspace_create($1,$2,$3)`, p.TokenHash, name, correlationID).Scan(&id)
	return id, err
}

func (s *Service) ListWorkspaces(ctx context.Context, p Principal, limit int, cursor string) ([]Workspace, string, error) {
	c, err := parseCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var beforeAt *time.Time
	var beforeID *string
	if c != nil {
		beforeAt, beforeID = &c.CreatedAt, &c.ID
	}
	limit = validateLimit(limit)
	rows, err := s.appDB.Pool.Query(ctx, `SELECT id,name,role,created_at FROM my_workspaces($1,$2,$3,$4)`, p.TokenHash, limit+1, beforeAt, beforeID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]Workspace, 0, limit+1)
	for rows.Next() {
		var item Workspace
		if err = rows.Scan(&item.ID, &item.Name, &item.Role, &item.CreatedAt); err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		return items, encodeCursor(last.CreatedAt, last.ID), nil
	}
	return items, "", nil
}

// AuthorizedTenantTx holds the membership row lock until the resource query is
// complete. A concurrent revocation therefore either wins first or waits.
func (s *Service) AuthorizedTenantTx(ctx context.Context, p Principal, tenantID, permission string) (pgx.Tx, string, error) {
	if _, err := uuid.Parse(tenantID); err != nil {
		return nil, "", ErrForbidden
	}
	tx, err := store.BeginAuthorizedTenant(ctx, s.appDB.Pool, tenantID, p.TokenHash)
	if err != nil {
		return nil, "", err
	}
	var role *string
	err = tx.QueryRow(ctx, `SELECT tenant_authorize($1,$2,$3)`, p.TokenHash, tenantID, permission).Scan(&role)
	if err != nil || role == nil {
		_ = tx.Rollback(ctx)
		if err != nil {
			return nil, "", err
		}
		return nil, "", ErrForbidden
	}
	return tx, *role, nil
}

func (s *Service) GetWorkspace(ctx context.Context, p Principal, tenantID string) (Workspace, error) {
	tx, role, err := s.AuthorizedTenantTx(ctx, p, tenantID, "read")
	if err != nil {
		return Workspace{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var w Workspace
	err = tx.QueryRow(ctx, `SELECT id,name,created_at FROM tenants WHERE id=$1`, tenantID).Scan(&w.ID, &w.Name, &w.CreatedAt)
	w.Role = role
	if err != nil {
		return Workspace{}, err
	}
	return w, tx.Commit(ctx)
}

func (s *Service) ListConnections(ctx context.Context, p Principal, tenantID string, limit int, cursor string) ([]Connection, string, error) {
	c, err := parseCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenantID, "read")
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var beforeAt *time.Time
	var beforeID *string
	if c != nil {
		beforeAt, beforeID = &c.CreatedAt, &c.ID
	}
	limit = validateLimit(limit)
	rows, err := tx.Query(ctx, `SELECT c.id,c.provider,c.provider_account_id,c.mode,c.status,c.health_status,c.generation,c.created_at,
 c.verified_at,c.last_checked_at,coalesce(s.auth_state,''),s.expires_at,c.execution_environment
 FROM connections c LEFT JOIN provider_token_state s ON s.tenant_id=c.tenant_id AND s.connection_id=c.id
 WHERE c.tenant_id=$1 AND ($2::timestamptz IS NULL OR (c.created_at,c.id)<($2,$3::uuid))
 ORDER BY c.created_at DESC,c.id DESC LIMIT $4`, tenantID, beforeAt, beforeID, limit+1)
	if err != nil {
		return nil, "", err
	}
	items := make([]Connection, 0, limit+1)
	for rows.Next() {
		var item Connection
		if err = rows.Scan(&item.ID, &item.Provider, &item.ProviderAccountID, &item.Mode, &item.Status,
			&item.HealthStatus, &item.Generation, &item.CreatedAt, &item.VerifiedAt, &item.LastCheckedAt,
			&item.AuthState, &item.ExpiresAt, &item.Environment); err != nil {
			rows.Close()
			return nil, "", err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		return items, encodeCursor(last.CreatedAt, last.ID), nil
	}
	return items, "", nil
}

func (s *Service) GetConnection(ctx context.Context, p Principal, tenantID, connectionID string) (Connection, error) {
	if _, err := uuid.Parse(connectionID); err != nil {
		return Connection{}, store.ErrNotFound
	}
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenantID, "read")
	if err != nil {
		return Connection{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var item Connection
	err = tx.QueryRow(ctx, `SELECT c.id,c.provider,c.provider_account_id,c.mode,c.status,c.health_status,c.generation,c.created_at,
 c.verified_at,c.last_checked_at,coalesce(s.auth_state,''),s.expires_at,c.execution_environment
 FROM connections c LEFT JOIN provider_token_state s ON s.tenant_id=c.tenant_id AND s.connection_id=c.id
 WHERE c.tenant_id=$1 AND c.id=$2`, tenantID, connectionID).
		Scan(&item.ID, &item.Provider, &item.ProviderAccountID, &item.Mode, &item.Status,
			&item.HealthStatus, &item.Generation, &item.CreatedAt, &item.VerifiedAt, &item.LastCheckedAt,
			&item.AuthState, &item.ExpiresAt, &item.Environment)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, store.ErrNotFound
	}
	if err != nil {
		return Connection{}, err
	}
	return item, tx.Commit(ctx)
}

func (s *Service) ListRuns(ctx context.Context, p Principal, tenantID string, limit int, cursor string) ([]Run, string, error) {
	return s.ListRunsFiltered(ctx, p, tenantID, limit, cursor, "")
}

func (s *Service) ListRunsFiltered(ctx context.Context, p Principal, tenantID string, limit int, cursor, status string) ([]Run, string, error) {
	var statuses []string
	switch status {
	case "":
	case "in_progress":
		statuses = []string{"pending", "running"}
	case "blocked":
		statuses = []string{"blocked_auth", "blocked_config"}
	case "pending", "running", "succeeded", "retry_wait", "blocked_auth", "blocked_config",
		"needs_attention", "unknown", "dead", "paused", "skipped", "canceled", "superseded":
		statuses = []string{status}
	default:
		return nil, "", ErrInvalidCursor
	}
	c, err := parseCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenantID, "read")
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var beforeAt *time.Time
	var beforeID *string
	if c != nil {
		beforeAt, beforeID = &c.CreatedAt, &c.ID
	}
	limit = validateLimit(limit)
	rows, err := tx.Query(ctx, `SELECT r.id,r.status,r.created_at,e.event_id,e.event_type,
 coalesce(effect.customer_id,''),coalesce(effect.invoice_id,''),coalesce(effect.attempts,0),coalesce(step.kind,'')
 FROM runs r JOIN planning_jobs j ON j.tenant_id=r.tenant_id AND j.id=r.planning_job_id
 JOIN webhook_events e ON e.tenant_id=j.tenant_id AND e.id=j.event_row_id
 LEFT JOIN LATERAL (
  SELECT max(a.customer_id) AS customer_id,max(nullif(a.invoice_id,'')) AS invoice_id,
   sum(a.attempts)::integer AS attempts
  FROM business_run_actions ra JOIN business_actions a ON a.tenant_id=ra.tenant_id AND a.id=ra.action_id
  WHERE ra.tenant_id=r.tenant_id AND ra.run_id=r.id
 ) effect ON true
 LEFT JOIN LATERAL (
  SELECT a.kind FROM business_run_actions ra
  JOIN business_actions a ON a.tenant_id=ra.tenant_id AND a.id=ra.action_id
  WHERE ra.tenant_id=r.tenant_id AND ra.run_id=r.id
  ORDER BY CASE WHEN a.status IN ('succeeded','skipped','superseded','canceled') THEN 1 ELSE 0 END,
   CASE WHEN a.kind='crm_projection' THEN 0 ELSE 1 END LIMIT 1
 ) step ON true
 WHERE r.tenant_id=$1 AND ($2::timestamptz IS NULL OR (r.created_at,r.id)<($2,$3::uuid))
 AND ($5::text[] IS NULL OR r.status=ANY($5::text[]))
 ORDER BY r.created_at DESC,r.id DESC LIMIT $4`, tenantID, beforeAt, beforeID, limit+1, statuses)
	if err != nil {
		return nil, "", err
	}
	items := make([]Run, 0, limit+1)
	for rows.Next() {
		var item Run
		if err = rows.Scan(&item.ID, &item.Status, &item.CreatedAt, &item.EventID, &item.EventType,
			&item.CustomerID, &item.InvoiceID, &item.Attempts, &item.Step); err != nil {
			rows.Close()
			return nil, "", err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		return items, encodeCursor(last.CreatedAt, last.ID), nil
	}
	return items, "", nil
}

func (s *Service) RunStatusCounts(ctx context.Context, p Principal, tenantID string) (map[string]int64, error) {
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenantID, "read")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT status,count(*) FROM runs WHERE tenant_id=$1 GROUP BY status`, tenantID)
	if err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	for rows.Next() {
		var status string
		var count int64
		if err = rows.Scan(&status, &count); err != nil {
			rows.Close()
			return nil, err
		}
		counts[status] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return counts, tx.Commit(ctx)
}

func (s *Service) ListMembers(ctx context.Context, p Principal, tenantID string, limit int, cursor string) ([]Member, string, error) {
	var after *string
	if cursor != "" {
		if len(cursor) > 64 {
			return nil, "", ErrInvalidCursor
		}
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, "", ErrInvalidCursor
		}
		value := string(raw)
		if _, err := uuid.Parse(value); err != nil {
			return nil, "", ErrInvalidCursor
		}
		after = &value
	}
	limit = validateLimit(limit)
	tx, _, err := s.AuthorizedTenantTx(ctx, p, tenantID, "members_read")
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT user_id,role,status FROM tenant_members_page($1,$2,$3,$4)`,
		p.TokenHash, tenantID, after, limit+1)
	if err != nil {
		return nil, "", err
	}
	items := make([]Member, 0, limit+1)
	for rows.Next() {
		var m Member
		if err = rows.Scan(&m.UserID, &m.Role, &m.Status); err != nil {
			rows.Close()
			return nil, "", err
		}
		items = append(items, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		return items, base64.RawURLEncoding.EncodeToString([]byte(items[len(items)-1].UserID)), nil
	}
	return items, "", nil
}

func (s *Service) ChangeMembership(ctx context.Context, p Principal, tenantID, targetUserID, action, role, correlationID string) error {
	if _, err := uuid.Parse(tenantID); err != nil {
		return ErrForbidden
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		return fmt.Errorf("invalid target user ID")
	}
	_, err := s.appDB.Pool.Exec(ctx, `SELECT membership_change($1,$2,$3,$4,$5,$6)`,
		p.TokenHash, tenantID, targetUserID, action, role, correlationID)
	return err
}
