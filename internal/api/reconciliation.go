package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lenoir-dev/LenoirDesk/internal/identity"
	"github.com/lenoir-dev/LenoirDesk/internal/ingress"
)

func (a *application) reconciliationStatusAPI(w http.ResponseWriter, r *http.Request) {
	status, err := a.identity.ReconciliationStatus(r.Context(), principal(r), chi.URLParam(r, "tenant"))
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 200, status)
}

func (a *application) requestHistoricalAPI(w http.ResponseWriter, r *http.Request) {
	var input identity.HistoricalReconcileInput
	if !decodeJSON(w, r, &input) {
		return
	}
	id, err := a.identity.RequestHistoricalReconciliation(r.Context(), principal(r), chi.URLParam(r, "tenant"), input, ingress.CorrelationID(r.Context()))
	if errors.Is(err, identity.ErrInvalidReconciliation) {
		apiError(w, r, 400, "invalid_reconciliation_bounds")
		return
	}
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]string{"id": id, "status": "pending"})
}

func (a *application) populateReconciliation(r *http.Request, v *uiView) error {
	var err error
	v.Operational, err = a.identity.ReconciliationStatusPage(r.Context(), principal(r), v.Tenant, r.URL.Query().Get("history_cursor"))
	if err != nil {
		return err
	}
	v.Connections, _, err = a.identity.ListConnections(r.Context(), principal(r), v.Tenant, 100, "")
	if err != nil {
		return err
	}
	eligible, err := a.identity.EligibleHistoricalConnectionIDs(r.Context(), principal(r), v.Tenant)
	if err != nil {
		return err
	}
	v.ReconcileState = reconciliationState(v.Operational, v.Connections)
	for _, checkpoint := range v.Operational.Checkpoints {
		if checkpoint.GapCode != "" {
			v.CoverageGaps++
		}
		if checkpoint.LastCompleteAt != nil && (v.LastCompleteAt == nil || checkpoint.LastCompleteAt.After(*v.LastCompleteAt)) {
			v.LastCompleteAt = checkpoint.LastCompleteAt
		}
	}
	for _, connection := range v.Connections {
		if eligible[connection.ID] {
			v.ReconcileConnections = append(v.ReconcileConnections, connection)
			v.HasReconcileConnection = true
		}
	}
	return nil
}

func reconciliationState(status identity.OperationalStatus, connections []identity.Connection) string {
	if !status.OutboundEnabled {
		return "blocked"
	}
	active := make([]string, 0, len(connections))
	for _, connection := range connections {
		if connection.Provider == "stripe" && connection.Status == "active" &&
			(connection.HealthStatus != "verified" || connection.AuthState == "blocked_auth") {
			return "blocked"
		}
		if connection.Provider == "stripe" && connection.Status == "active" && connection.HealthStatus == "verified" {
			active = append(active, connection.ID)
		}
	}
	if len(status.Checkpoints) == 0 {
		return "unverified"
	}
	if len(active) == 0 {
		return "blocked"
	}
	complete := make(map[string]map[string]bool)
	gap := false
	for _, checkpoint := range status.Checkpoints {
		if checkpoint.GapCode != "" {
			gap = true
		}
		if checkpoint.LastCompleteAt != nil {
			if complete[checkpoint.ConnectionID] == nil {
				complete[checkpoint.ConnectionID] = make(map[string]bool)
			}
			complete[checkpoint.ConnectionID][checkpoint.Kind] = true
		}
	}
	if gap {
		return "degraded"
	}
	if status.UnknownActions > 0 || status.DeadActions > 0 || status.OldestOutboxSeconds > 300 || status.OldestDueSeconds > 300 {
		return "degraded"
	}
	for _, id := range active {
		if !complete[id]["events"] || !complete[id]["known"] {
			return "unverified"
		}
	}
	return "healthy"
}

func (a *application) reconciliationScreen(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Reconciliation")
	if !ok {
		return
	}
	if err := a.populateReconciliation(r, &v); err != nil {
		appError(w, r, err)
		return
	}
	v.Historical.FromAt = time.Now().UTC().Add(-24 * time.Hour)
	v.Historical.ToAt = time.Now().UTC().Add(-5 * time.Minute)
	v.HistoricalDraft = historicalDraft{From: v.Historical.FromAt.UTC().Format("2006-01-02T15:04:05"),
		To: v.Historical.ToAt.UTC().Format("2006-01-02T15:04:05"), Max: "100"}
	a.renderUI(w, 200, "reconciliation", v)
}

func (a *application) reconciliationFormError(w http.ResponseWriter, r *http.Request, v uiView, code string) {
	if err := a.populateReconciliation(r, &v); err != nil {
		appError(w, r, err)
		return
	}
	v.HistoricalDraft = historicalDraft{Connection: r.FormValue("connection_id"), From: r.FormValue("from_at"),
		To: r.FormValue("to_at"), Max: r.FormValue("max_events"), Reason: r.FormValue("reason")}
	v.Error = code
	a.renderUI(w, http.StatusBadRequest, "reconciliation", v)
}

func historicalForm(r *http.Request) (identity.HistoricalReconcileInput, error) {
	if err := r.ParseForm(); err != nil {
		return identity.HistoricalReconcileInput{}, identity.ErrInvalidReconciliation
	}
	from, err := parseHistoricalTime(r.FormValue("from_at"))
	if err != nil {
		return identity.HistoricalReconcileInput{}, identity.ErrInvalidReconciliation
	}
	to, err := parseHistoricalTime(r.FormValue("to_at"))
	if err != nil {
		return identity.HistoricalReconcileInput{}, identity.ErrInvalidReconciliation
	}
	limit, err := strconv.Atoi(r.FormValue("max_events"))
	if err != nil {
		return identity.HistoricalReconcileInput{}, identity.ErrInvalidReconciliation
	}
	input := identity.HistoricalReconcileInput{ConnectionID: r.FormValue("connection_id"), FromAt: from,
		ToAt: to, MaxEvents: limit, Reason: r.FormValue("reason")}
	return input, input.Validate()
}

func parseHistoricalTime(value string) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at.UTC(), nil
	}
	// The accessible datetime-local control is explicitly labelled UTC.
	return time.ParseInLocation("2006-01-02T15:04:05", value, time.UTC)
}

func (a *application) reconciliationPreviewScreen(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Reconciliation")
	if !ok {
		return
	}
	if !v.CanOperate {
		apiError(w, r, 403, "forbidden")
		return
	}
	input, err := historicalForm(r)
	if err != nil {
		a.reconciliationFormError(w, r, v, "reconcile_invalid_bounds")
		return
	}
	if err = a.populateReconciliation(r, &v); err != nil {
		appError(w, r, err)
		return
	}
	verified := false
	for _, connection := range v.ReconcileConnections {
		if connection.ID == input.ConnectionID {
			verified = true
			break
		}
	}
	if !verified {
		a.reconciliationFormError(w, r, v, "reconcile_connection_unavailable")
		return
	}
	v.Historical = input
	v.EstimatedPages = (input.MaxEvents + 24) / 25
	a.renderUI(w, 200, "reconciliation_preview", v)
}

func (a *application) reconciliationSubmit(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Reconciliation")
	if !ok {
		return
	}
	if !v.CanOperate {
		apiError(w, r, 403, "forbidden")
		return
	}
	input, err := historicalForm(r)
	if err != nil {
		a.reconciliationFormError(w, r, v, "reconcile_invalid_bounds")
		return
	}
	_, err = a.identity.RequestHistoricalReconciliation(r.Context(), principal(r), v.Tenant, input, ingress.CorrelationID(r.Context()))
	if errors.Is(err, identity.ErrInvalidReconciliation) {
		a.reconciliationFormError(w, r, v, "reconcile_connection_unavailable")
		return
	}
	if err != nil {
		appError(w, r, err)
		return
	}
	http.Redirect(w, r, "/ui/tenants/"+v.Tenant+"/reconciliation?notice=reconciliation_requested", http.StatusSeeOther)
}
