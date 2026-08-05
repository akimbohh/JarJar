package store

// legalTransitions encodes the request-status state machine from
// DATA-CONTRACTS.md §7. A terminal status has no entry.
var legalTransitions = map[string]map[string]bool{
	StatusQueued: {
		StatusPlanning: true,
		StatusFailed:   true,
	},
	StatusPlanning: {
		StatusAwaitingClarification: true,
		StatusAwaitingApproval:      true,
		StatusMaterializing:         true,
		StatusInfeasible:            true,
		StatusFailed:                true,
	},
	StatusAwaitingClarification: {
		StatusPlanning: true,
		StatusFailed:   true,
	},
	StatusAwaitingApproval: {
		StatusMaterializing: true,
		StatusRejected:      true,
	},
	StatusMaterializing: {
		StatusApplyingServer: true,
		StatusFailed:         true,
	},
	StatusApplyingServer: {
		StatusPublished: true,
		StatusFailed:    true,
	},
}

// IsTerminal reports whether a request in this status will never change again.
func IsTerminal(status string) bool {
	switch status {
	case StatusPublished, StatusFailed, StatusRejected, StatusInfeasible:
		return true
	}
	return false
}

// CanTransition reports whether from → to is a legal request-status transition.
func CanTransition(from, to string) bool {
	return legalTransitions[from][to]
}
