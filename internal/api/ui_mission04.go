package api

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/identity"
)

// These types only arrange persisted facts for display. Execution and recovery
// state remain exclusively owned by the existing business services.
type runStage struct {
	Provider, Status, Explanation string
	Action                        *identity.RunAction
}

type runTimelineItem struct {
	At          time.Time
	Label, Note string
	Order       int
}

func buildRunPresentation(d identity.RunDetail) ([]runStage, []runTimelineItem, bool) {
	stages := []runStage{{Provider: "stripe", Status: "succeeded", Explanation: "stage_source_recorded"},
		{Provider: "hubspot", Status: "not_planned", Explanation: "stage_crm_unplanned"},
		{Provider: "slack", Status: "not_planned", Explanation: "stage_slack_unplanned"}}
	if d.EventType != "invoice.paid" {
		stages[2].Status, stages[2].Explanation = "not_applicable", "stage_slack_not_applicable"
	} else if d.EventSource != "webhook" && d.EventSource != "" {
		stages[2].Status, stages[2].Explanation = "suppressed", "stage_slack_suppressed"
	}
	items := []runTimelineItem{{At: d.AcceptedAt, Label: "timeline_event_recorded", Note: d.EventType, Order: 0},
		{At: d.CreatedAt, Label: "timeline_run_planned", Note: d.ID, Order: 1}}
	crmNoWrite := false
	for i := range d.Actions {
		a := &d.Actions[i]
		stage := 1
		if a.Kind != "crm_projection" {
			stage = 2
		}
		stages[stage].Status = a.Status
		stages[stage].Action = a
		stages[stage].Explanation = "stage_action_" + a.Status
		if a.Kind == "crm_projection" && a.Status == "succeeded" {
			read, write := false, false
			for _, attempt := range a.Timeline {
				for _, call := range attempt.Calls {
					if call.Provider == "hubspot" {
						switch call.Method {
						case "GET":
							read = true
						case "POST", "PATCH", "PUT":
							write = true
						}
					}
				}
			}
			crmNoWrite = read && !write
			if crmNoWrite {
				stages[stage].Explanation = "stage_crm_no_write"
			}
		}
		for _, attempt := range a.Timeline {
			items = append(items, runTimelineItem{At: attempt.StartedAt, Label: "timeline_attempt", Note: fmt.Sprintf("%s · #%d", a.Kind, attempt.Number), Order: 2})
			for _, call := range attempt.Calls {
				note := call.Provider + " " + call.Method + " " + call.Path
				if call.Status != nil {
					note += fmt.Sprintf(" · HTTP %d", *call.Status)
				}
				items = append(items, runTimelineItem{At: call.StartedAt, Label: "timeline_provider_call", Note: note, Order: 3})
			}
			if attempt.FinishedAt != nil {
				items = append(items, runTimelineItem{At: *attempt.FinishedAt, Label: "timeline_attempt_result", Note: attempt.Outcome, Order: 4})
			}
		}
		for _, decision := range a.Decisions {
			items = append(items, runTimelineItem{At: decision.CreatedAt, Label: "timeline_operator_decision", Note: decision.Decision, Order: 5})
		}
	}
	if d.FinishedAt != nil {
		items = append(items, runTimelineItem{At: *d.FinishedAt, Label: "timeline_run_result", Note: d.Status, Order: 6})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].At.Equal(items[j].At) {
			return items[i].Order < items[j].Order
		}
		return items[i].At.Before(items[j].At)
	})
	return stages, items, crmNoWrite
}

func safeRunError(lang, class string) string {
	if class == "" {
		return tr(lang, "run_no_error")
	}
	switch {
	case strings.Contains(class, "identity"), strings.Contains(class, "mapped_contact"):
		return tr(lang, "run_error_identity")
	case strings.Contains(class, "auth"), strings.Contains(class, "credential"):
		return tr(lang, "run_error_auth")
	case strings.Contains(class, "quota"), strings.Contains(class, "rate_limited"):
		return tr(lang, "run_error_quota")
	case strings.Contains(class, "circuit"), strings.Contains(class, "connect"), strings.Contains(class, "timeout"):
		return tr(lang, "run_error_provider")
	case strings.Contains(class, "slack_result_unknown"):
		return tr(lang, "run_error_unknown_slack")
	default:
		if len(class) > 80 || !safeErrorCode.MatchString(class) {
			return tr(lang, "run_error_generic")
		}
		return tr(lang, "run_error_code") + " " + class
	}
}

func stageStatusLabel(status string) string {
	switch status {
	case "not_planned":
		return "stage_not_planned"
	case "not_applicable":
		return "stage_not_applicable"
	case "suppressed":
		return "stage_suppressed"
	default:
		return statusLabel(status)
	}
}

func timelineNote(lang, note string) string {
	switch note {
	case "succeeded", "pending", "running", "retry_wait", "blocked_auth", "blocked_config", "needs_attention", "unknown", "dead", "paused", "skipped", "canceled", "superseded":
		return tr(lang, statusLabel(note))
	case "confirm_unknown":
		return tr(lang, "timeline_delivery_confirmed")
	case "manual_retry":
		return tr(lang, "timeline_manual_retry")
	case "risk_retry":
		return tr(lang, "timeline_risk_retry")
	case "leave_unknown":
		return tr(lang, "timeline_leave_unknown")
	}
	if strings.HasPrefix(note, "crm_projection · ") {
		return tr(lang, "run_step_crm") + strings.TrimPrefix(note, "crm_projection")
	}
	if strings.HasPrefix(note, "slack_invoice_paid · ") {
		return tr(lang, "run_step_slack") + strings.TrimPrefix(note, "slack_invoice_paid")
	}
	return note
}

func auditLabel(lang, operation, result string) string {
	if decision, ok := strings.CutPrefix(operation, "business."); ok {
		switch decision {
		case "manual_retry", "confirm_unknown", "leave_unknown", "risk_retry":
			operation = timelineNote(lang, decision)
		}
	}
	if result == "success" {
		result = tr(lang, "audit_success")
	}
	return operation + " · " + result
}
