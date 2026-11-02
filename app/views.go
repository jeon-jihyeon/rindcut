package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/export"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/judge"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

// Quiet days still produce a report so missing delivery can reveal a stopped reporter
type Report struct {
	Since          time.Time         `json:"since"`
	Until          time.Time         `json:"until"`
	Totals         Counts            `json:"totals"`
	Jobs           map[string]Counts `json:"jobs"`
	CriticProblems []CriticProblem   `json:"critic_problems"`
	// Problems replaces the ledger's list so a missed slot can carry the session that started late
	Problems []Problem         `json:"problems"`
	Names    map[string]string `json:"names"`
	// Closed slots remain unjudged after two ticks without a verdict
	Unjudged map[string]Gap `json:"unjudged"`
	// Unjudged jobs can reveal a stopped tick
	Silent []string `json:"silent"`
	Lag    []Lag    `json:"lag"`
}

type Problem struct {
	VerdictID string     `json:"verdict_id"`
	JobID     string     `json:"job_id"`
	SlotKey   string     `json:"slot_key"`
	Slot      time.Time  `json:"slot"`
	Verdict   string     `json:"verdict"`
	Reason    string     `json:"reason"`
	Corrected bool       `json:"corrected,omitempty"`
	LateStart *LateStart `json:"late_start,omitempty"`
}

// LateStart is a session that ran after the slot's window closed and before the next slot
// It is shown next to the missed verdict and never changes it
type LateStart struct {
	SessionID string    `json:"session_id"`
	StartedAt time.Time `json:"started_at"`
	Grace     Duration  `json:"grace"`
}

// Gap counts the closed slots of one job that never got a verdict
type Gap struct {
	Overdue int `json:"overdue"` // still eligible for judgment
	Expired int `json:"expired"` // beyond the lookback and never judged
}

// A broken exporter setting must not prevent the summary
// Reporting reads the ledger and cursors without constructing exporters
func (a App) Report(ctx context.Context, since time.Duration) (Report, error) {
	entries, err := a.ledger().All(ctx)
	if err != nil {
		return Report{}, err
	}
	now := a.Now()
	from := now.Add(-since)
	revs := revisions(entries)
	summary := ledger.Summary(entries, from, now)
	r := Report{Since: summary.Since, Until: summary.Until, Totals: Counts(summary.Totals),
		Jobs: map[string]Counts{}, CriticProblems: []CriticProblem{},
		Problems: lateStarts(summary.Problems, revs, sessionsSince(entries, from)), Names: names(entries),
		Unjudged: unjudged(entries, revs, from, now), Silent: []string{}, Lag: []Lag{}}
	for id, counts := range summary.Jobs {
		r.Jobs[id] = Counts(counts)
	}
	for _, p := range summary.CriticProblems {
		r.CriticProblems = append(r.CriticProblems, CriticProblem{VerdictID: p.VerdictID,
			JobID: p.JobID, SlotKey: p.SlotKey, Slot: p.Slot, Critic: criticView(p.Critic)})
	}
	for id := range r.Unjudged {
		r.Silent = append(r.Silent, id)
	}
	sort.Strings(r.Silent)
	if s, err := a.settings(); err == nil {
		r.Lag = lagViews(export.Lags(a.cursorDir(), s.Enabled(a.UserHome, a.SearchPath), entries))
	} else {
		r.Lag = append(r.Lag, Lag{Exporter: "settings", LastError: err.Error()})
	}
	return r, nil
}

// unjudged counts each closed slot in the period that has no verdict
// A slot gets its verdict at the first tick after its window
// Only a slot two ticks past that counts
func unjudged(entries []ledger.Entry, revs []job.Job, from, now time.Time) map[string]Gap {
	judged := map[string]bool{}
	for _, e := range entries {
		var v ledger.Judgment
		if e.Kind == ledger.KindVerdict && e.Decode(&v) == nil {
			judged[v.SlotKey] = true
		}
	}
	settled, earliest := now.Add(-2*tickInterval), now.Add(-judge.Lookback)
	out := map[string]Gap{}
	for _, s := range judge.Scheduled(revs, from, now) {
		if s.End.After(settled) || judged[s.Key()] {
			continue
		}
		g := out[s.JobID]
		if s.At.Before(earliest) {
			g.Expired++
		} else {
			g.Overdue++
		}
		out[s.JobID] = g
	}
	return out
}

func lateStarts(problems []ledger.Problem, revs []job.Job, sessions []ledger.Session) []Problem {
	out := make([]Problem, len(problems))
	for i, p := range problems {
		out[i] = Problem{VerdictID: p.VerdictID, JobID: p.JobID, SlotKey: p.SlotKey, Slot: p.Slot,
			Verdict: string(p.Verdict), Reason: p.Reason, Corrected: p.Corrected}
		r, ok := job.At(revs, p.JobID, p.Slot)
		if p.Verdict != ledger.VerdictMissed || !ok || r.Evidence != job.EvidenceSession {
			continue
		}
		slot := job.Slot{JobID: p.JobID, At: p.Slot, End: p.Slot.Add(time.Duration(r.Grace))}
		if sess, late := judge.Late(r, slot, sessions); late {
			out[i].LateStart = &LateStart{SessionID: sess.SessionID, StartedAt: sess.StartedAt, Grace: Duration(r.Grace)}
		}
	}
	return out
}

// A late start may cite only sessions that began in the report period
func sessionsSince(entries []ledger.Entry, from time.Time) []ledger.Session {
	var out []ledger.Session
	for _, e := range entries {
		var s ledger.Session
		if e.Kind == ledger.KindSession && e.Decode(&s) == nil && s.StartedAt.After(from) {
			out = append(out, s)
		}
	}
	return out
}

type VerdictView struct {
	ID         string      `json:"id"`
	At         time.Time   `json:"at"`
	Judgment   Judgment    `json:"judgment"`
	Correction *Correction `json:"correction,omitempty"`
}

type VerdictFilter struct {
	JobID   string
	Since   time.Time
	Verdict string
	Limit   int
}

func (a App) Verdicts(ctx context.Context, f VerdictFilter) ([]VerdictView, error) {
	entries, err := a.ledger().All(ctx)
	if err != nil {
		return nil, err
	}
	fixes := ledger.Corrections(entries)
	out := []VerdictView{}
	for i := len(entries) - 1; i >= 0 && (f.Limit == 0 || len(out) < f.Limit); i-- {
		e := entries[i]
		var j ledger.Judgment
		if e.Kind != ledger.KindVerdict || e.Decode(&j) != nil || j.Slot.Before(f.Since) || (f.JobID != "" && j.JobID != f.JobID) {
			continue
		}
		v := VerdictView{ID: e.ID, At: e.At, Judgment: judgmentView(j)}
		effective := j.Verdict
		if c, ok := fixes[e.ID]; ok {
			view := Correction{VerdictID: c.VerdictID, Verdict: string(c.Verdict), Reason: c.Reason, By: c.By}
			v.Correction, effective = &view, c.Verdict
		}
		if f.Verdict == "" || string(effective) == f.Verdict {
			out = append(out, v)
		}
	}
	return out, nil
}

// Corrections preserve the original verdict
func (a App) Correct(ctx context.Context, in Correction) (Entry, error) {
	c := ledger.Correction{VerdictID: in.VerdictID, Verdict: ledger.Verdict(in.Verdict), Reason: in.Reason, By: in.By}
	if !c.Verdict.Valid() {
		return Entry{}, fmt.Errorf("verdict %q must be pass, fail, missed or inconclusive", c.Verdict)
	}
	if c.Reason == "" || c.By == "" {
		return Entry{}, errors.New("a correction needs the reason and who gave it")
	}
	entries, err := a.ledger().All(ctx)
	if err != nil {
		return Entry{}, err
	}
	for _, e := range entries {
		if e.ID == c.VerdictID && e.Kind == ledger.KindVerdict {
			entry, err := a.ledger().Append(ctx, ledger.KindCorrection, c)
			return entryView(entry), err
		}
	}
	return Entry{}, fmt.Errorf("no verdict %s", c.VerdictID)
}

func (a App) settings() (export.Settings, error) {
	s, err := export.LoadSettings(a.settingsPath())
	if err != nil {
		return s, fmt.Errorf("exporters.json: %w", err)
	}
	return s, nil
}

type ExporterResult struct {
	Settings Settings `json:"settings"`
	// The test result can report a disabled exporter or a failed send
	Test string `json:"test"`
}

// SetExporter replaces one exporter's settings and tests them with one send right away
// Failed test sends preserve the settings for an incremental correction
func (a App) SetExporter(ctx context.Context, name string, raw json.RawMessage) (ExporterResult, error) {
	s, err := a.settings()
	if err != nil {
		return ExporterResult{}, err
	}
	if err := s.Replace(name, raw); err != nil {
		return ExporterResult{}, err
	}
	if err := export.SaveSettings(a.settingsPath(), s); err != nil {
		return ExporterResult{}, err
	}
	out := ExporterResult{Settings: settingsView(s.Masked()), Test: "off"}
	built, err := s.Build(ctx, nil, nil, a.Version, a.UserHome, a.SearchPath, a.cursorDir())
	if err != nil {
		out.Test = err.Error()
		return out, nil
	}
	for _, ex := range built {
		if t, ok := ex.(interface{ Test(context.Context) error }); ok && ex.Name() == name {
			out.Test = "ok"
			if err := t.Test(ctx); err != nil {
				out.Test = err.Error()
			}
		}
	}
	shutdown(ctx, built)
	return out, nil
}

// Only session job folders may leave a trace
// Backfill may complete an unreadable transcript without guessing its contents
func (a App) RecordSession(ctx context.Context, in Session) (bool, error) {
	hook := ledger.Session{SessionID: in.SessionID, RunID: in.RunID, Cwd: in.Cwd, StartedAt: in.StartedAt,
		EndedAt: in.EndedAt, FirstPrompt: in.FirstPrompt, TranscriptPath: in.TranscriptPath,
		EndReason: in.EndReason, Source: ledger.Source(in.Source)}
	if hook.SessionID == "" {
		return false, errors.New("hook input has no session_id")
	}
	entries, err := a.ledger().All(ctx)
	if err != nil {
		return false, err
	}
	s := hook
	if t, err := judge.ReadTranscript(hook.TranscriptPath); err == nil {
		s.FirstPrompt, s.StartedAt = t.FirstPrompt, t.StartedAt
		if s.Cwd == "" {
			s.Cwd = t.Cwd
		}
	}
	if !judge.InFolders(s.Cwd, judge.SessionFolders(revisions(entries))) {
		return false, nil
	}
	s.EndedAt, s.Source = a.Now().UTC(), ledger.SourceHook
	_, err = a.ledger().Append(ctx, ledger.KindSession, s)
	return err == nil, err
}

type Status struct {
	Version       string         `json:"version"`
	Home          string         `json:"home"`
	TickInstalled bool           `json:"tick_installed"`
	LastTick      *time.Time     `json:"last_tick,omitempty"`
	Jobs          int            `json:"jobs"`
	Schedule      ScheduleStatus `json:"schedule"`
}

func (a App) Status(ctx context.Context) (Status, error) {
	entries, err := a.ledger().All(ctx)
	if err != nil {
		return Status{}, err
	}
	st := Status{Version: a.Version, Home: string(a.Home), TickInstalled: a.scheduler().Installed()}
	schedule := a.scheduler().Inspect(ctx)
	st.Schedule = ScheduleStatus{State: schedule.State, Reason: schedule.Reason}
	for _, j := range job.Current(revisions(entries)) {
		if !j.Removed {
			st.Jobs++
		}
	}
	if b, err := os.ReadFile(a.homePath("last-tick")); err == nil {
		if at, err := time.Parse(time.RFC3339, string(b)); err == nil {
			st.LastTick = &at
		}
	}
	return st, nil
}
