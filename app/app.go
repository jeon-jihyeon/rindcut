// Package app provides the use cases shared by the CLI and MCP tools
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/export"
	"github.com/jeon-jihyeon/rindcut/internal/home"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/judge"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
	"github.com/jeon-jihyeon/rindcut/internal/lock"
)

const (
	// Only a recently checked definition may be saved
	previewTTL   = 30 * time.Minute
	tickInterval = 5 * time.Minute
)

var (
	ErrNoPreview = errors.New("preview the job first and show the owner the result, then save the same job")
	ErrNoJob     = errors.New("no such job")
)

type App struct {
	Home     string
	UserHome string
	Now      func() time.Time
	Version  string
	// Schedule installation starts with the first saved job
	Installer Scheduler
	// Unreadable ledger lines remain visible to the owner
	Warn io.Writer
	// SearchPath is the PATH to find nodloop in
	SearchPath string
}

func (a App) ledger() *ledger.Ledger {
	return ledger.Open(a.homePath("ledger.jsonl"), a.lockPath("ledger"), a.Now, a.Version, func(line int, err error) {
		fmt.Fprintf(a.Warn, "rindcut: skipped ledger line %d: %v\n", line, err)
	})
}

func (a App) judge() judge.Judge {
	return judge.Judge{Ledger: a.ledger(), Now: a.Now, UserHome: a.UserHome, Dir: string(a.Home)}
}

func (a App) settingsPath() string { return a.homePath("exporters.json") }
func (a App) cursorDir() string    { return a.homePath("cursors") }

// Revisions reads every job revision with the time it was recorded
func revisions(entries []ledger.Entry) []job.Job {
	var out []job.Job
	for _, e := range entries {
		var j job.Job
		if e.Kind == ledger.KindJob && e.Decode(&j) == nil {
			j.Since = e.At
			out = append(out, j)
		}
	}
	return out
}

// Names maps every job id ever recorded to its latest name
func names(entries []ledger.Entry) map[string]string {
	out := map[string]string{}
	for id, j := range job.Current(revisions(entries)) {
		out[id] = j.Name
	}
	return out
}

// Revision numbers do not change the preview identity
func fingerprint(j job.Job) string {
	j.Revision, j.Since = 0, time.Time{}
	b, _ := json.Marshal(j)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

type Preview struct {
	Job       Job         `json:"job"`
	NextSlots []time.Time `json:"next_slots"`
	Probe     Judgment    `json:"probe"`
	ProbeID   string      `json:"probe_id"`
	// Checks without slot evidence cannot establish the result of one run
	Warnings []string `json:"warnings"`
}

// A job preview must not count as a slot verdict
func (a App) Preview(ctx context.Context, in Job) (Preview, error) {
	entries, err := a.ledger().All(ctx)
	if err != nil {
		return Preview{}, err
	}
	j := nextRevision(entries, in.model())
	if err := j.Validate(); err != nil {
		return Preview{}, err
	}
	j = a.judge().Pin(j)
	now := a.Now()
	slots, err := j.Slots(now, now.AddDate(0, 2, 0))
	if err != nil {
		return Preview{}, err
	}
	p := Preview{Job: jobView(j), NextSlots: []time.Time{}, Warnings: []string{}}
	for _, s := range slots[:min(3, len(slots))] {
		p.NextSlots = append(p.NextSlots, s.At)
	}
	for i, c := range j.Checks {
		if c.SlotFree() {
			p.Warnings = append(p.Warnings, fmt.Sprintf("check %d, %s, looks at nothing tied to the slot, so it gives the same answer every run", i+1, c.Kind))
		}
	}
	e, err := a.judge().Probe(ctx, j, now, fingerprint(j))
	if err != nil {
		return Preview{}, err
	}
	p.ProbeID = e.ID
	var probe ledger.Judgment
	if err := e.Decode(&probe); err != nil {
		return Preview{}, err
	}
	p.Probe = judgmentView(probe)
	return p, nil
}

type Saved struct {
	Job Job `json:"job"`
	// Installed is true when this save also scheduled the tick
	Installed bool `json:"installed"`
}

// Save records a job revision the owner previewed and keeps the tick scheduled
func (a App) Save(ctx context.Context, in Job) (Saved, error) {
	unlock, err := lock.Acquire(ctx, a.lockPath("jobs"), time.Second)
	if err != nil {
		return Saved{}, fmt.Errorf("job definition busy or unavailable, try save again: %w", err)
	}
	defer unlock()
	entries, err := a.ledger().All(ctx)
	if err != nil {
		return Saved{}, err
	}
	j := nextRevision(entries, in.model())
	if err := j.Validate(); err != nil {
		return Saved{}, err
	}
	j = a.judge().Pin(j)
	if !previewed(entries, fingerprint(j), a.Now()) {
		return Saved{}, ErrNoPreview
	}
	if _, err := a.ledger().Append(ctx, ledger.KindJob, j); err != nil {
		return Saved{}, err
	}
	out := Saved{Job: jobView(j)}
	if !a.scheduler().Installed() {
		if err := a.scheduler().Install(); err != nil {
			return out, fmt.Errorf("job saved, but the tick couldn't be scheduled, the next save tries again: %w", err)
		}
		out.Installed = true
	}
	return out, nil
}

// The recorded revision takes precedence over the number supplied by the caller
// Two saves cannot claim the same revision
func nextRevision(entries []ledger.Entry, j job.Job) job.Job {
	if j.ID == "" {
		j.ID = job.Slug(j.Name)
	}
	j.Revision, j.Removed = 1, false
	if cur, ok := job.Current(revisions(entries))[j.ID]; ok {
		j.Revision = cur.Revision + 1
	}
	return j
}

// previewed looks back through the last previewTTL of entries for a probe of the same definition
func previewed(entries []ledger.Entry, fingerprint string, now time.Time) bool {
	for i := len(entries) - 1; i >= 0 && now.Sub(entries[i].At) <= previewTTL; i-- {
		var p ledger.Judgment
		if entries[i].Kind == ledger.KindProbe && entries[i].Decode(&p) == nil && p.Fingerprint == fingerprint {
			return true
		}
	}
	return false
}

// Removing a job preserves its recorded history
func (a App) Remove(ctx context.Context, id string) (Job, error) {
	unlock, err := lock.Acquire(ctx, a.lockPath("jobs"), time.Second)
	if err != nil {
		return Job{}, fmt.Errorf("job definition busy or unavailable, try remove again: %w", err)
	}
	defer unlock()
	entries, err := a.ledger().All(ctx)
	if err != nil {
		return Job{}, err
	}
	cur, ok := job.Current(revisions(entries))[id]
	if !ok || cur.Removed {
		return Job{}, fmt.Errorf("%s: %w", id, ErrNoJob)
	}
	cur.Revision++
	cur.Removed = true
	_, err = a.ledger().Append(ctx, ledger.KindJob, cur)
	return jobView(cur), err
}

type JobView struct {
	Job
	LastVerdict *Judgment  `json:"last_verdict,omitempty"`
	NextSlot    *time.Time `json:"next_slot,omitempty"`
}

func (a App) Jobs(ctx context.Context, all bool) ([]JobView, error) {
	entries, err := a.ledger().All(ctx)
	if err != nil {
		return nil, err
	}
	last := map[string]ledger.Judgment{}
	for _, e := range entries {
		var v ledger.Judgment
		if e.Kind == ledger.KindVerdict && e.Decode(&v) == nil {
			last[v.JobID] = v
		}
	}
	now := a.Now()
	out := []JobView{}
	for _, j := range job.Current(revisions(entries)) {
		if j.Removed && !all {
			continue
		}
		v := JobView{Job: jobView(j)}
		if lv, ok := last[j.ID]; ok {
			view := judgmentView(lv)
			v.LastVerdict = &view
		}
		if slots, err := j.Slots(now, now.AddDate(0, 2, 0)); err == nil && len(slots) > 0 && !j.Removed {
			v.NextSlot = &slots[0].At
		}
		out = append(out, v)
	}
	sort.Slice(out, func(x, y int) bool { return out[x].ID < out[y].ID })
	return out, nil
}

type TickResult struct {
	// Busy means another tick held the lock and this one did nothing
	Busy        bool          `json:"busy"`
	Verdicts    []Entry       `json:"verdicts"`
	Backfilled  int           `json:"backfilled"`
	Lag         []Lag         `json:"lag"`
	Observation *ObserveCycle `json:"observation,omitempty"`
}

// The tick lock spans slot judgments and observation collection and verdict export
// A slow export cannot overlap the next tick and resend or rewind its cursor
func (a App) Tick(ctx context.Context) (TickResult, error) {
	unlock, err := lock.Acquire(ctx, a.lockPath("tick"), 0)
	if errors.Is(err, lock.ErrBusy) {
		return TickResult{Busy: true}, nil
	}
	if err != nil {
		return TickResult{}, err
	}
	defer unlock()
	r, err := a.judge().Tick(ctx)
	if err != nil {
		return TickResult{}, err
	}
	// Status must distinguish a quiet tick from a stopped one
	_ = os.WriteFile(a.homePath("last-tick"), []byte(a.Now().UTC().Format(time.RFC3339)), 0o600)
	observed, observeErr := a.observeSync(ctx)
	lags, exportErr := a.export(ctx, "")
	out := TickResult{Verdicts: entriesView(r.Verdicts), Backfilled: r.Backfilled, Lag: lagViews(lags)}
	if observed.ID != "" {
		out.Observation = &observed
	}
	return out, errors.Join(observeErr, exportErr)
}

// Export retries eligible scheduled records under the tick lock
func (a App) Export(ctx context.Context, name string, fromStart bool) ([]Lag, error) {
	unlock, err := lock.Acquire(ctx, a.lockPath("tick"), tickInterval)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if fromStart {
		s, err := a.settings()
		if err != nil {
			return nil, err
		}
		for _, n := range s.Enabled(a.UserHome, a.SearchPath) {
			if name != "" && n != name {
				continue
			}
			if err := export.Reset(a.cursorDir(), n); err != nil {
				return nil, err
			}
		}
	}
	lags, err := a.export(ctx, name)
	return lagViews(lags), err
}

// export runs under the tick lock
func (a App) export(ctx context.Context, name string) ([]export.Lag, error) {
	entries, err := a.ledger().All(ctx)
	if err != nil {
		return nil, err
	}
	s, err := a.settings()
	if err != nil {
		return nil, err
	}
	all, err := s.Build(ctx, entries, names(entries), a.Version, a.UserHome, a.SearchPath, a.cursorDir())
	if err != nil {
		return nil, err
	}
	var chosen []export.Exporter
	for _, ex := range all {
		if name == "" || ex.Name() == name {
			chosen = append(chosen, ex)
		}
	}
	lags := export.Runner{Exporters: chosen, CursorDir: a.cursorDir(), Now: a.Now}.Run(ctx, entries)
	shutdown(ctx, all)
	return lags, nil
}

// The MCP server outlives each call and must not retain idle exporter clients
func shutdown(ctx context.Context, exporters []export.Exporter) {
	for _, ex := range exporters {
		if c, ok := ex.(interface{ Shutdown(context.Context) error }); ok {
			_ = c.Shutdown(ctx)
		}
	}
}

// Ready creates the home before anything writes into it
func (a App) Ready() error {
	if err := home.Dir(a.Home).Ensure(); err != nil {
		return fmt.Errorf("can't create %s: %w", a.Home, err)
	}
	return nil
}
