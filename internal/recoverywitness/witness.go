package recoverywitness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrClosed is intentionally generic: no database URL, file content or
// provider identifier belongs in a worker log when recovery is required.
var ErrClosed = errors.New("external restore witness is missing or mismatched")

type State struct {
	Version     int             `json:"version"`
	Incarnation string          `json:"incarnation"`
	Sequence    int64           `json:"effect_sequence"`
	Active      map[string]bool `json:"active_effects,omitempty"`
}

type Witness struct {
	Pool   *pgxpool.Pool
	Path   string
	mu     sync.Mutex
	active map[string]bool
}

func (w *Witness) locked(fn func() error) error {
	if w == nil || w.Pool == nil || w.Path == "" {
		return ErrClosed
	}
	parent := filepath.Dir(w.Path)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return ErrClosed
	}
	lock, err := os.OpenFile(w.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return ErrClosed
	}
	defer func() { _ = lock.Close() }()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return ErrClosed
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

func (w *Witness) read() (State, error) {
	var state State
	info, err := os.Lstat(w.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 65536 {
		return state, ErrClosed
	}
	data, err := os.ReadFile(w.Path)
	if err != nil || json.Unmarshal(data, &state) != nil || state.Version != 1 ||
		state.Incarnation == "" || state.Sequence < 0 {
		return State{}, ErrClosed
	}
	return state, nil
}

func (w *Witness) sameActive(file map[string]bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(file) != len(w.active) {
		return false
	}
	for id := range file {
		if !w.active[id] {
			return false
		}
	}
	return true
}

func (w *Witness) write(state State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return ErrClosed
	}
	tmp, err := os.CreateTemp(filepath.Dir(w.Path), ".witness-*")
	if err != nil {
		return ErrClosed
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck
	defer func() { _ = tmp.Close() }()
	if err = tmp.Chmod(0o600); err != nil {
		return ErrClosed
	}
	if _, err = tmp.Write(data); err != nil {
		return ErrClosed
	}
	if err = tmp.Sync(); err != nil {
		return ErrClosed
	}
	if err = tmp.Close(); err != nil {
		return ErrClosed
	}
	if err = os.Rename(tmp.Name(), w.Path); err != nil {
		return ErrClosed
	}
	dir, err := os.Open(filepath.Dir(w.Path))
	if err != nil {
		return ErrClosed
	}
	defer func() { _ = dir.Close() }()
	if err = dir.Sync(); err != nil {
		return ErrClosed
	}
	return nil
}

func (w *Witness) Check(ctx context.Context) error {
	return w.locked(func() error {
		file, err := w.read()
		if err != nil {
			return ErrClosed
		}
		var db State
		var enabled bool
		err = w.Pool.QueryRow(ctx, `SELECT incarnation::text,effect_sequence,
 (SELECT outbound_enabled FROM restore_safety_gate WHERE singleton=true)
 FROM restore_witness_state WHERE singleton=true`).Scan(&db.Incarnation, &db.Sequence, &enabled)
		if err != nil || !enabled || file.Incarnation != db.Incarnation || file.Sequence != db.Sequence || !w.sameActive(file.Active) {
			return ErrClosed
		}
		return nil
	})
}

// Prepare persists an external high-water mark before any provider request.
// A crash between the database commit and file fsync closes the gate; it cannot
// cause an automatic resend. All workers in one deployment must share Path.
func (w *Witness) Prepare(ctx context.Context, tenantID, actionID string) error {
	key := tenantID + "/" + actionID
	if tenantID == "" || actionID == "" {
		return ErrClosed
	}
	return w.locked(func() error {
		file, err := w.read()
		if err != nil || !w.sameActive(file.Active) || file.Active[key] {
			return ErrClosed
		}
		tx, err := w.Pool.Begin(ctx)
		if err != nil {
			return ErrClosed
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		var db State
		err = tx.QueryRow(ctx, `SELECT incarnation::text,effect_sequence FROM restore_witness_state
 WHERE singleton=true FOR UPDATE`).Scan(&db.Incarnation, &db.Sequence)
		if err != nil || file.Incarnation != db.Incarnation || file.Sequence != db.Sequence {
			return ErrClosed
		}
		var enabled bool
		if err = tx.QueryRow(ctx, `SELECT outbound_enabled FROM restore_safety_gate WHERE singleton=true`).Scan(&enabled); err != nil || !enabled {
			return ErrClosed
		}
		db.Version = 1
		db.Sequence++
		db.Active = file.Active
		if db.Active == nil {
			db.Active = make(map[string]bool)
		}
		db.Active[key] = true
		if _, err = tx.Exec(ctx, `UPDATE restore_witness_state SET effect_sequence=$1 WHERE singleton=true`, db.Sequence); err != nil {
			return ErrClosed
		}
		if err = tx.Commit(ctx); err != nil {
			return ErrClosed
		}
		if err = w.write(db); err != nil {
			return err
		}
		w.mu.Lock()
		if w.active == nil {
			w.active = make(map[string]bool)
		}
		w.active[key] = true
		w.mu.Unlock()
		return nil
	})
}

// Complete removes an in-flight marker only after the action outcome is
// durable. A crash or uncommitted result leaves the marker for operator review.
func (w *Witness) Complete(ctx context.Context, tenantID, actionID string) error {
	key := tenantID + "/" + actionID
	return w.locked(func() error {
		file, err := w.read()
		if err != nil || !file.Active[key] || !w.sameActive(file.Active) {
			return ErrClosed
		}
		var status string
		tx, beginErr := w.Pool.Begin(ctx)
		if beginErr != nil {
			return ErrClosed
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, tenantID); err != nil {
			return ErrClosed
		}
		err = tx.QueryRow(ctx, `SELECT status FROM business_actions WHERE tenant_id=$1 AND id=$2`, tenantID, actionID).Scan(&status)
		if err != nil || status == "running" || status == "queued" || status == "pending" {
			w.mu.Lock()
			delete(w.active, key)
			w.mu.Unlock()
			return ErrClosed
		}
		if err = tx.Commit(ctx); err != nil {
			return ErrClosed
		}
		delete(file.Active, key)
		if err = w.write(file); err != nil {
			return err
		}
		w.mu.Lock()
		delete(w.active, key)
		w.mu.Unlock()
		return nil
	})
}

// Bind is a maintenance-only operation. A fresh enrollment needs an empty
// witness and a zero effect counter. Rebinding after restore requires the SQL
// recovery gate and reviewed Slack uncertainty before the gate can be cleared.
func (w *Witness) Bind(ctx context.Context, reason string, rebind bool) error {
	if len(reason) < 12 || len(reason) > 500 {
		return ErrClosed
	}
	return w.locked(func() error {
		_, statErr := os.Lstat(w.Path)
		if !rebind && !os.IsNotExist(statErr) {
			return ErrClosed
		}
		tx, err := w.Pool.Begin(ctx)
		if err != nil {
			return ErrClosed
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		var db State
		if err = tx.QueryRow(ctx, `SELECT incarnation::text,effect_sequence FROM restore_witness_state
 WHERE singleton=true FOR UPDATE`).Scan(&db.Incarnation, &db.Sequence); err != nil {
			return ErrClosed
		}
		var enabled bool
		var recoveryStarted bool
		if err = tx.QueryRow(ctx, `SELECT outbound_enabled,recovery_started_at IS NOT NULL
 FROM restore_safety_gate WHERE singleton=true FOR SHARE`).Scan(&enabled, &recoveryStarted); err != nil {
			return ErrClosed
		}
		if rebind {
			var unresolved int
			if enabled || !recoveryStarted {
				return ErrClosed
			}
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM business_actions WHERE kind='slack_invoice_paid'
 AND status='unknown' AND error_class='restore_uncertain'`).Scan(&unresolved); err != nil || unresolved != 0 {
				return ErrClosed
			}
		} else if !enabled || db.Sequence != 0 {
			return ErrClosed
		}
		action := "enroll"
		if rebind {
			action = "rebind"
		}
		if _, err = tx.Exec(ctx, `INSERT INTO restore_witness_audit(action,reason,incarnation,effect_sequence)
 VALUES($1,$2,$3,$4)`, action, reason, db.Incarnation, db.Sequence); err != nil {
			return ErrClosed
		}
		if err = tx.Commit(ctx); err != nil {
			return ErrClosed
		}
		db.Version = 1
		return w.write(db)
	})
}
