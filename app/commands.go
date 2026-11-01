package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/home"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/judge"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

func New(getenv func(string) string, now func() time.Time, version string, warn io.Writer) (App, error) {
	h, err := home.Resolve(getenv)
	if err != nil {
		return App{}, err
	}
	user := getenv("HOME")
	a := App{Home: string(h), UserHome: user, Now: now, Version: version, Warn: warn, SearchPath: getenv("PATH"),
		Installer: Scheduler{Binary: stableBinary(h), Home: string(h), UserHome: user, GOOS: runtime.GOOS,
			UID: os.Getuid(), Exec: runScheduler}}
	return a, nil
}

func runScheduler(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return nil
}

func stableBinary(h home.Dir) string {
	stable := h.Path("bin", "rindcut")
	if _, err := os.Stat(stable); err == nil {
		return stable
	}
	if executable, err := os.Executable(); err == nil {
		if abs, err := filepath.EvalSymlinks(executable); err == nil {
			return abs
		}
		return executable
	}
	return stable
}

func (a App) homePath(parts ...string) string {
	return filepath.Join(append([]string{a.Home}, parts...)...)
}
func (a App) lockPath(name string) string { return a.homePath(name + ".lock") }
func (a App) scheduler() judge.Installer  { return judge.Installer(a.Installer) }

func (a App) Install(context.Context) error   { return a.scheduler().Install() }
func (a App) Uninstall(context.Context) error { return a.scheduler().Uninstall() }

func (a App) Check(ctx context.Context, id string, at time.Time) (Judgment, error) {
	entries, err := a.ledger().All(ctx)
	if err != nil {
		return Judgment{}, err
	}
	j, ok := job.Current(revisions(entries))[id]
	if !ok || j.Removed {
		return Judgment{}, fmt.Errorf("%s: %w", id, ErrNoJob)
	}
	entry, err := a.judge().Probe(ctx, j, at, fingerprint(j))
	if err != nil {
		return Judgment{}, err
	}
	var result ledger.Judgment
	if err := entry.Decode(&result); err != nil {
		return Judgment{}, err
	}
	return judgmentView(result), nil
}

type LedgerFilter struct {
	JobID string
	Kind  string
	Since time.Duration
	Limit int
}

func (a App) LedgerEntries(ctx context.Context, filter LedgerFilter) ([]Entry, error) {
	entries, err := a.ledger().All(ctx)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, entry := range entries {
		recent := filter.Since == 0 || a.Now().Sub(entry.At) <= filter.Since
		if (filter.Kind == "" || string(entry.Kind) == filter.Kind) && recent &&
			(filter.JobID == "" || entryAboutJob(entry, filter.JobID)) {
			out = append(out, entryView(entry))
		}
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[len(out)-filter.Limit:]
	}
	return out, nil
}

func entryAboutJob(entry ledger.Entry, id string) bool {
	var ref struct {
		ID    string `json:"id"`
		JobID string `json:"job_id"`
	}
	if json.Unmarshal(entry.Data, &ref) != nil {
		return false
	}
	return ref.JobID == id || (entry.Kind == ledger.KindJob && ref.ID == id)
}

func (a App) Settings() (Settings, error) {
	s, err := a.settings()
	return settingsView(s.Masked()), err
}
