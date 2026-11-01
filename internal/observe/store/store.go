package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

const maxLine = 64 << 10

var (
	ErrBusy            = errors.New("observe_busy")
	ErrCorrupt         = errors.New("store_corrupt")
	ErrIncomplete      = errors.New("storage_incomplete")
	ErrRecoveryPending = errors.New("recovery_pending")
	ErrInvalidCursor   = errors.New("invalid_cursor")
	ErrStalePreview    = errors.New("stale_preview")
	ErrLineTooLong     = errors.New("record_too_large")
)

type Record struct {
	ID   string          `json:"id"`
	Kind string          `json:"kind"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

type Snapshot struct {
	StoreID         string
	CommittedOffset int64
	Records         []Record
	Present         bool
}

type TargetRef struct {
	StoreID   string `json:"store_id"`
	CaptureID string `json:"capture_id"`
}

type Query struct {
	SourceID  string
	Type      string
	ActionID  string
	TargetRef *TargetRef
	Kind      string
	Cursor    string
	Limit     int
}

type Page struct {
	Records      []Record `json:"records"`
	NextCursor   *string  `json:"next_cursor"`
	HasMore      bool     `json:"has_more"`
	StorePresent bool     `json:"store_present"`
}

type Recovery struct {
	Token          string `json:"token"`
	Stage          string `json:"stage"`
	StoreID        string `json:"store_id,omitempty"`
	NewStoreID     string `json:"new_store_id,omitempty"`
	Quarantine     string `json:"quarantine,omitempty"`
	ContinuityLost bool   `json:"continuity_lost"`
}

type Store struct {
	home string
}

type head struct {
	StoreID string `json:"store_id"`
	Offset  int64  `json:"committed_offset"`
}

type commit struct {
	Previous head `json:"previous"`
	Proposed head `json:"proposed"`
}

func validStoreID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, value := range id {
		if !strings.ContainsRune("0123456789abcdef", value) {
			return false
		}
	}
	return true
}

func Open(home string) *Store { return &Store{home: home} }

func (s *Store) path(name string) string { return filepath.Join(s.home, "observe", name) }

func (s *Store) AcquireControl(ctx context.Context) (func(), error) {
	if err := ensureDir(s.home); err != nil {
		return nil, err
	}
	return acquire(ctx, filepath.Join(s.home, "observe-control.lock"), false, true)
}

func (s *Store) Read(ctx context.Context) (Snapshot, error) {
	unlock, present, err := s.readLock(ctx)
	if err != nil || !present {
		return Snapshot{}, err
	}
	defer unlock()
	return s.read(ctx, false, true)
}

func (s *Store) Prepare(ctx context.Context) (Snapshot, error) {
	if err := ensureDir(s.home); err != nil {
		return Snapshot{}, err
	}
	unlock, err := s.appendLock(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	if err := s.checkRecovery(); err != nil {
		return Snapshot{}, err
	}
	if err := s.resumeCommit(ctx); err != nil {
		return Snapshot{}, err
	}
	return s.read(ctx, false, true)
}

func (s *Store) Append(ctx context.Context, records []Record) (Snapshot, error) {
	encoded, err := encodeRecords(ctx, records)
	if err != nil {
		return Snapshot{}, err
	}
	if err := ensureDir(s.home); err != nil {
		return Snapshot{}, err
	}
	unlock, err := s.appendLock(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	if err := s.checkRecovery(); err != nil {
		return Snapshot{}, err
	}
	if err := s.resumeCommit(ctx); err != nil {
		return Snapshot{}, err
	}
	if err := s.initialize(); err != nil {
		return Snapshot{}, err
	}
	before, err := s.read(ctx, false, false)
	if err != nil || len(records) == 0 {
		return before, err
	}
	previous := head{StoreID: before.StoreID, Offset: before.CommittedOffset}
	proposed := head{StoreID: previous.StoreID, Offset: previous.Offset + int64(len(encoded))}
	if proposed.Offset < previous.Offset {
		return Snapshot{}, ErrLineTooLong
	}
	if err := s.appendBatch(ctx, previous, proposed, encoded); err != nil {
		return Snapshot{}, err
	}
	before.CommittedOffset = proposed.Offset
	return before, nil
}
