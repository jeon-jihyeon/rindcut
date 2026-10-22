// Package export carries ledger entries to the tools people already look at
package export

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

const batchSize = 100

// ErrDeferred means an entry can't be delivered safely yet, like a nodloop run that isn't recorded
// The runner keeps its id in the cursor's pending list and moves on to later entries
var ErrDeferred = errors.New("delivery pending")

// Exporter names, the keys of exporters.json and of each cursor file
const (
	NameOTLP     = "otlp"
	NameLangfuse = "langfuse-score"
	NameNodloop  = "nodloop"
)

//go:generate go run go.uber.org/mock/mockgen@v0.6.0 -source=runner.go -destination=mocks/exporter_mock.go -package=mocks Exporter

// Exporter sends verdicts and corrections somewhere else
// Export must be idempotent, because a crash between a send and the cursor write resends the batch
type Exporter interface {
	Name() string
	Export(ctx context.Context, entries []ledger.Entry) error
}

// Carried says which entries leave the machine
// Sessions, probes and job revisions stay local: they hold prompts and paths, and no backend needs them
func Carried(k ledger.Kind) bool { return k == ledger.KindVerdict || k == ledger.KindCorrection }

// PermanentError is a refusal that resending won't change, like a 400
// The runner drops the entry instead of retrying it forever behind every later one
type PermanentError struct{ Err error }

func (e PermanentError) Error() string { return e.Err.Error() }
func (e PermanentError) Unwrap() error { return e.Err }

// Cursor is how far one exporter got
type Cursor struct {
	// Last is the id of the last entry the exporter accepted, deferred or dropped
	Last string `json:"last"`
	// Pending lists deferred entry ids, retried in ledger order before new entries until accepted or dropped
	Pending []string `json:"pending,omitempty"`
	Dropped int      `json:"dropped,omitempty"`
	// LastDropped is the latest dropped entry and why, kept after later sends succeed
	LastDropped string `json:"last_dropped,omitempty"`
	// LastError is the latest failure of the last run, recomputed on every run
	LastError string     `json:"last_error,omitempty"`
	FailedAt  *time.Time `json:"failed_at,omitempty"`
}

// Lag is what one exporter still owes and what it gave up on, shown in the daily summary so a broken export isn't silent
type Lag struct {
	Exporter    string     `json:"exporter"`
	Pending     int        `json:"pending"`
	Dropped     int        `json:"dropped,omitempty"`
	LastDropped string     `json:"last_dropped,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	FailedAt    *time.Time `json:"failed_at,omitempty"`
}

func (c Cursor) lag(name string, pending int) Lag {
	return Lag{Exporter: name, Pending: pending, Dropped: c.Dropped, LastDropped: c.LastDropped, LastError: c.LastError, FailedAt: c.FailedAt}
}

type Runner struct {
	Exporters []Exporter
	// CursorDir holds one JSON file per exporter
	CursorDir string
	Now       func() time.Time
}

// Run retries each exporter's pending entries one by one, then sends every carried entry after its cursor in batches, oldest first
// A pending retry never moves the cursor back, and one exporter failing never holds back another
func (r Runner) Run(ctx context.Context, entries []ledger.Entry) []Lag {
	carried, positions := carriedOnly(entries)
	lags := make([]Lag, 0, len(r.Exporters))
	for _, ex := range r.Exporters {
		cur := loadCursor(r.CursorDir, ex.Name())
		cur.LastError, cur.FailedAt = "", nil
		deferred := cur.Pending
		cur.Pending = nil
		for _, id := range deferred {
			i, ok := positions[id]
			if !ok {
				cur.Pending = append(cur.Pending, id)
				continue
			}
			last := cur.Last
			if err := r.send(ctx, ex, carried[i:i+1], &cur); err != nil {
				cur.Pending = append(cur.Pending, id)
				r.failed(&cur, err)
			}
			cur.Last = last
		}
		rest := after(carried, cur.Last)
		for len(rest) > 0 {
			n := min(batchSize, len(rest))
			if err := r.send(ctx, ex, rest[:n], &cur); err != nil {
				r.failed(&cur, err)
				break
			}
			rest = rest[n:]
		}
		if err := saveCursor(r.CursorDir, ex.Name(), cur); err != nil {
			r.failed(&cur, err)
		}
		lags = append(lags, cur.lag(ex.Name(), len(after(carried, cur.Last))+len(cur.Pending)))
	}
	return lags
}

func (r Runner) failed(cur *Cursor, err error) {
	now := r.Now().UTC()
	cur.LastError, cur.FailedAt = err.Error(), &now
}

// send delivers one batch and moves the cursor past it
// A permanent refusal or a deferral of a batch is retried entry by entry, so only those entries are dropped or kept pending
func (r Runner) send(ctx context.Context, ex Exporter, batch []ledger.Entry, cur *Cursor) error {
	err := ex.Export(ctx, batch)
	var permanent PermanentError
	deferred := errors.Is(err, ErrDeferred)
	switch {
	case err == nil:
		cur.Last = batch[len(batch)-1].ID
		return nil
	case !errors.As(err, &permanent) && !deferred:
		return err
	case len(batch) > 1:
		for _, e := range batch {
			if err := r.send(ctx, ex, []ledger.Entry{e}, cur); err != nil {
				return err
			}
		}
		return nil
	default:
		if deferred {
			cur.Last = batch[0].ID
			cur.Pending = append(cur.Pending, batch[0].ID)
			if err != ErrDeferred {
				r.failed(cur, err)
			}
			return nil
		}
		cur.Last, cur.Dropped, cur.LastDropped = batch[0].ID, cur.Dropped+1, batch[0].ID+": "+err.Error()
		return nil
	}
}

// Lags reports what each named exporter owes without building or calling any exporter
func Lags(cursorDir string, names []string, entries []ledger.Entry) []Lag {
	carried, _ := carriedOnly(entries)
	lags := make([]Lag, 0, len(names))
	for _, name := range names {
		cur := loadCursor(cursorDir, name)
		lags = append(lags, cur.lag(name, len(after(carried, cur.Last))+len(cur.Pending)))
	}
	return lags
}

// Reset makes the next run resend everything, for a new backend or a wiped one
func Reset(cursorDir, name string) error {
	err := os.Remove(cursorPath(cursorDir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func carriedOnly(entries []ledger.Entry) ([]ledger.Entry, map[string]int) {
	var out []ledger.Entry
	positions := map[string]int{}
	for _, e := range entries {
		if Carried(e.Kind) {
			positions[e.ID] = len(out)
			out = append(out, e)
		}
	}
	return out, positions
}

// after is the entries following the one with id
// A cursor the ledger doesn't know means the ledger was replaced, so everything is sent again
func after(entries []ledger.Entry, id string) []ledger.Entry {
	if id == "" {
		return entries
	}
	for i, e := range entries {
		if e.ID == id {
			return entries[i+1:]
		}
	}
	return entries
}

func cursorPath(dir, name string) string { return filepath.Join(dir, name+".json") }

func loadCursor(dir, name string) Cursor {
	var c Cursor
	if b, err := os.ReadFile(cursorPath(dir, name)); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

// Written to a temp file and renamed, so a crash never leaves half a cursor
func saveCursor(dir, name string, c Cursor) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := cursorPath(dir, name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	return os.Rename(tmp, cursorPath(dir, name))
}
