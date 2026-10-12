package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/identity"
	"github.com/lenoir-dev/LenoirDesk/internal/ingress"
)

func (a *application) runDetail(w http.ResponseWriter, r *http.Request) {
	d, err := a.identity.BusinessRun(r.Context(), principal(r), chi.URLParam(r, "tenant"), chi.URLParam(r, "run"))
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 200, d)
}
func (a *application) retryBusinessAction(w http.ResponseWriter, r *http.Request) {
	var req identity.RecoveryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Decision = "manual_retry"
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	a.recoverBusinessAction(w, r, req)
}
func (a *application) resolveBusinessAction(w http.ResponseWriter, r *http.Request) {
	var req identity.RecoveryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Decision == "manual_retry" {
		apiError(w, r, 400, "invalid_recovery_decision")
		return
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	a.recoverBusinessAction(w, r, req)
}
func (a *application) recoverBusinessAction(w http.ResponseWriter, r *http.Request, req identity.RecoveryRequest) {
	result, err := a.identity.RecoverBusinessAction(r.Context(), principal(r), chi.URLParam(r, "tenant"),
		chi.URLParam(r, "action"), req, ingress.CorrelationID(r.Context()))
	if err != nil {
		category := "internal"
		switch {
		case errors.Is(err, identity.ErrInvalidRecovery):
			category = "invalid"
			apiError(w, r, 400, "invalid_recovery_decision")
		case errors.Is(err, identity.ErrRecoveryConflict):
			category = "conflict"
			apiError(w, r, 409, "recovery_conflict")
		default:
			appError(w, r, err)
		}
		a.log.Warn("business recovery rejected", "category", category, "request_id", ingress.CorrelationID(r.Context()))
		return
	}
	writeJSON(w, 200, result)
}
func (a *application) billingSync(w http.ResponseWriter, r *http.Request) {
	d, err := a.identity.Billing(r.Context(), principal(r), chi.URLParam(r, "tenant"), chi.URLParam(r, "stripe"))
	if err != nil {
		appError(w, r, err)
		return
	}
	writeJSON(w, 200, d)
}
func (a *application) configureBillingSync(w http.ResponseWriter, r *http.Request) {
	var cfg business.Config
	if !decodeJSON(w, r, &cfg) {
		return
	}
	if cfg.ActivationTime.IsZero() {
		cfg.ActivationTime = time.Now().UTC()
	}
	if cfg.ActivationTime.Before(time.Now().UTC().Add(-30 * time.Second)) {
		apiError(w, r, 400, "activation_must_be_future_only")
		return
	}
	id, err := a.identity.ConfigureBilling(r.Context(), principal(r), chi.URLParam(r, "tenant"), cfg, ingress.CorrelationID(r.Context()))
	if err != nil {
		if errors.Is(err, identity.ErrForbidden) {
			appError(w, r, err)
			return
		}
		apiError(w, r, 400, "invalid_billing_configuration")
		return
	}
	writeJSON(w, 201, map[string]string{"revision_id": id})
}
func (a *application) pauseBillingSync(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused bool `json:"paused"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := a.identity.PauseBilling(r.Context(), principal(r), chi.URLParam(r, "tenant"), chi.URLParam(r, "stripe"), body.Paused, ingress.CorrelationID(r.Context())); err != nil {
		appError(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (a *application) linkBillingContact(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HubSpotConnectionID string `json:"hubspot_connection_id"`
		CustomerKey         string `json:"customer_key"`
		ContactID           string `json:"contact_id"`
		Reason              string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := a.identity.LinkContactReviewed(r.Context(), principal(r), chi.URLParam(r, "tenant"), body.HubSpotConnectionID, body.CustomerKey, body.ContactID, strings.TrimSpace(body.Reason), ingress.CorrelationID(r.Context())); err != nil {
		if errors.Is(err, identity.ErrContactConflict) || errors.Is(err, identity.ErrContactMissing) {
			apiError(w, r, 409, "mapping_conflict")
		} else {
			appError(w, r, err)
		}
		return
	}
	w.WriteHeader(204)
}

func (a *application) recoverBusinessActionUI(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	tenant, runID, actionID := chi.URLParam(r, "tenant"), chi.URLParam(r, "run"), chi.URLParam(r, "action")
	d, err := a.identity.BusinessRun(r.Context(), principal(r), tenant, runID)
	if err != nil {
		http.Error(w, "Run unavailable", 404)
		return
	}
	found := false
	for _, item := range d.Actions {
		if item.ID == actionID {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "Action unavailable", 404)
		return
	}
	generation, err := strconv.ParseInt(r.FormValue("generation"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid generation", 400)
		return
	}
	req := identity.RecoveryRequest{Decision: r.FormValue("decision"), Reason: r.FormValue("reason"),
		ExpectedGeneration: generation, ExternalChannel: r.FormValue("external_channel"),
		ExternalTS: r.FormValue("external_ts"), AcknowledgeDuplicateRisk: r.FormValue("ack_risk") == "true"}
	_, err = a.identity.RecoverBusinessAction(r.Context(), principal(r), tenant, actionID, req, ingress.CorrelationID(r.Context()))
	if err != nil {
		a.log.Warn("business recovery rejected", "category", "ui", "request_id", ingress.CorrelationID(r.Context()))
		http.Error(w, "Recovery rejected; reload the run and check its current state", http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/ui/tenants/"+tenant+"/runs/"+runID+"?notice=recovered", http.StatusSeeOther)
}

func (a *application) configureBillingSyncUI(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Automation")
	if !ok {
		return
	}
	if !v.CanOperate {
		apiError(w, r, http.StatusForbidden, "forbidden")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	cfg := billingForm(r)
	if cfg.Validate() != nil || len(automationFieldErrors(cfg)) != 0 {
		a.automationError(w, r, v, "automation_invalid_form")
		return
	}
	_, err := a.identity.ConfigureBilling(r.Context(), principal(r), chi.URLParam(r, "tenant"), cfg, ingress.CorrelationID(r.Context()))
	if err != nil {
		if err := a.populateAutomation(r, &v); err != nil {
			appError(w, r, err)
			return
		}
		v.Detail.Config = cfg
		v.Error = "automation_activation_failed"
		a.renderUI(w, http.StatusConflict, "automation_preview", v)
		return
	}
	http.Redirect(w, r, "/ui/tenants/"+chi.URLParam(r, "tenant")+"/billing-sync?notice=activated", http.StatusSeeOther)
}
func (a *application) pauseBillingSyncUI(w http.ResponseWriter, r *http.Request) {
	v, ok := a.uiBase(w, r, "Automation")
	if !ok {
		return
	}
	if !v.CanOperate {
		apiError(w, r, http.StatusForbidden, "forbidden")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	err := a.identity.PauseBilling(r.Context(), principal(r), chi.URLParam(r, "tenant"), r.FormValue("stripe"), r.FormValue("paused") == "true", ingress.CorrelationID(r.Context()))
	if err != nil {
		if err := a.populateAutomation(r, &v); err != nil {
			appError(w, r, err)
			return
		}
		v.Error = "automation_pause_failed"
		a.renderUI(w, http.StatusConflict, "automation", v)
		return
	}
	notice := "resumed"
	if r.FormValue("paused") == "true" {
		notice = "paused"
	}
	http.Redirect(w, r, "/ui/tenants/"+chi.URLParam(r, "tenant")+"/billing-sync?notice="+notice, http.StatusSeeOther)
}
