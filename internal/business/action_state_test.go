package business

import "testing"

func TestActionTransitionTable(t *testing.T) {
	all := []ActionStatus{ActionPending, ActionQueued, ActionRunning, ActionRetryWait, ActionBlockedAuth,
		ActionBlockedConfig, ActionPaused, ActionUnknown, ActionNeedsAttention, ActionDead,
		ActionSucceeded, ActionCanceled, ActionSuperseded}
	type edge struct {
		from, to ActionStatus
		reason   ActionTransition
	}
	allowed := map[edge]bool{}
	add := func(reason ActionTransition, from []ActionStatus, to ...ActionStatus) {
		for _, f := range from {
			for _, dest := range to {
				allowed[edge{f, dest, reason}] = true
			}
		}
	}
	add(TransitionDispatch, []ActionStatus{ActionPending, ActionRetryWait}, ActionQueued)
	add(TransitionClaim, []ActionStatus{ActionQueued}, ActionRunning)
	add(TransitionFinish, []ActionStatus{ActionRunning}, ActionSucceeded, ActionRetryWait, ActionBlockedAuth,
		ActionBlockedConfig, ActionPaused, ActionUnknown, ActionNeedsAttention, ActionDead)
	add(TransitionPause, []ActionStatus{ActionPending, ActionQueued, ActionRetryWait}, ActionPaused)
	add(TransitionResume, []ActionStatus{ActionPaused}, ActionPending)
	add(TransitionManualRetry, []ActionStatus{ActionDead, ActionNeedsAttention, ActionBlockedAuth, ActionBlockedConfig}, ActionPending)
	add(TransitionConfirmUnknown, []ActionStatus{ActionUnknown}, ActionSucceeded)
	add(TransitionLeaveUnknown, []ActionStatus{ActionUnknown}, ActionUnknown)
	add(TransitionRiskRetry, []ActionStatus{ActionUnknown}, ActionPending)
	add(TransitionCancel, []ActionStatus{ActionPending, ActionQueued, ActionRetryWait, ActionPaused}, ActionCanceled)
	add(TransitionSupersede, []ActionStatus{ActionPending, ActionQueued, ActionRetryWait, ActionPaused}, ActionSuperseded)
	reasons := []ActionTransition{TransitionDispatch, TransitionClaim, TransitionFinish, TransitionPause, TransitionResume,
		TransitionManualRetry, TransitionConfirmUnknown, TransitionLeaveUnknown, TransitionRiskRetry, TransitionCancel, TransitionSupersede}
	for _, reason := range reasons {
		for _, from := range all {
			for _, to := range all {
				if got, want := CanTransition(from, to, reason), allowed[edge{from, to, reason}]; got != want {
					t.Errorf("%s: %s -> %s: got %v, want %v", reason, from, to, got, want)
				}
			}
		}
	}
}
