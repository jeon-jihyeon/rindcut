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

// Report is the daily summary: verdict counts, what went wrong, which jobs went quiet and what exports owe
// A day where everything passed still has one, because a summary that stops arriving is the alarm
type Report struct {
	ledger.Report
	Names map[string]string `json:"names"`
	// Silent lists active jobs whose slot closed two ticks ago with no verdict, a sign the tick stopped
	Silent []string     `json:"silent"`
	Lag    []export.Lag `json:"lag"`
}

// Report reads the ledger and cursors only, so a broken exporter setting never stops the summary
func (a App) Report(ctx context.Context, since time.Duration) (Report, error) {
	entries, err := a.Ledger().All(ctx)
	if err != nil {
		return Report{}, err
	}
	now := a.Now()
	from := now.Add(-since)
	r := Report{Report: ledger.Summary(entries, from, now), Names: Names(entries), Silent: []string{}, Lag: []export.Lag{}}
	if s, err := a.Settings(); err == nil {
		r.Lag = export.Lags(a.cursorDir(), s.Enabled(a.UserHome, a.SearchPath), entries)
	} else {
		r.Lag = append(r.Lag, export.Lag{Exporter: "settings", LastError: err.Error()})
	}
	// A slot gets its verdict at the first tick after its window, so only a slot two ticks past that counts as silent
	settled := now.Add(-2 * tickInterval)
	for id, j := range job.Current(Revisions(entries)) {
		if j.Removed {
			continue
		}
		slots, err := j.Slots(laterOf(from, j.Since), settled)
		closed := err == nil && len(slots) > 0 && !slots[0].End.After(settled)
		if _, judged := r.Jobs[id]; closed && !judged {
			r.Silent = append(r.Silent, id)
		}
	}
	sort.Strings(r.Silent)
	return r, nil
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
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
	built, err := s.Build(ctx, nil, nil, a.Version, a.UserHome, a.SearchPath)
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
	if t, err := judge.ReadSession(hook.TranscriptPath); err == nil {
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
