package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

func (s *Store) initialize() error {
	if err := checkDir(s.path("")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	if err := os.Mkdir(s.path(""), 0o700); err != nil {
		return err
	}
	if err := s.initializeFiles(id); err != nil {
		return err
	}
	return syncDir(s.home)
}

func (s *Store) initializeFiles(id string) error {
	f, err := openFile(s.path("ledger.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return writeJSON(s.path("head.json"), head{StoreID: id})
}

func encodeRecords(ctx context.Context, records []Record) ([]byte, error) {
	var out bytes.Buffer
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validateRecord(record); err != nil {
			return nil, err
		}
		line, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		if len(line) >= maxLine {
			return nil, ErrLineTooLong
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func validateRecord(record Record) error {
	if record.ID == "" || len(record.ID) > 256 || record.At.IsZero() {
		return ErrCorrupt
	}
	switch record.Kind {
	case "source", "event", "observation", "checkpoint", "cycle":
	default:
		return ErrCorrupt
	}
	var data map[string]json.RawMessage
	if err := decode(record.Data, &data); err != nil || data == nil {
		return ErrCorrupt
	}
	return nil
}

func (s *Store) read(ctx context.Context, allowCommit, retain bool) (Snapshot, error) {
	if err := s.checkRecovery(); err != nil {
		return Snapshot{}, err
	}
	if err := checkDir(s.path("")); errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, nil
	} else if err != nil {
		return Snapshot{}, ErrCorrupt
	}
	if !allowCommit {
		if _, err := os.Lstat(s.path("commit.intent")); err == nil {
			return Snapshot{}, ErrIncomplete
		} else if !errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, ErrIncomplete
		}
	}
	var h head
	if err := readJSON(s.path("head.json"), &h); err != nil || !validStoreID(h.StoreID) || h.Offset < 0 {
		return Snapshot{}, ErrCorrupt
	}
	f, err := openFile(s.path("ledger.jsonl"), os.O_RDONLY)
	if err != nil {
		return Snapshot{}, ErrCorrupt
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.Size() < h.Offset {
		return Snapshot{}, ErrCorrupt
	}
	records, err := scan(ctx, f, 0, h.Offset, retain)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{StoreID: h.StoreID, CommittedOffset: h.Offset, Records: records, Present: true}, nil
}

func scan(ctx context.Context, f *os.File, start, end int64, retain bool) ([]Record, error) {
	r := bufio.NewReaderSize(io.NewSectionReader(f, start, end-start), maxLine)
	var records []Record
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := r.ReadSlice('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return records, nil
		}
		if err != nil || len(line)-1 >= maxLine {
			return nil, ErrCorrupt
		}
		var record Record
		if err := decode(line, &record); err != nil {
			return nil, ErrCorrupt
		}
		if err := validateRecord(record); err != nil {
			return nil, ErrCorrupt
		}
		if retain {
			records = append(records, record)
		}
	}
}

func (s *Store) appendBatch(ctx context.Context, previous, proposed head, encoded []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// The intent distinguishes an interrupted batch until the head publishes it
	if err := writeJSON(s.path("commit.intent"), commit{Previous: previous, Proposed: proposed}); err != nil {
		return err
	}
	f, err := openFile(s.path("ledger.jsonl"), os.O_RDWR)
	if err != nil {
		return err
	}
	err = f.Truncate(previous.Offset)
	if err == nil {
		_, err = f.WriteAt(encoded, previous.Offset)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := writeJSON(s.path("head.json"), proposed); err != nil {
		return err
	}
	return removeSynced(s.path("commit.intent"))
}

func (s *Store) resumeCommit(ctx context.Context) error {
	if err := checkDir(s.path("")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return ErrCorrupt
	}
	var intent commit
	if err := readJSON(s.path("commit.intent"), &intent); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return ErrCorrupt
	}
	if !validStoreID(intent.Previous.StoreID) || intent.Previous.StoreID != intent.Proposed.StoreID ||
		intent.Previous.Offset < 0 || intent.Proposed.Offset <= intent.Previous.Offset {
		return ErrCorrupt
	}
	snapshot, err := s.read(ctx, true, false)
	if err != nil {
		return err
	}
	current := head{StoreID: snapshot.StoreID, Offset: snapshot.CommittedOffset}
	if current != intent.Previous && current != intent.Proposed {
		return ErrCorrupt
	}
	return s.finishCommit(current, intent.Previous)
}

func (s *Store) finishCommit(current, previous head) error {
	f, err := openFile(s.path("ledger.jsonl"), os.O_RDWR)
	if err != nil {
		return err
	}
	if current == previous {
		// An unpublished tail cannot advance any reader checkpoint
		err = f.Truncate(current.Offset)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := syncFile(s.path("head.json")); err != nil {
		return err
	}
	if err := syncDir(s.path("")); err != nil {
		return err
	}
	return removeSynced(s.path("commit.intent"))
}

func removeSynced(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
