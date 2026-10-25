package judge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/check"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

const criticProtocol = "rindcut.critic/v1"

type criticInput struct {
	Protocol         string          `json:"protocol"`
	JobID            string          `json:"job_id"`
	JobRevision      int             `json:"job_revision"`
	Slot             string          `json:"slot"`
	CriteriaRevision string          `json:"criteria_revision"`
	Criteria         []job.Criterion `json:"criteria"`
}

type criticOutput struct {
	Protocol         string                     `json:"protocol"`
	JobID            string                     `json:"job_id"`
	JobRevision      int                        `json:"job_revision"`
	Slot             string                     `json:"slot"`
	CriteriaRevision string                     `json:"criteria_revision"`
	Complete         bool                       `json:"complete"`
	Criteria         []ledger.CriterionJudgment `json:"criteria"`
	Reason           string                     `json:"reason,omitempty"`
}

func (j Judge) evaluate(ctx context.Context, r job.Job, s job.Slot, checks []check.Result) *ledger.CriticResult {
	if r.Critic == nil {
		return nil
	}
	c := r.Critic
	out := &ledger.CriticResult{Protocol: criticProtocol, Source: c.Source, SourceRevision: c.SourceRevision,
		CriteriaRevision: c.CriteriaRevision, EvaluatedAt: j.Now().UTC(), Verdict: ledger.CriticInconclusive}
	for _, result := range checks {
		if result.Status != check.StatusPass {
			out.Cause = "checks_not_passed"
			return out
		}
	}
	env := j.slotEnv(r, s)
	env.Input, _ = json.Marshal(criticInput{Protocol: criticProtocol, JobID: r.ID, JobRevision: r.Revision,
		Slot: s.At.Format(time.RFC3339), CriteriaRevision: c.CriteriaRevision, Criteria: c.Criteria})
	result := check.Run(ctx, c.Command, env)
	if result.Truncated {
		out.Cause = "truncated_output"
		return out
	}
	text, captured := result.Observed["stdout"].(string)
	stdoutBytes, _ := result.Observed["stdout_bytes"].(int)
	if captured && len(text) == stdoutBytes {
		sum := sha256.Sum256([]byte(text))
		out.OutputSHA256 = hex.EncodeToString(sum[:])
	}
	if result.Status != check.StatusPass {
		out.Cause = "command_error"
		return out
	}
	if !captured || len(text) != stdoutBytes {
		out.Cause = "invalid_response"
		return out
	}
	response, cause := parseCritic(text, r, env.Slot)
	if cause != "" {
		out.Cause = cause
		return out
	}
	out.Criteria, out.Verdict = response.Criteria, criticVerdict(response.Criteria)
	if c.RetainReason {
		out.Reason = strings.ToValidUTF8(response.Reason[:min(len(response.Reason), 1024)], "")
	}
	return out
}

func parseCritic(text string, r job.Job, slot time.Time) (criticOutput, string) {
	var out criticOutput
	if !decodeCritic(text, &out) {
		return out, "invalid_response"
	}
	switch {
	case out.Protocol != criticProtocol:
		return out, "protocol_mismatch"
	case out.Slot != slot.Format(time.RFC3339):
		return out, "slot_mismatch"
	case out.CriteriaRevision != r.Critic.CriteriaRevision:
		return out, "criteria_revision_mismatch"
	case out.JobID != r.ID || out.JobRevision != r.Revision:
		return out, "job_revision_mismatch"
	case !out.Complete:
		return out, "incomplete_response"
	case !completeCriteria(out.Criteria, r.Critic.Criteria):
		return out, "criteria_mismatch"
	}
	return out, ""
}

func completeCriteria(results []ledger.CriterionJudgment, criteria []job.Criterion) bool {
	if len(results) != len(criteria) {
		return false
	}
	remaining := make(map[string]bool, len(criteria))
	for _, c := range criteria {
		remaining[c.ID] = true
	}
	for _, result := range results {
		if !remaining[result.ID] || !result.Verdict.Valid() {
			return false
		}
		delete(remaining, result.ID)
	}
	return len(remaining) == 0
}

func criticVerdict(results []ledger.CriterionJudgment) ledger.CriticVerdict {
	verdict := ledger.CriticApproved
	for _, result := range results {
		switch result.Verdict {
		case ledger.CriticInconclusive:
			return ledger.CriticInconclusive
		case ledger.CriticRejected:
			verdict = ledger.CriticRejected
		case ledger.CriticRevision:
			if verdict == ledger.CriticApproved {
				verdict = ledger.CriticRevision
			}
		}
	}
	return verdict
}
