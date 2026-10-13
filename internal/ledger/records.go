package ledger

import "time"

// Verdict is what rindcut concluded about one slot
type Verdict string

const (
	VerdictPass         Verdict = "pass"
	VerdictFail         Verdict = "fail"
	VerdictMissed       Verdict = "missed"       // the job expected a session and none ran
	VerdictInconclusive Verdict = "inconclusive" // a check couldn't read the state
)

func (v Verdict) Valid() bool {
	switch v {
	case VerdictPass, VerdictFail, VerdictMissed, VerdictInconclusive:
		return true
	}
	return false
}

// Session is a local Claude session as the plugin hook or the transcript backfill saw it
type Session struct {
	SessionID      string    `json:"session_id"`
	Cwd            string    `json:"cwd"`
	StartedAt      time.Time `json:"started_at"`
	EndedAt        time.Time `json:"ended_at"`
	FirstPrompt    string    `json:"first_prompt"`
	TranscriptPath string    `json:"transcript_path"`
	EndReason      string    `json:"end_reason,omitempty"`
	Source         Source    `json:"source"`
}

// Source says who recorded a session
type Source string

const (
	SourceHook     Source = "hook"     // the plugin's SessionEnd hook
	SourceBackfill Source = "backfill" // the tick, reading a transcript the hook missed
)

// Complete is true when the record carries what matching needs
// The hook can fire before a transcript is readable, and a later backfill fills the gap
func (s Session) Complete() bool { return s.FirstPrompt != "" && !s.StartedAt.IsZero() }

// CheckStatusPass is the stored status of a passing check, the same word the check package uses
const CheckStatusPass = "pass"

// CheckRecord is a check result as stored, the same shape the check package produces
type CheckRecord struct {
	Kind      string         `json:"kind"`
	Status    string         `json:"status"`
	Message   string         `json:"message"`
	Observed  map[string]any `json:"observed"`
	Truncated bool           `json:"truncated,omitempty"`
}

// Judgment is the payload of a verdict, and of a probe, which is a verdict that doesn't count
type Judgment struct {
	JobID       string        `json:"job_id"`
	JobRevision int           `json:"job_revision"`
	SlotKey     string        `json:"slot_key"`
	Slot        time.Time     `json:"slot"`
	WindowEnd   time.Time     `json:"window_end"`
	Verdict     Verdict       `json:"verdict"`
	Checks      []CheckRecord `json:"checks"`
	Sessions    []string      `json:"sessions,omitempty"`
	Trigger     string        `json:"trigger"`
	Note        string        `json:"note,omitempty"`
	// Fingerprint names the job definition a probe checked, so a save can prove the owner saw it
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Correction overrides a verdict without touching it
// It always points at the original verdict, and the latest correction wins
type Correction struct {
	VerdictID string  `json:"verdict_id"`
	Verdict   Verdict `json:"verdict"`
	Reason    string  `json:"reason"`
	By        string  `json:"by"`
}
