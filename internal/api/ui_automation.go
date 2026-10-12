package api

import (
	"regexp"

	"github.com/lenoir-dev/LenoirDesk/internal/business"
	"github.com/lenoir-dev/LenoirDesk/internal/identity"
)

var uiPriceID = regexp.MustCompile(`^price_[A-Za-z0-9_-]{1,154}$`)

func automationFieldErrors(cfg business.Config) map[string]string {
	errors := map[string]string{}
	if cfg.StripeConnectionID == "" {
		errors["stripe"] = "automation_connection_required"
	}
	if cfg.HubSpotConnectionID == "" {
		errors["hubspot"] = "automation_connection_required"
	}
	if cfg.SlackConnectionID == "" {
		errors["slack"] = "automation_connection_required"
	}
	if len(cfg.TrackedPriceIDs) < 1 || len(cfg.TrackedPriceIDs) > 20 {
		errors["prices"] = "automation_prices_count_error"
	} else {
		seen := map[string]bool{}
		for _, id := range cfg.TrackedPriceIDs {
			if !uiPriceID.MatchString(id) {
				errors["prices"] = "automation_prices_format_error"
				break
			}
			if seen[id] {
				errors["prices"] = "automation_prices_duplicate_error"
				break
			}
			seen[id] = true
		}
	}
	if !slackChannel.MatchString(cfg.SlackChannelID) {
		errors["channel"] = "automation_channel_error"
	}
	if cfg.CreationPolicy != "explicit_only" && cfg.CreationPolicy != "unique_upsert" {
		errors["policy"] = "automation_policy_error"
	}
	return errors
}

func nextRevision(revisions []identity.BillingRevision) int {
	next := 1
	for _, revision := range revisions {
		if revision.Revision >= next {
			next = revision.Revision + 1
		}
	}
	return next
}

func hasVerifiedProvider(connections []identity.Connection, provider string) bool {
	for _, c := range connections {
		if selectableConnection(c, provider, c.ID) {
			return true
		}
	}
	return false
}

func selectableConnection(c identity.Connection, provider, id string) bool {
	return c.ID == id && c.Provider == provider && c.Status == "active" && c.HealthStatus == "verified" &&
		c.Mode == "test" && (c.AuthState == "" || c.AuthState == "ready")
}

func runFilterCount(counts map[string]int64, filter string) int64 {
	switch filter {
	case "in_progress":
		return counts["pending"] + counts["running"]
	case "blocked":
		return counts["blocked_auth"] + counts["blocked_config"]
	case "":
		var total int64
		for _, n := range counts {
			total += n
		}
		return total
	default:
		return counts[filter]
	}
}

func runStepLabel(run identity.Run) string {
	if run.Status == "succeeded" {
		return "run_step_complete"
	}
	switch run.Step {
	case "crm_projection":
		return "run_step_crm"
	case "slack_notification":
		return "run_step_slack"
	default:
		return "run_step_planning"
	}
}

func runStatusHelp(status string) string {
	switch status {
	case "succeeded", "pending", "running", "retry_wait", "blocked_auth", "blocked_config",
		"needs_attention", "unknown", "dead", "paused", "skipped", "canceled", "superseded":
		return "run_help_" + status
	default:
		return "run_help_other"
	}
}
