package ledger

import (
	"sort"
	"time"
)

type Counts struct {
	Pass         int `json:"pass"`
	Fail         int `json:"fail"`
	Missed       int `json:"missed"`
	Inconclusive int `json:"inconclusive"`
}

func (c *Counts) add(v Verdict) {
	switch v {
	case VerdictPass:
		c.Pass++
	case VerdictFail:
		c.Fail++
	case VerdictMissed:
		c.Missed++
	case VerdictInconclusive:
		c.Inconclusive++
	}
}

// Problem is a slot that didn't pass, with the reason a person reads first
type Problem struct {
	VerdictID string    `json:"verdict_id"`
	JobID     string    `json:"job_id"`
	SlotKey   string    `json:"slot_key"`
	Slot      time.Time `json:"slot"`
	Verdict   Verdict   `json:"verdict"`
	Reason    string    `json:"reason"`
	Corrected bool      `json:"corrected,omitempty"`
}

// Report sums the verdicts of slots in [Since, Until)
// Corrections apply, so a verdict a person fixed counts as fixed
type Report struct {
	Since    time.Time         `json:"since"`
	Until    time.Time         `json:"until"`
	Totals   Counts            `json:"totals"`
	Jobs     map[string]Counts `json:"jobs"`
	Problems []Problem         `json:"problems"`
}

// Corrections maps each corrected verdict id to the correction in force, the latest one
func Corrections(entries []Entry) map[string]Correction {
	out := map[string]Correction{}
	for _, e := range entries {
		var c Correction
		if e.Kind == KindCorrection && e.Decode(&c) == nil {
			out[c.VerdictID] = c
		}
	}
	return out
}

func Summary(entries []Entry, since, until time.Time) Report {
	corrected := Corrections(entries)
	r := Report{Since: since, Until: until, Jobs: map[string]Counts{}, Problems: []Problem{}}
	for _, e := range entries {
		var j Judgment
		if e.Kind != KindVerdict || e.Decode(&j) != nil || j.Slot.Before(since) || !j.Slot.Before(until) {
			continue
		}
		v, reason := j.Verdict, firstProblem(j)
		c, fixed := corrected[e.ID]
		if fixed {
			v, reason = c.Verdict, c.Reason
		}
		r.Totals.add(v)
		jc := r.Jobs[j.JobID]
		jc.add(v)
		r.Jobs[j.JobID] = jc
		if v != VerdictPass {
			r.Problems = append(r.Problems, Problem{
				VerdictID: e.ID, JobID: j.JobID, SlotKey: j.SlotKey, Slot: j.Slot, Verdict: v, Reason: reason, Corrected: fixed,
			})
		}
	}
	sort.SliceStable(r.Problems, func(a, b int) bool { return r.Problems[a].Slot.Before(r.Problems[b].Slot) })
	return r
}

// The message of the first check that didn't pass, else the verdict's note
func firstProblem(j Judgment) string {
	for _, c := range j.Checks {
		if c.Status != CheckStatusPass {
			return c.Message
		}
	}
	return j.Note
}
