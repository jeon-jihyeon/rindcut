package ledger

import "time"

type CriticVerdict string

const (
	CriticApproved     CriticVerdict = "approved"
	CriticRevision     CriticVerdict = "revision"
	CriticRejected     CriticVerdict = "rejected"
	CriticInconclusive CriticVerdict = "inconclusive"
)

func (v CriticVerdict) Valid() bool {
	switch v {
	case CriticApproved, CriticRevision, CriticRejected, CriticInconclusive:
		return true
	}
	return false
}

type CriterionJudgment struct {
	ID      string        `json:"id"`
	Verdict CriticVerdict `json:"verdict"`
}

type CriticResult struct {
	Protocol         string              `json:"protocol"`
	Source           string              `json:"source"`
	SourceRevision   string              `json:"source_revision"`
	CriteriaRevision string              `json:"criteria_revision"`
	EvaluatedAt      time.Time           `json:"evaluated_at"`
	Verdict          CriticVerdict       `json:"verdict"`
	Criteria         []CriterionJudgment `json:"criteria,omitempty"`
	Cause            string              `json:"cause,omitempty"`
	OutputSHA256     string              `json:"output_sha256,omitempty"`
	Reason           string              `json:"reason,omitempty"`
}

type CriticProblem struct {
	VerdictID string       `json:"verdict_id"`
	JobID     string       `json:"job_id"`
	SlotKey   string       `json:"slot_key"`
	Slot      time.Time    `json:"slot"`
	Critic    CriticResult `json:"critic"`
}
