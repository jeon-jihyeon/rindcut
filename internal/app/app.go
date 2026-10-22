// Package app wires job, judge, ledger and export into the use cases the CLI and the MCP tools share
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
	// A preview must be this fresh for a save, so the owner saves what they just saw checked
	previewTTL = 30 * time.Minute
	// The scheduler runs the tick this often
	tickInterval = 5 * time.Minute
)

var (
	ErrNoPreview = errors.New("preview the job first and show the owner the result, then save the same job")
	ErrNoJob     = errors.New("no such job")
)

type App struct {
	Home     home.Dir
	UserHome string
	Now      func() time.Time
	Version  string
	// Installer keeps the tick scheduled, installed on the first saved job
	Installer judge.Installer
	// Warn receives ledger lines that couldn't be read, so damage is reported and never dropped in silence
	Warn io.Writer
	// SearchPath is the PATH to find nodloop in
	SearchPath string
}

func (a App) Ledger() *ledger.Ledger {
	return ledger.Open(a.Home.Ledger(), a.Home.Lock("ledger"), a.Now, a.Version, func(line int, err error) {
		fmt.Fprintf(a.Warn, "rindcut: skipped ledger line %d: %v\n", line, err)
	})
}

func (a App) Judge() judge.Judge {
	return judge.Judge{Ledger: a.Ledger(), Now: a.Now, UserHome: a.UserHome, Dir: string(a.Home)}
}

func (a App) settingsPath() string { return a.Home.Path("exporters.json") }
func (a App) cursorDir() string    { return a.Home.Path("cursors") }

// Revisions reads every job revision with the time it was recorded
func Revisions(entries []ledger.Entry) []job.Job {
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
func Names(entries []ledger.Entry) map[string]string {
	out := map[string]string{}
	for id, j := range job.Current(Revisions(entries)) {
		out[id] = j.Name
	}
	return out
}

// Fingerprint identifies a job definition apart from its revision number, for matching a save to its preview
func Fingerprint(j job.Job) string {
	j.Revision, j.Since = 0, time.Time{}
	b, _ := json.Marshal(j)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

type Preview struct {
	Job       job.Job         `json:"job"`
	NextSlots []time.Time     `json:"next_slots"`
	Probe     ledger.Judgment `json:"probe"`
	ProbeID   string          `json:"probe_id"`
	// Warnings name the checks that give the same answer every slot, so their pass proves nothing about one run
	Warnings []string `json:"warnings"`
}

// Preview validates a job, lists its next slots and judges its latest slot without counting it
func (a App) Preview(ctx context.Context, j job.Job) (Preview, error) {
	entries, err := a.Ledger().All(ctx)
	if err != nil {
		return Preview{}, err
	}
	j = a.Judge().Pin(nextRevision(entries, j))
	if err := j.Validate(); err != nil {
		return Preview{}, err
	}
	now := a.Now()
	slots, err := j.Slots(now, now.AddDate(0, 2, 0))
	if err != nil {
		return Preview{}, err
	}
	p := Preview{Job: j, NextSlots: []time.Time{}, Warnings: []string{}}
	for _, s := range slots[:min(3, len(slots))] {
		p.NextSlots = append(p.NextSlots, s.At)
	}
	for i, c := range j.Checks {
		if c.SlotFree() {
			p.Warnings = append(p.Warnings, fmt.Sprintf("check %d, %s, looks at nothing tied to the slot, so it gives the same answer every run", i+1, c.Kind))
		}
	}
	e, err := a.Judge().Probe(ctx, j, now, Fingerprint(j))
	if err != nil {
		return Preview{}, err
	}
	p.ProbeID = e.ID
	return p, e.Decode(&p.Probe)
}

type Saved struct {
	Job job.Job `json:"job"`
	// Installed is true when this save also scheduled the tick
	Installed bool `json:"installed"`
}

// Save records a job revision the owner previewed and keeps the tick scheduled
func (a App) Save(ctx context.Context, j job.Job) (Saved, error) {
	entries, err := a.Ledger().All(ctx)
	if err != nil {
		return Saved{}, err
	}
	j = a.Judge().Pin(nextRevision(entries, j))
	if err := j.Validate(); err != nil {
		return Saved{}, err
	}
	if !previewed(entries, Fingerprint(j), a.Now()) {
		return Saved{}, ErrNoPreview
	}
	if _, err := a.Ledger().Append(ctx, ledger.KindJob, j); err != nil {
		return Saved{}, err
	}
	out := Saved{Job: j}
	if !a.Installer.Installed() {
		if err := a.Installer.Install(); err != nil {
			return out, fmt.Errorf("job saved, but the tick couldn't be scheduled, the next save tries again: %w", err)
		}
		out.Installed = true
	}
	return out, nil
}

// nextRevision numbers a job after the one recorded under its id, naming it from its name when it has no id
// The caller's revision number is ignored, so two saves can't claim the same revision
func nextRevision(entries []ledger.Entry, j job.Job) job.Job {
	if j.ID == "" {
		j.ID = job.Slug(j.Name)
	}
	j.Revision, j.Removed = 1, false
	if cur, ok := job.Current(Revisions(entries))[j.ID]; ok {
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

// Remove writes a removing revision, keeping the job's history
func (a App) Remove(ctx context.Context, id string) (job.Job, error) {
	entries, err := a.Ledger().All(ctx)
	if err != nil {
		return job.Job{}, err
	}
	cur, ok := job.Current(Revisions(entries))[id]
	if !ok || cur.Removed {
		return job.Job{}, fmt.Errorf("%s: %w", id, ErrNoJob)
	}
	cur.Revision++
	cur.Removed = true
	_, err = a.Ledger().Append(ctx, ledger.KindJob, cur)
	return cur, err
}

type JobView struct {
	job.Job
	LastVerdict *ledger.Judgment `json:"last_verdict,omitempty"`
	NextSlot    *time.Time       `json:"next_slot,omitempty"`
}

func (a App) Jobs(ctx context.Context, all bool) ([]JobView, error) {
	entries, err := a.Ledger().All(ctx)
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
	for _, j := range job.Current(Revisions(entries)) {
		if j.Removed && !all {
			continue
		}
		v := JobView{Job: j}
		if lv, ok := last[j.ID]; ok {
			v.LastVerdict = &lv
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
	Busy bool `json:"busy"`
	judge.TickResult
	Lag []export.Lag `json:"lag"`
}

// Tick judges closed slots, then sends everything new to the exporters
// The lock spans both, so a slow export can't overlap the next tick's export and resend or rewind a cursor
func (a App) Tick(ctx context.Context) (TickResult, error) {
	unlock, err := lock.Acquire(ctx, a.Home.Lock("tick"), 0)
	if errors.Is(err, lock.ErrBusy) {
		return TickResult{Busy: true}, nil
	}
	if err != nil {
		return TickResult{}, err
	}
	defer unlock()
	r, err := a.Judge().Tick(ctx)
	if err != nil {
		return TickResult{}, err
	}
	// Status reads this to tell a running tick from a stopped one, even on days with no slot
	_ = os.WriteFile(a.Home.Path("last-tick"), []byte(a.Now().UTC().Format(time.RFC3339)), 0o600)
	lags, err := a.export(ctx, "")
	return TickResult{TickResult: r, Lag: lags}, err
}

// Export runs the exporters, one when name is set, and resends everything with fromStart
func (a App) Export(ctx context.Context, name string, fromStart bool) ([]export.Lag, error) {
	unlock, err := lock.Acquire(ctx, a.Home.Lock("tick"), tickInterval)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if fromStart {
		s, err := a.Settings()
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
	return a.export(ctx, name)
}

// export runs under the tick lock
func (a App) export(ctx context.Context, name string) ([]export.Lag, error) {
	entries, err := a.Ledger().All(ctx)
	if err != nil {
		return nil, err
	}
	s, err := a.Settings()
	if err != nil {
		return nil, err
	}
	all, err := s.Build(ctx, entries, Names(entries), a.Version, a.UserHome, a.SearchPath, a.cursorDir())
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

// shutdown releases what an exporter holds, like the OTLP HTTP client, since the MCP server outlives each call
func shutdown(ctx context.Context, exporters []export.Exporter) {
	for _, ex := range exporters {
		if c, ok := ex.(interface{ Shutdown(context.Context) error }); ok {
			_ = c.Shutdown(ctx)
		}
	}
}

// Ready creates the home before anything writes into it
func (a App) Ready() error {
	if err := a.Home.Ensure(); err != nil {
		return fmt.Errorf("can't create %s: %w", a.Home, err)
	}
	return nil
}
