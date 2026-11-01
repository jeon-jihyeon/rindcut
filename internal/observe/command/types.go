package command

import "time"

const (
	SourceProtocol   = "rindcut.source/v1"
	ObserverProtocol = "rindcut.observer/v1"
	MaxOutput        = 1 << 20
)

type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }

type Capability struct {
	Kind        string   `json:"kind"`
	Types       []string `json:"types"`
	Limitations []string `json:"limitations"`
}

type Coverage struct {
	Kind        string   `json:"kind"`
	Limitations []string `json:"limitations"`
	WindowStart *string  `json:"window_start,omitempty"`
	WindowEnd   *string  `json:"window_end,omitempty"`
}

type Reader struct {
	Program           string   `json:"program"`
	Args              []string `json:"args"`
	AllowedAttributes []string `json:"allowed_attributes"`
	TimeoutSeconds    int      `json:"timeout_seconds,omitempty" positive:"true"`
	Limit             int      `json:"limit,omitempty" positive:"true"`
	FingerprintFiles  []string `json:"fingerprint_files,omitempty"`
}

type Observer struct {
	ID                string         `json:"id"`
	Types             []string       `json:"types"`
	CriterionID       string         `json:"criterion_id"`
	CriterionRevision string         `json:"criterion_revision"`
	Parameters        map[string]any `json:"parameters"`
	Program           string         `json:"program"`
	Args              []string       `json:"args"`
	DeadlineSeconds   int            `json:"deadline_seconds"`
	IntervalSeconds   int            `json:"interval_seconds,omitempty" positive:"true"`
	Mode              string         `json:"mode,omitempty"`
	TimeoutSeconds    int            `json:"timeout_seconds,omitempty" positive:"true"`
	FingerprintFiles  []string       `json:"fingerprint_files,omitempty"`
	ActorKind         string         `json:"actor_kind,omitempty"`
}

type Definition struct {
	ID                string     `json:"id"`
	Enabled           bool       `json:"enabled"`
	SourceInstance    string     `json:"source_instance"`
	Stream            string     `json:"stream"`
	Capability        Capability `json:"capability"`
	AdapterRevision   string     `json:"adapter_revision"`
	ProjectionProfile string     `json:"projection_profile"`
	Reader            Reader     `json:"reader"`
	Observers         []Observer `json:"observers"`
	Description       string     `json:"description,omitempty"`
}

type EventRef struct {
	SourceID          string `json:"source_id"`
	SourceInstance    string `json:"source_instance"`
	Generation        string `json:"generation"`
	Stream            string `json:"stream"`
	Type              string `json:"type"`
	ID                string `json:"id"`
	ProjectionProfile string `json:"projection_profile"`
}

type Link struct {
	Relation string   `json:"relation"`
	Target   EventRef `json:"target"`
}

type EvidenceRef struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
}

type Evidence struct {
	Kind   string      `json:"kind"`
	Ref    EvidenceRef `json:"ref"`
	Digest *string     `json:"digest,omitempty"`
}

type Event struct {
	SourceInstance    string         `json:"source_instance"`
	Generation        string         `json:"generation"`
	Stream            string         `json:"stream"`
	ID                string         `json:"id"`
	Type              string         `json:"type"`
	SchemaVersion     string         `json:"schema_version"`
	ProducerVersion   *string        `json:"producer_version,omitempty"`
	AdapterRevision   string         `json:"adapter_revision"`
	ProjectionProfile string         `json:"projection_profile"`
	OccurredAt        *string        `json:"occurred_at,omitempty"`
	ActionID          *string        `json:"action_id,omitempty"`
	AttemptID         *string        `json:"attempt_id,omitempty"`
	Links             []Link         `json:"links"`
	Attributes        map[string]any `json:"attributes"`
	Evidence          []Evidence     `json:"evidence"`
}

type Capture struct {
	Event
	SourceID         string `json:"source_id"`
	CaptureID        string `json:"capture_id"`
	CapturedAt       string `json:"captured_at"`
	NormalizedDigest string `json:"normalized_digest"`
	SourceRevision   int64  `json:"source_revision"`
}

type ReadRequest struct {
	Protocol       string  `json:"protocol"`
	Operation      string  `json:"operation"`
	SourceID       string  `json:"source_id"`
	SourceRevision int64   `json:"source_revision"`
	SourceInstance string  `json:"source_instance"`
	Stream         string  `json:"stream"`
	Generation     *string `json:"generation" nullable:"true"`
	Cursor         *string `json:"cursor" nullable:"true"`
	Limit          int     `json:"limit"`
}

type Page struct {
	Protocol       string     `json:"protocol"`
	SourceID       string     `json:"source_id"`
	SourceRevision int64      `json:"source_revision"`
	SourceInstance string     `json:"source_instance"`
	Stream         string     `json:"stream"`
	Generation     *string    `json:"generation" nullable:"true"`
	Capability     Capability `json:"capability"`
	Status         string     `json:"status"`
	Records        []Event    `json:"records"`
	NextCursor     *string    `json:"next_cursor" nullable:"true"`
	HasMore        bool       `json:"has_more"`
	Coverage       Coverage   `json:"coverage"`
}

type TargetRef struct {
	StoreID   string `json:"store_id"`
	CaptureID string `json:"capture_id"`
}

type ObservationRef struct {
	StoreID       string `json:"store_id"`
	ObservationID string `json:"observation_id"`
}

type ObserveRequest struct {
	Protocol              string         `json:"protocol"`
	SourceID              string         `json:"source_id"`
	SourceRevision        int64          `json:"source_revision"`
	ObserverID            string         `json:"observer_id"`
	ObserverRevision      int64          `json:"observer_revision"`
	CriterionID           string         `json:"criterion_id"`
	CriterionRevision     string         `json:"criterion_revision"`
	SeriesID              string         `json:"series_id"`
	Parameters            map[string]any `json:"parameters"`
	TargetRef             TargetRef      `json:"target_ref"`
	Event                 Capture        `json:"event"`
	PreviousObservationID *string        `json:"previous_observation_id,omitempty"`
	Now                   string         `json:"now"`
}

type ObservationResponse struct {
	Protocol          string          `json:"protocol"`
	SourceID          string          `json:"source_id"`
	SourceRevision    int64           `json:"source_revision"`
	ObserverID        string          `json:"observer_id"`
	ObserverRevision  int64           `json:"observer_revision"`
	CriterionID       string          `json:"criterion_id"`
	CriterionRevision string          `json:"criterion_revision"`
	SeriesID          string          `json:"series_id"`
	TargetRef         TargetRef       `json:"target_ref"`
	Result            string          `json:"result"`
	ReasonCode        string          `json:"reason_code"`
	Evidence          []Evidence      `json:"evidence"`
	NextObserveAt     *string         `json:"next_observe_at,omitempty"`
	Supersedes        *ObservationRef `json:"supersedes,omitempty"`
}

func Timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
