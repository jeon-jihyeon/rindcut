package store

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
)

type cursor struct {
	Protocol string `json:"protocol"`
	StoreID  string `json:"store_id"`
	Offset   int64  `json:"offset"`
	Filter   string `json:"filter"`
}

func (s *Store) Query(ctx context.Context, query Query) (Page, error) {
	if query.Limit == 0 {
		query.Limit = 100
	}
	if !validQuery(query) {
		return Page{}, ErrInvalidCursor
	}
	unlock, present, err := s.readLock(ctx)
	if err != nil {
		return Page{}, err
	}
	if !present {
		if query.Cursor != "" {
			return Page{}, ErrInvalidCursor
		}
		return Page{Records: []Record{}}, nil
	}
	defer unlock()
	snapshot, err := s.read(ctx, false, false)
	if err != nil {
		return Page{}, err
	}
	if !snapshot.Present {
		if query.Cursor != "" {
			return Page{}, ErrInvalidCursor
		}
		return Page{Records: []Record{}}, nil
	}
	c, err := decodeCursor(query, snapshot)
	if err != nil {
		return Page{}, err
	}
	f, err := openFile(s.path("ledger.jsonl"), os.O_RDONLY)
	if err != nil {
		return Page{}, ErrCorrupt
	}
	defer func() { _ = f.Close() }()
	if err := cursorBoundary(f, c.Offset); err != nil {
		return Page{}, err
	}
	return queryPage(ctx, f, query, c, snapshot.CommittedOffset)
}

func validQuery(query Query) bool {
	if query.Limit < 1 || query.Limit > 100 || !publicKind(query.Kind) && query.Kind != "" {
		return false
	}
	return query.TargetRef == nil || query.TargetRef.StoreID != "" && query.TargetRef.CaptureID != ""
}

func decodeCursor(query Query, snapshot Snapshot) (cursor, error) {
	filter := filterDigest(query)
	if query.Cursor == "" {
		return cursor{Protocol: "rindcut.records/v1", StoreID: snapshot.StoreID, Filter: filter}, nil
	}
	if len(query.Cursor) > 2048 {
		return cursor{}, ErrInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(query.Cursor)
	if err != nil {
		return cursor{}, ErrInvalidCursor
	}
	var c cursor
	if err := decode(data, &c); err != nil || c.Protocol != "rindcut.records/v1" ||
		c.StoreID != snapshot.StoreID || c.Filter != filter || c.Offset < 0 || c.Offset > snapshot.CommittedOffset {
		return cursor{}, ErrInvalidCursor
	}
	return c, nil
}

func filterDigest(query Query) string {
	data, _ := json.Marshal([]any{query.SourceID, query.Type, query.ActionID, query.TargetRef, query.Kind})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func cursorBoundary(f *os.File, offset int64) error {
	if offset == 0 {
		return nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], offset-1); err != nil || last[0] != '\n' {
		return ErrInvalidCursor
	}
	return nil
}

func queryPage(ctx context.Context, f *os.File, query Query, c cursor, end int64) (Page, error) {
	page := Page{Records: []Record{}, StorePresent: true}
	r := bufio.NewReaderSize(io.NewSectionReader(f, c.Offset, end-c.Offset), maxLine)
	for len(page.Records) < query.Limit {
		if err := ctx.Err(); err != nil {
			return Page{}, err
		}
		line, err := r.ReadSlice('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}
		if err != nil || len(line)-1 >= maxLine {
			return Page{}, ErrCorrupt
		}
		c.Offset += int64(len(line))
		var record Record
		if err := decode(line, &record); err != nil {
			return Page{}, ErrCorrupt
		}
		if matches(query, record, c.StoreID) {
			page.Records = append(page.Records, record)
		}
	}
	data, _ := json.Marshal(c)
	next := base64.RawURLEncoding.EncodeToString(data)
	page.NextCursor = &next
	page.HasMore = c.Offset < end
	return page, nil
}

func publicKind(kind string) bool {
	return kind == "event" || kind == "observation" || kind == "cycle"
}

func matches(query Query, record Record, storeID string) bool {
	if !publicKind(record.Kind) || query.Kind != "" && query.Kind != record.Kind {
		return false
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(record.Data, &data); err != nil {
		return false
	}
	if !matchesString(data, "source_id", query.SourceID) || !matchesString(data, "type", query.Type) ||
		!matchesString(data, "action_id", query.ActionID) {
		return false
	}
	if query.TargetRef == nil {
		return true
	}
	if record.Kind == "event" {
		return query.TargetRef.StoreID == storeID && matchesString(data, "capture_id", query.TargetRef.CaptureID)
	}
	var target TargetRef
	return json.Unmarshal(data["target_ref"], &target) == nil && target == *query.TargetRef
}

func matchesString(data map[string]json.RawMessage, name, wanted string) bool {
	if wanted == "" {
		return true
	}
	var value string
	return json.Unmarshal(data[name], &value) == nil && value == wanted
}
