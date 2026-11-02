package app

import (
	"encoding/json"
	"time"
)

type SourceRequest struct {
	Operation          string          `json:"operation,omitempty"`
	Definition         json.RawMessage `json:"definition,omitempty"`
	ExpectedRevision   *int64          `json:"expected_revision,omitempty"`
	Recovery           *SourceRecovery `json:"recovery,omitempty"`
	ExpectedStoreToken string          `json:"expected_store_token,omitempty"`
	PreviewDigest      string          `json:"preview_digest,omitempty"`
	Confirmed          bool            `json:"confirmed,omitempty"`
}

type SourceRecovery struct {
	Mode               string `json:"mode"`
	CheckpointToken    string `json:"checkpoint_token,omitempty"`
	CollectionIdentity string `json:"collection_identity,omitempty"`
	Generation         string `json:"generation,omitempty"`
}

type SourcePreview struct {
	Operation          string            `json:"operation"`
	PreviewDigest      string            `json:"preview_digest"`
	Definition         json.RawMessage   `json:"definition,omitempty"`
	ProposedRevision   int64             `json:"proposed_revision,omitempty"`
	CollectionIdentity string            `json:"collection_identity,omitempty"`
	CheckpointToken    string            `json:"checkpoint_token,omitempty"`
	ExpectedStoreToken string            `json:"expected_store_token,omitempty"`
	Effects            []string          `json:"effects"`
	Recovery           *SourceRecovery   `json:"recovery,omitempty"`
	StoreRecovery      *StoreRecovery    `json:"store_recovery,omitempty"`
	Executions         []SourceExecution `json:"executions,omitempty"`
	Schedule           *SchedulePlan     `json:"schedule,omitempty"`
}

type SchedulePlan struct {
	OS              string `json:"os"`
	UID             int    `json:"uid"`
	Program         string `json:"program"`
	Home            string `json:"home"`
	UserHome        string `json:"user_home"`
	ClaudeConfigDir string `json:"claude_config_dir,omitempty"`
	Name            string `json:"name,omitempty"`
	IntervalSeconds int    `json:"interval_seconds"`
	Effect          string `json:"effect"`
}

type ScheduleStatus struct {
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	Changed bool   `json:"changed,omitempty"`
}

type SourceExecution struct {
	Role         string              `json:"role"`
	ObserverID   string              `json:"observer_id,omitempty"`
	Program      string              `json:"program"`
	Dir          string              `json:"dir"`
	Args         []string            `json:"args"`
	Fingerprints []SourceFingerprint `json:"fingerprints"`
}

type SourceFingerprint struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type StoreRecovery struct {
	Stage          string `json:"stage"`
	StoreID        string `json:"store_id,omitempty"`
	NewStoreID     string `json:"new_store_id,omitempty"`
	ContinuityLost bool   `json:"continuity_lost"`
}

type SourceSaved struct {
	Source   *SourceSummary  `json:"source,omitempty"`
	Recovery *StoreRecovery  `json:"recovery,omitempty"`
	Schedule *ScheduleStatus `json:"schedule,omitempty"`
}

type SourceRemoveRequest struct {
	ID               string `json:"id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Confirmed        bool   `json:"confirmed"`
}

type SourceCapability struct {
	Kind        string   `json:"kind"`
	Types       []string `json:"types"`
	Limitations []string `json:"limitations"`
}

type SourceObserverSummary struct {
	ID                string `json:"id"`
	Revision          int64  `json:"revision"`
	CriterionID       string `json:"criterion_id"`
	CriterionRevision string `json:"criterion_revision"`
	ActorKind         string `json:"actor_kind"`
}

type SourceSummary struct {
	ID                 string                  `json:"id"`
	Revision           int64                   `json:"revision"`
	Enabled            bool                    `json:"enabled"`
	Removed            bool                    `json:"removed"`
	SourceInstance     string                  `json:"source_instance"`
	Stream             string                  `json:"stream"`
	Capability         SourceCapability        `json:"capability"`
	AdapterRevision    string                  `json:"adapter_revision"`
	ProjectionProfile  string                  `json:"projection_profile"`
	Description        string                  `json:"description,omitempty"`
	Observers          []SourceObserverSummary `json:"observers"`
	CollectionIdentity string                  `json:"collection_identity"`
	CheckpointToken    string                  `json:"checkpoint_token"`
}

type ObserveTarget struct {
	StoreID   string `json:"store_id"`
	CaptureID string `json:"capture_id"`
}

type ObserveQuery struct {
	SourceID  string         `json:"source_id,omitempty"`
	Type      string         `json:"type,omitempty"`
	ActionID  string         `json:"action_id,omitempty"`
	TargetRef *ObserveTarget `json:"target_ref,omitempty"`
	Kind      string         `json:"kind,omitempty"`
	Cursor    string         `json:"cursor,omitempty"`
	Limit     int            `json:"limit,omitempty"`
}

type ObserveRecord struct {
	ID   string          `json:"id"`
	Kind string          `json:"kind"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

type ObservePage struct {
	Records      []ObserveRecord `json:"records"`
	NextCursor   *string         `json:"next_cursor" nullable:"true"`
	HasMore      bool            `json:"has_more"`
	StorePresent bool            `json:"store_present"`
}

type ObserveCycle struct {
	ID           string    `json:"id"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	Attempts     int       `json:"attempts"`
	Captured     int       `json:"captured"`
	Observations int       `json:"observations"`
	Incomplete   bool      `json:"incomplete"`
	Errors       []string  `json:"errors"`
}

type ObserveSourceStatus struct {
	Source         SourceSummary    `json:"source"`
	Generation     *string          `json:"generation,omitempty"`
	Blocked        string           `json:"blocked,omitempty"`
	Gap            bool             `json:"gap"`
	Conflicts      int              `json:"conflicts"`
	Pending        int              `json:"pending"`
	Overdue        int              `json:"overdue"`
	LastReasonCode string           `json:"last_reason_code,omitempty"`
	Coverage       *ObserveCoverage `json:"coverage,omitempty"`
	LastAttemptAt  *time.Time       `json:"last_attempt_at,omitempty"`
	LastSuccessAt  *time.Time       `json:"last_success_at,omitempty"`
}

type ObserveCoverage struct {
	Kind        string   `json:"kind"`
	Limitations []string `json:"limitations"`
	WindowStart *string  `json:"window_start,omitempty"`
	WindowEnd   *string  `json:"window_end,omitempty"`
	ReportedBy  string   `json:"reported_by"`
}

type ObserveStatus struct {
	StorePresent bool                  `json:"store_present"`
	StoreID      string                `json:"store_id,omitempty"`
	Sources      []ObserveSourceStatus `json:"sources"`
}
