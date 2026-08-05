package worker

// Plan mirrors schemas/plan.schema.json — the structured output of the headless
// planning run. Action carries the union of fields across action types; the
// validator enforces which are required per type.
type Plan struct {
	SchemaVersion         int      `json:"schema_version"`
	Status                string   `json:"status"`
	Actions               []Action `json:"actions"`
	Summary               string   `json:"summary"`
	AdminNotes            string   `json:"admin_notes"`
	Confidence            string   `json:"confidence"`
	ClarificationQuestion *string  `json:"clarification_question"`
}

type Action struct {
	Type string `json:"type"`

	// add_mod / update_mod
	Platform    string `json:"platform,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	ProjectSlug string `json:"project_slug,omitempty"`
	VersionID   string `json:"version_id,omitempty"`
	FromPath    string `json:"from_path,omitempty"`

	// remove_mod / config_edit
	Path        string `json:"path,omitempty"`
	Description string `json:"description,omitempty"`

	// server_property
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`

	Reason string `json:"reason,omitempty"`
}

// Plan status values.
const (
	PlanOK                 = "ok"
	PlanNeedsClarification = "needs_clarification"
	PlanInfeasible         = "infeasible"
)

// Action types.
const (
	ActAddMod         = "add_mod"
	ActRemoveMod      = "remove_mod"
	ActUpdateMod      = "update_mod"
	ActConfigEdit     = "config_edit"
	ActServerProperty = "server_property"
)

// Confidence values.
const (
	ConfHigh   = "high"
	ConfMedium = "medium"
	ConfLow    = "low"
)

// rollbackPlan marks a synthetic rollback request. Stored in requests.plan_json
// as {"rollback_to": N}; the worker detects it and runs the rollback path.
type rollbackPlan struct {
	RollbackTo int `json:"rollback_to"`
}
