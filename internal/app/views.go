package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/check"
	"github.com/jeon-jihyeon/rindcut/internal/export"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/judge"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

// Report is the daily summary: verdict counts, what went wrong, which jobs went quiet and what exports owe
// A day where everything passed still has one, because a summary that stops arriving is the alarm
type Report struct {
	ledger.Report
	// Problems replaces the ledger's list so a missed slot can carry the session that started late
	Problems []Problem         `json:"problems"`
	Names    map[string]string `json:"names"`
	// Unjudged counts, per job id, closed slots in the period still without a verdict two ticks later
	Unjudged map[string]Gap `json:"unjudged"`
	// Silent lists the job ids in Unjudged, a sign the tick stopped
	Silent []string     `json:"silent"`
	Lag    []export.Lag `json:"lag"`
}

// Problem is a ledger problem plus, for a missed session job, the late session that may explain it
type Problem struct {
	ledger.Problem
	LateStart *LateStart `json:"late_start,omitempty"`
}

// LateStart is a session that ran after the slot's window closed and before the next slot
// It is shown next to the missed verdict and never changes it
type LateStart struct {
	SessionID string         `json:"session_id"`
	StartedAt time.Time      `json:"started_at"`
	Grace     check.Duration `json:"grace"`
}

// Gap counts the closed slots of one job that never got a verdict
type Gap struct {
	Overdue int `json:"overdue"` // still eligible for judgment
	Expired int `json:"expired"` // beyond the lookback and never judged
}

// Report reads the ledger and cursors only, so a broken exporter setting never stops the summary
func (a App) Report(ctx context.Context, since time.Duration) (Report, error) {
	entries, err := a.Ledger().All(ctx)
	if err != nil {
		return Report{}, err
	}
	now := a.Now()
	from := now.Add(-since)
	revs := Revisions(entries)
	summary := ledger.Summary(entries, from, now)
	r := Report{Report: summary, Problems: lateStarts(summary.Problems, revs, sessionsSince(entries, from)), Names: Names(entries),
		Unjudged: unjudged(entries, revs, from, now), Silent: []string{}, Lag: []export.Lag{}}
	for id := range r.Unjudged {
		r.Silent = append(r.Silent, id)
	}
	sort.Strings(r.Silent)
	if s, err := a.Settings(); err == nil {
		r.Lag = export.Lags(a.cursorDir(), s.Enabled(a.UserHome, a.SearchPath), entries)
	} else {
		r.Lag = append(r.Lag, export.Lag{Exporter: "settings", LastError: err.Error()})
	}
	return r, nil
}

// unjudged counts each closed slot in the period that has no verdict
// A slot gets its verdict at the first tick after its window, so only a slot two ticks past that counts
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
		out[i] = Problem{Problem: p}
		r, ok := job.At(revs, p.JobID, p.Slot)
		if p.Verdict != ledger.VerdictMissed || !ok || r.Evidence != job.EvidenceSession {
			continue
		}
		slot := job.Slot{JobID: p.JobID, At: p.Slot, End: p.Slot.Add(time.Duration(r.Grace))}
		if sess, late := judge.Late(r, slot, sessions); late {
			out[i].LateStart = &LateStart{SessionID: sess.SessionID, StartedAt: sess.StartedAt, Grace: r.Grace}
		}
	}
	return out
}

// sessionsSince lists sessions that started in the report period, the only ones a late start may cite
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
	ID         string             `json:"id"`
	At         time.Time          `json:"at"`
	Judgment   ledger.Judgment    `json:"judgment"`
	Correction *ledger.Correction `json:"correction,omitempty"`
}

type VerdictFilter struct {
	JobID   string
	Since   time.Time
	Verdict ledger.Verdict
	Limit   int
}

// Verdicts lists verdicts newest first with the correction in force, the full evidence included
func (a App) Verdicts(ctx context.Context, f VerdictFilter) ([]VerdictView, error) {
	entries, err := a.Ledger().All(ctx)
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
		v := VerdictView{ID: e.ID, At: e.At, Judgment: j}
		effective := j.Verdict
		if c, ok := fixes[e.ID]; ok {
			v.Correction, effective = &c, c.Verdict
		}
		if f.Verdict == "" || effective == f.Verdict {
			out = append(out, v)
		}
	}
	return out, nil
}

// Correct overrides a verdict with a person's reason, leaving the original in place
func (a App) Correct(ctx context.Context, c ledger.Correction) (ledger.Entry, error) {
	if !c.Verdict.Valid() {
		return ledger.Entry{}, fmt.Errorf("verdict %q must be pass, fail, missed or inconclusive", c.Verdict)
	}
	if c.Reason == "" || c.By == "" {
		return ledger.Entry{}, errors.New("a correction needs the reason and who gave it")
	}
	entries, err := a.Ledger().All(ctx)
	if err != nil {
		return ledger.Entry{}, err
	}
	for _, e := range entries {
		if e.ID == c.VerdictID && e.Kind == ledger.KindVerdict {
			return a.Ledger().Append(ctx, ledger.KindCorrection, c)
		}
	}
	return ledger.Entry{}, fmt.Errorf("no verdict %s", c.VerdictID)
}

func (a App) Settings() (export.Settings, error) {
	s, err := export.LoadSettings(a.settingsPath())
	if err != nil {
		return s, fmt.Errorf("exporters.json: %w", err)
	}
	return s, nil
}

type ExporterResult struct {
	Settings export.Settings `json:"settings"`
	// Test is "ok", "off" for a disabled exporter, or what went wrong on a test send
	Test string `json:"test"`
}

// SetExporter replaces one exporter's settings and tests them with one send right away
// The settings are kept even when the test fails, so the owner can fix a key without retyping the rest
func (a App) SetExporter(ctx context.Context, name string, raw json.RawMessage) (ExporterResult, error) {
	s, err := a.Settings()
	if err != nil {
		return ExporterResult{}, err
	}
	if err := s.Replace(name, raw); err != nil {
		return ExporterResult{}, err
	}
	if err := export.SaveSettings(a.settingsPath(), s); err != nil {
		return ExporterResult{}, err
	}
	out := ExporterResult{Settings: s.Masked(), Test: "off"}
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

// RecordSession appends what the SessionEnd hook saw, filled from the transcript head
// Only sessions in a session job's folder are kept, so the owner's other work leaves no trace
// An unreadable transcript is recorded as it is and completed by the tick's backfill, never filled with guesses
func (a App) RecordSession(ctx context.Context, hook ledger.Session) (bool, error) {
	if hook.SessionID == "" {
		return false, errors.New("hook input has no session_id")
	}
	entries, err := a.Ledger().All(ctx)
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
	if !judge.InFolders(s.Cwd, judge.SessionFolders(Revisions(entries))) {
		return false, nil
	}
	s.EndedAt, s.Source = a.Now().UTC(), ledger.SourceHook
	_, err = a.Ledger().Append(ctx, ledger.KindSession, s)
	return err == nil, err
}

type Status struct {
	Version       string     `json:"version"`
	Home          string     `json:"home"`
	TickInstalled bool       `json:"tick_installed"`
	LastTick      *time.Time `json:"last_tick,omitempty"`
	Jobs          int        `json:"jobs"`
}

func (a App) Status(ctx context.Context) (Status, error) {
	entries, err := a.Ledger().All(ctx)
	if err != nil {
		return Status{}, err
	}
	st := Status{Version: a.Version, Home: string(a.Home), TickInstalled: a.Installer.Installed()}
	for _, j := range job.Current(Revisions(entries)) {
		if !j.Removed {
			st.Jobs++
		}
	}
	if b, err := os.ReadFile(a.Home.Path("last-tick")); err == nil {
		if at, err := time.Parse(time.RFC3339, string(b)); err == nil {
			st.LastTick = &at
		}
	}
	return st, nil
}
