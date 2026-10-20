package ledger

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/lock"
)

const (
	maxLine = 64 << 10
	// The SessionEnd hook appends inside a 1.5 s budget, so nobody waits long for the lock
	lockWait = 500 * time.Millisecond
)

var (
	ErrLineTooLong = errors.New("ledger entry over 64 KB")
	errOverlong    = errors.New("line over 64 KB, not written by rindcut")
)

// Ledger appends and reads the JSONL file
// It never rewrites a line
type Ledger struct {
	path, lockPath string
	now            func() time.Time
	version        string
	// onSkip hears about lines that aren't valid entries, like a torn last line after a crash
	onSkip func(line int, err error)
}

// Open needs onSkip so a broken line is always reported somewhere, never dropped in silence
func Open(path, lockPath string, now func() time.Time, version string, onSkip func(line int, err error)) *Ledger {
	return &Ledger{path: path, lockPath: lockPath, now: now, version: version, onSkip: onSkip}
}

func (l *Ledger) Append(ctx context.Context, kind Kind, data any) (Entry, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Entry{}, fmt.Errorf("encode %s entry: %w", kind, err)
	}
	at := l.now().UTC()
	e := Entry{ID: newID(at), Kind: kind, At: at, Version: l.version, Data: raw}
	e, line, err := encodeEntry(e, data)
	if err != nil {
		return Entry{}, err
	}
	if len(line) >= maxLine {
		return Entry{}, fmt.Errorf("%s entry has %d bytes: %w", kind, len(line), ErrLineTooLong)
	}
	unlock, err := lock.Acquire(ctx, l.lockPath, lockWait)
	if err != nil {
		return Entry{}, fmt.Errorf("ledger: %w", err)
	}
	defer unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = f.Close() }()
	// A crash mid-write leaves a line with no newline, and the next entry must not glue onto it
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	// One write of one line, so readers never see half an entry from a live writer
	if _, err := f.Write(append(line, '\n')); err != nil {
		return Entry{}, fmt.Errorf("append to ledger: %w", err)
	}
	return e, f.Sync()
}

// Scan reads every entry oldest first
// A missing ledger is empty, not an error
func (l *Ledger) Scan(ctx context.Context, fn func(Entry) error) error {
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, maxLine)
	for n := 1; ; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := readLine(r)
		switch {
		case errors.Is(err, errOverlong):
			l.onSkip(n, err)
		case len(line) > 0:
			if e, perr := parse(line); perr != nil {
				l.onSkip(n, perr)
			} else if ferr := fn(e); ferr != nil {
				return ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil && !errors.Is(err, errOverlong) {
			return err
		}
	}
}

// readLine reads one line within the buffer and drains a longer one, so a damaged file can't grow memory without bound
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		return line, err
	}
	for errors.Is(err, bufio.ErrBufferFull) {
		_, err = r.ReadSlice('\n')
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return nil, errOverlong
}

func parse(line []byte) (Entry, error) {
	var e Entry
	if err := json.Unmarshal(line, &e); err != nil {
		return Entry{}, err
	}
	if e.ID == "" || e.Kind == "" {
		return Entry{}, errors.New("entry without id or kind")
	}
	return e, nil
}

// All loads every entry, which the scale rindcut runs at keeps small
func (l *Ledger) All(ctx context.Context) ([]Entry, error) {
	var out []Entry
	err := l.Scan(ctx, func(e Entry) error {
		out = append(out, e)
		return nil
	})
	return out, err
}
