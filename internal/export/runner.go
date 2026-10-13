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

// PermanentError is a refusal that resending won't change, like a 400 or a run nodloop doesn't know
// The runner drops the entry instead of retrying it forever behind every later one
type PermanentError struct{ Err error }

func (e PermanentError) Error() string { return e.Err.Error() }
func (e PermanentError) Unwrap() error { return e.Err }

// Cursor is how far one exporter got
type Cursor struct {
	// Last is the id of the last entry the exporter accepted or dropped
	Last    string `json:"last"`
	Dropped int    `json:"dropped,omitempty"`
	// LastDropped is the latest dropped entry and why, kept after later sends succeed
	LastDropped string `json:"last_dropped,omitempty"`
	// LastError is the latest failure that will be retried, cleared by the next success
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

// Run sends every carried entry after each exporter's cursor, in batches, oldest first
// One exporter failing never holds back another
func (r Runner) Run(ctx context.Context, entries []ledger.Entry) []Lag {
	carried := carriedOnly(entries)
	lags := make([]Lag, 0, len(r.Exporters))
	for _, ex := range r.Exporters {
		cur := loadCursor(r.CursorDir, ex.Name())
		rest := after(carried, cur.Last)
		for len(rest) > 0 {
			n := min(batchSize, len(rest))
			if err := r.send(ctx, ex, rest[:n], &cur); err != nil {
				now := r.Now().UTC()
				cur.LastError, cur.FailedAt = err.Error(), &now
				break
			}
			rest = rest[n:]
		}
		_ = saveCursor(r.CursorDir, ex.Name(), cur)
		lags = append(lags, cur.lag(ex.Name(), len(after(carried, cur.Last))))
	}
	return lags
}

// send delivers one batch and moves the cursor past it
// A permanent refusal of a batch is retried entry by entry, so only the refused entries are dropped
func (r Runner) send(ctx context.Context, ex Exporter, batch []ledger.Entry, cur *Cursor) error {
	err := ex.Export(ctx, batch)
	var permanent PermanentError
	switch {
	case err == nil:
		cur.Last, cur.LastError, cur.FailedAt = batch[len(batch)-1].ID, "", nil
		return nil
	case !errors.As(err, &permanent):
		return err
	case len(batch) > 1:
		for _, e := range batch {
			if err := r.send(ctx, ex, []ledger.Entry{e}, cur); err != nil {
				return err
			}
		}
		return nil
	default:
		cur.Last, cur.Dropped, cur.LastDropped = batch[0].ID, cur.Dropped+1, batch[0].ID+": "+err.Error()
		return nil
	}
}

// Lags reports what each named exporter owes without building or calling any exporter
func Lags(cursorDir string, names []string, entries []ledger.Entry) []Lag {
	carried := carriedOnly(entries)
	lags := make([]Lag, 0, len(names))
	for _, name := range names {
		cur := loadCursor(cursorDir, name)
		lags = append(lags, cur.lag(name, len(after(carried, cur.Last))))
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

func carriedOnly(entries []ledger.Entry) []ledger.Entry {
	var out []ledger.Entry
	for _, e := range entries {
		if Carried(e.Kind) {
			out = append(out, e)
		}
	}
	return out
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
	return os.Rename(tmp, cursorPath(dir, name))
}
