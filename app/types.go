package app

import (
	"encoding/json"
	"fmt"
	"time"
)

type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var text string
	if err := json.Unmarshal(b, &text); err != nil {
		return fmt.Errorf("duration must be text like \"30m\": %w", err)
	}
	v, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

type CheckSpec struct {
	Kind     string            `json:"kind"`
	Path     string            `json:"path,omitempty"`
	MinBytes int64             `json:"min_bytes,omitempty"`
	Fresh    *bool             `json:"fresh,omitempty"`
	URL      string            `json:"url,omitempty"`
	Status   int               `json:"status,omitempty"`
	Contains string            `json:"contains,omitempty"`
	Run      string            `json:"run,omitempty"`
	Timeout  Duration          `json:"timeout,omitempty"`
	Scripts  map[string]string `json:"scripts,omitempty"`
}

type Criterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Critic struct {
	Source           string      `json:"source"`
	SourceRevision   string      `json:"source_revision"`
	CriteriaRevision string      `json:"criteria_revision"`
	Criteria         []Criterion `json:"criteria"`
	Command          CheckSpec   `json:"command"`
	RetainReason     bool        `json:"retain_reason,omitempty"`
}

type Job struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Schedule string      `json:"schedule"`
	Timezone string      `json:"timezone"`
	Grace    Duration    `json:"grace"`
	Evidence string      `json:"evidence"`
	Folder   string      `json:"folder,omitempty"`
	Marker   string      `json:"marker,omitempty"`
	Checks   []CheckSpec `json:"checks"`
	Critic   *Critic     `json:"critic,omitempty"`
	Revision int         `json:"revision"`
	Removed  bool        `json:"removed,omitempty"`
	Since    time.Time   `json:"-"`
}

type Entry struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	At      time.Time       `json:"at"`
	Version string          `json:"version"`
	Data    json.RawMessage `json:"data"`
}

func (e Entry) Decode(v any) error { return json.Unmarshal(e.Data, v) }

type Session struct {
	SessionID      string    `json:"session_id"`
	RunID          string    `json:"run_id,omitempty"`
	Cwd            string    `json:"cwd"`
	StartedAt      time.Time `json:"started_at"`
	EndedAt        time.Time `json:"ended_at"`
	FirstPrompt    string    `json:"first_prompt"`
	TranscriptPath string    `json:"transcript_path"`
	EndReason      string    `json:"end_reason,omitempty"`
	Source         string    `json:"source"`
}

type CheckRecord struct {
	Kind      string         `json:"kind"`
	Status    string         `json:"status"`
	Message   string         `json:"message"`
	Observed  map[string]any `json:"observed"`
	Truncated bool           `json:"truncated,omitempty"`
}

type CriterionJudgment struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
}

type CriticResult struct {
	Protocol         string              `json:"protocol"`
	Source           string              `json:"source"`
	SourceRevision   string              `json:"source_revision"`
	CriteriaRevision string              `json:"criteria_revision"`
	EvaluatedAt      time.Time           `json:"evaluated_at"`
	Verdict          string              `json:"verdict"`
	Criteria         []CriterionJudgment `json:"criteria,omitempty"`
	Cause            string              `json:"cause,omitempty"`
	OutputSHA256     string              `json:"output_sha256,omitempty"`
	Reason           string              `json:"reason,omitempty"`
}

type Judgment struct {
	JobID       string        `json:"job_id"`
	JobRevision int           `json:"job_revision"`
	SlotKey     string        `json:"slot_key"`
	Slot        time.Time     `json:"slot"`
	WindowEnd   time.Time     `json:"window_end"`
	Verdict     string        `json:"verdict"`
	Checks      []CheckRecord `json:"checks"`
	Critic      *CriticResult `json:"critic,omitempty"`
	Sessions    []string      `json:"sessions,omitempty"`
	RunID       string        `json:"run_id,omitempty"`
	Trigger     string        `json:"trigger"`
	Note        string        `json:"note,omitempty"`
	Fingerprint string        `json:"fingerprint,omitempty"`
}

type Correction struct {
	VerdictID string `json:"verdict_id"`
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason"`
	By        string `json:"by"`
}

type Counts struct {
	Pass         int `json:"pass"`
	Fail         int `json:"fail"`
	Missed       int `json:"missed"`
	Inconclusive int `json:"inconclusive"`
}

type CriticProblem struct {
	VerdictID string       `json:"verdict_id"`
	JobID     string       `json:"job_id"`
	SlotKey   string       `json:"slot_key"`
	Slot      time.Time    `json:"slot"`
	Critic    CriticResult `json:"critic"`
}

type Lag struct {
	Exporter    string     `json:"exporter"`
	Pending     int        `json:"pending"`
	Dropped     int        `json:"dropped,omitempty"`
	LastDropped string     `json:"last_dropped,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	FailedAt    *time.Time `json:"failed_at,omitempty"`
}

type OTLPSettings struct {
	Enabled  bool              `json:"enabled"`
	Endpoint string            `json:"endpoint"`
	Headers  map[string]string `json:"headers,omitempty"`
}

type LangfuseSettings struct {
	Enabled   bool   `json:"enabled"`
	Host      string `json:"host"`
	PublicKey string `json:"public_key"`
	SecretKey string `json:"secret_key"`
}

type NodloopSettings struct {
	Enabled bool `json:"enabled"`
}

type Settings struct {
	OTLP     *OTLPSettings     `json:"otlp,omitempty"`
	Langfuse *LangfuseSettings `json:"langfuse-score,omitempty"`
	Nodloop  *NodloopSettings  `json:"nodloop,omitempty"`
}

type Scheduler struct {
	Binary   string
	Home     string
	UserHome string
	GOOS     string
	UID      int
	Exec     func(string, ...string) error
}
