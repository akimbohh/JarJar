package store

import "time"

// Role values.
const (
	RolePlayer = "player"
	RoleAdmin  = "admin"
)

// Request status values and the legal transition set live in status.go.
const (
	StatusQueued                = "queued"
	StatusPlanning              = "planning"
	StatusAwaitingClarification = "awaiting_clarification"
	StatusAwaitingApproval      = "awaiting_approval"
	StatusMaterializing         = "materializing"
	StatusApplyingServer        = "applying_server"
	StatusPublished             = "published"
	StatusFailed                = "failed"
	StatusRejected              = "rejected"
	StatusInfeasible            = "infeasible"
)

// Event types.
const (
	EventVersionPublished = "version_published"
	EventRequestUpdated   = "request_updated"
	EventServerStatus     = "server_status"
	EventSetupProgress    = "setup_progress"
)

type Player struct {
	ID        string
	Name      string
	Role      string
	CreatedAt time.Time
}

type Invite struct {
	Code      string
	Role      string
	CreatedAt time.Time
	UsedBy    *string
}

type Request struct {
	ID                    string
	PlayerID              string
	PlayerName            string // joined from players
	Text                  string
	Status                string
	PlanJSON              *string
	ClarificationQuestion *string
	ClarificationAnswer   *string
	ClarificationRounds   int
	Error                 *string
	Version               *int
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type Version struct {
	Number       int
	CreatedAt    time.Time
	RequestID    *string
	GitCommit    string
	Summary      string
	Status       string
	ManifestPath string
}

type Event struct {
	Seq       int64
	Type      string
	Payload   []byte // raw JSON
	CreatedAt time.Time
}
