package business

// ActionStatus is the persisted business-effect state. The table is deliberately
// small: a caller must choose an explicit transition rather than assigning a
// plausible-looking status after an external request.
type ActionStatus string

const (
	ActionPending        ActionStatus = "pending"
	ActionQueued         ActionStatus = "queued"
	ActionRunning        ActionStatus = "running"
	ActionRetryWait      ActionStatus = "retry_wait"
	ActionBlockedAuth    ActionStatus = "blocked_auth"
	ActionBlockedConfig  ActionStatus = "blocked_config"
	ActionPaused         ActionStatus = "paused"
	ActionUnknown        ActionStatus = "unknown"
	ActionNeedsAttention ActionStatus = "needs_attention"
	ActionDead           ActionStatus = "dead"
	ActionSucceeded      ActionStatus = "succeeded"
	ActionCanceled       ActionStatus = "canceled"
	ActionSuperseded     ActionStatus = "superseded"
)

type ActionTransition string

const (
	TransitionDispatch       ActionTransition = "dispatch"
	TransitionClaim          ActionTransition = "claim"
	TransitionFinish         ActionTransition = "finish"
	TransitionPause          ActionTransition = "pause"
	TransitionResume         ActionTransition = "resume"
	TransitionManualRetry    ActionTransition = "manual_retry"
	TransitionConfirmUnknown ActionTransition = "confirm_unknown"
	TransitionLeaveUnknown   ActionTransition = "leave_unknown"
	TransitionRiskRetry      ActionTransition = "risk_retry"
	TransitionCancel         ActionTransition = "cancel"
	TransitionSupersede      ActionTransition = "supersede"
)

// CanTransition is the application-side contract. SQL predicates and row locks
// enforce the same boundaries for concurrent actors.
func CanTransition(from, to ActionStatus, reason ActionTransition) bool {
	switch reason {
	case TransitionDispatch:
		return (from == ActionPending || from == ActionRetryWait) && to == ActionQueued
	case TransitionClaim:
		return from == ActionQueued && to == ActionRunning
	case TransitionFinish:
		if from != ActionRunning {
			return false
		}
		switch to {
		case ActionSucceeded, ActionRetryWait, ActionBlockedAuth, ActionBlockedConfig,
			ActionPaused, ActionUnknown, ActionNeedsAttention, ActionDead:
			return true
		}
	case TransitionPause:
		return (from == ActionPending || from == ActionQueued || from == ActionRetryWait) && to == ActionPaused
	case TransitionResume:
		return from == ActionPaused && to == ActionPending
	case TransitionManualRetry:
		return (from == ActionDead || from == ActionNeedsAttention || from == ActionBlockedAuth || from == ActionBlockedConfig) && to == ActionPending
	case TransitionConfirmUnknown:
		return from == ActionUnknown && to == ActionSucceeded
	case TransitionLeaveUnknown:
		return from == ActionUnknown && to == ActionUnknown
	case TransitionRiskRetry:
		return from == ActionUnknown && to == ActionPending
	case TransitionCancel:
		return (from == ActionPending || from == ActionQueued || from == ActionRetryWait || from == ActionPaused) && to == ActionCanceled
	case TransitionSupersede:
		return (from == ActionPending || from == ActionQueued || from == ActionRetryWait || from == ActionPaused) && to == ActionSuperseded
	}
	return false
}
