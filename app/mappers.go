package app

import (
	"maps"
	"slices"

	"github.com/jeon-jihyeon/rindcut/internal/check"
	"github.com/jeon-jihyeon/rindcut/internal/export"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

func (s CheckSpec) model() check.Spec {
	return check.Spec{Kind: check.Kind(s.Kind), Path: s.Path, MinBytes: s.MinBytes, Fresh: s.Fresh,
		URL: s.URL, Status: s.Status, Contains: s.Contains, Run: s.Run, Timeout: check.Duration(s.Timeout),
		Scripts: maps.Clone(s.Scripts)}
}

func checkView(s check.Spec) CheckSpec {
	return CheckSpec{Kind: string(s.Kind), Path: s.Path, MinBytes: s.MinBytes, Fresh: s.Fresh,
		URL: s.URL, Status: s.Status, Contains: s.Contains, Run: s.Run, Timeout: Duration(s.Timeout),
		Scripts: maps.Clone(s.Scripts)}
}

func (v Job) model() job.Job {
	j := job.Job{ID: v.ID, Name: v.Name, Schedule: v.Schedule, Timezone: v.Timezone,
		Grace: check.Duration(v.Grace), Evidence: job.Evidence(v.Evidence), Folder: v.Folder, Marker: v.Marker,
		Revision: v.Revision, Removed: v.Removed, Since: v.Since}
	if v.Checks != nil {
		j.Checks = make([]check.Spec, len(v.Checks))
		for i, c := range v.Checks {
			j.Checks[i] = c.model()
		}
	}
	if v.Critic != nil {
		c := v.Critic
		j.Critic = &job.Critic{Source: c.Source, SourceRevision: c.SourceRevision, CriteriaRevision: c.CriteriaRevision,
			Command: c.Command.model(), RetainReason: c.RetainReason}
		if c.Criteria != nil {
			j.Critic.Criteria = make([]job.Criterion, len(c.Criteria))
			for i, v := range c.Criteria {
				j.Critic.Criteria[i] = job.Criterion(v)
			}
		}
	}
	return j
}

func jobView(j job.Job) Job {
	v := Job{ID: j.ID, Name: j.Name, Schedule: j.Schedule, Timezone: j.Timezone,
		Grace: Duration(j.Grace), Evidence: string(j.Evidence), Folder: j.Folder, Marker: j.Marker,
		Revision: j.Revision, Removed: j.Removed, Since: j.Since}
	if j.Checks != nil {
		v.Checks = make([]CheckSpec, len(j.Checks))
		for i, c := range j.Checks {
			v.Checks[i] = checkView(c)
		}
	}
	if j.Critic != nil {
		c := j.Critic
		v.Critic = &Critic{Source: c.Source, SourceRevision: c.SourceRevision, CriteriaRevision: c.CriteriaRevision,
			Command: checkView(c.Command), RetainReason: c.RetainReason}
		if c.Criteria != nil {
			v.Critic.Criteria = make([]Criterion, len(c.Criteria))
			for i, c := range c.Criteria {
				v.Critic.Criteria[i] = Criterion(c)
			}
		}
	}
	return v
}

func entryView(e ledger.Entry) Entry {
	return Entry{ID: e.ID, Kind: string(e.Kind), At: e.At, Version: e.Version, Data: slices.Clone(e.Data)}
}

func entriesView(entries []ledger.Entry) []Entry {
	if entries == nil {
		return nil
	}
	out := make([]Entry, len(entries))
	for i, e := range entries {
		out[i] = entryView(e)
	}
	return out
}

func judgmentView(j ledger.Judgment) Judgment {
	v := Judgment{JobID: j.JobID, JobRevision: j.JobRevision, SlotKey: j.SlotKey, Slot: j.Slot,
		WindowEnd: j.WindowEnd, Verdict: string(j.Verdict), Sessions: slices.Clone(j.Sessions),
		RunID: j.RunID, Trigger: j.Trigger, Note: j.Note, Fingerprint: j.Fingerprint}
	if j.Checks != nil {
		v.Checks = make([]CheckRecord, len(j.Checks))
		for i, c := range j.Checks {
			v.Checks[i] = CheckRecord(c)
		}
	}
	if j.Critic != nil {
		c := criticView(*j.Critic)
		v.Critic = &c
	}
	return v
}

func criticView(c ledger.CriticResult) CriticResult {
	v := CriticResult{Protocol: c.Protocol, Source: c.Source, SourceRevision: c.SourceRevision,
		CriteriaRevision: c.CriteriaRevision, EvaluatedAt: c.EvaluatedAt, Verdict: string(c.Verdict),
		Cause: c.Cause, OutputSHA256: c.OutputSHA256, Reason: c.Reason}
	if c.Criteria != nil {
		v.Criteria = make([]CriterionJudgment, len(c.Criteria))
		for i, c := range c.Criteria {
			v.Criteria[i] = CriterionJudgment{ID: c.ID, Verdict: string(c.Verdict)}
		}
	}
	return v
}

func lagViews(lags []export.Lag) []Lag {
	if lags == nil {
		return nil
	}
	out := make([]Lag, len(lags))
	for i, l := range lags {
		out[i] = Lag(l)
	}
	return out
}

func settingsView(s export.Settings) Settings {
	v := Settings{}
	if s.OTLP != nil {
		v.OTLP = &OTLPSettings{Enabled: s.OTLP.Enabled, Endpoint: s.OTLP.Endpoint, Headers: maps.Clone(s.OTLP.Headers)}
	}
	if s.Langfuse != nil {
		v.Langfuse = (*LangfuseSettings)(s.Langfuse)
	}
	if s.Nodloop != nil {
		v.Nodloop = (*NodloopSettings)(s.Nodloop)
	}
	return v
}
