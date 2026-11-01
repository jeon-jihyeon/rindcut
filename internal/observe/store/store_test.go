package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/observe/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func records() []store.Record {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return []store.Record{
		{ID: "private-source", Kind: "source", At: at, Data: json.RawMessage(`{"command":"private","cursor":"secret"}`)},
		{ID: "event-1", Kind: "event", At: at, Data: json.RawMessage(
			`{"source_id":"acme","type":"acme.created","action_id":"action-1","capture_id":"capture-1"}`)},
		{ID: "private-checkpoint", Kind: "checkpoint", At: at, Data: json.RawMessage(`{"cursor":"secret"}`)},
		{ID: "observation-1", Kind: "observation", At: at, Data: json.RawMessage(
			`{"source_id":"acme","type":"acme.created","action_id":"action-1"}`)},
		{ID: "cycle-1", Kind: "cycle", At: at, Data: json.RawMessage(`{"status":"complete"}`)},
	}
}

func privateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.Chmod(home, 0o700))
	return home
}

func recordIDs(records []store.Record) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return ids
}

func TestQuery(t *testing.T) {
	tcs := []struct {
		name  string
		query store.Query
		ids   []string
	}{
		{name: "all public", ids: []string{"event-1", "observation-1", "cycle-1"}},
		{name: "source", query: store.Query{SourceID: "acme"}, ids: []string{"event-1", "observation-1"}},
		{name: "type", query: store.Query{Type: "acme.created"}, ids: []string{"event-1", "observation-1"}},
		{name: "action", query: store.Query{ActionID: "action-1"}, ids: []string{"event-1", "observation-1"}},
		{name: "kind", query: store.Query{Kind: "event"}, ids: []string{"event-1"}},
		{name: "missing", query: store.Query{SourceID: "missing"}, ids: []string{}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			home := privateHome(t)
			s := store.Open(home)
			written, err := s.Append(ctx, records())
			require.NoError(t, err)
			assert.NotEmpty(t, written.StoreID)
			assert.Empty(t, written.Records)
			before := fileHashes(t, home)
			page, err := s.Query(ctx, tc.query)
			require.NoError(t, err)
			assert.Equal(t, tc.ids, recordIDs(page.Records))
			assert.True(t, page.StorePresent)
			assert.False(t, page.HasMore)
			assert.NotNil(t, page.NextCursor)
			snapshot, err := s.Read(ctx)
			require.NoError(t, err)
			assert.Len(t, snapshot.Records, 5)
			assert.Equal(t, before, fileHashes(t, home))
		})
	}
}

func TestAbsentStoreReadOnly(t *testing.T) {
	tcs := []struct {
		name string
		leaf string
		mode os.FileMode
	}{
		{name: "absent home", leaf: "new-home"},
		{name: "existing home"},
		{name: "legacy existing home", mode: 0o755},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := privateHome(t)
			if tc.mode != 0 {
				require.NoError(t, os.Chmod(root, tc.mode))
			}
			home := filepath.Join(root, tc.leaf)
			s := store.Open(home)
			ctx := context.Background()
			before := fileHashes(t, root)
			snapshot, err := s.Read(ctx)
			require.NoError(t, err)
			assert.False(t, snapshot.Present)
			page, err := s.Query(ctx, store.Query{})
			require.NoError(t, err)
			assert.Equal(t, store.Page{Records: []store.Record{}}, page)
			preview, err := s.RecoveryPreview(ctx)
			require.NoError(t, err)
			assert.Equal(t, "preview", preview.Stage)
			assert.Equal(t, before, fileHashes(t, root))
			info, err := os.Stat(root)
			require.NoError(t, err)
			assert.Equal(t, tc.mode == 0o755, info.Mode().Perm() == 0o755)
		})
	}
}

func TestQueryCursor(t *testing.T) {
	type want struct {
		ids      []string
		more     bool
		endLimit int
	}
	tcs := []struct {
		name  string
		limit int
		want  want
	}{
		{name: "next page", limit: 1, want: want{ids: []string{"observation-1"}, more: true, endLimit: 2}},
		{name: "limit can change", limit: 2, want: want{ids: []string{"observation-1", "cycle-1"}, endLimit: 100}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := store.Open(privateHome(t))
			_, err := s.Append(ctx, records())
			require.NoError(t, err)
			page, err := s.Query(ctx, store.Query{Limit: 1})
			require.NoError(t, err)
			assert.Equal(t, []string{"event-1"}, recordIDs(page.Records))
			assert.True(t, page.HasMore)
			require.NotNil(t, page.NextCursor)
			query := store.Query{Limit: tc.limit, Cursor: *page.NextCursor}
			got, err := s.Query(ctx, query)
			require.NoError(t, err)
			assert.Equal(t, tc.want.ids, recordIDs(got.Records))
			assert.Equal(t, tc.want.more, got.HasMore)
			assert.True(t, got.StorePresent)
			require.NotNil(t, got.NextCursor)
			expected, err := s.Query(ctx, store.Query{Limit: tc.want.endLimit})
			require.NoError(t, err)
			assert.Equal(t, expected.NextCursor, got.NextCursor)
		})
	}
}

func TestQueryInvalidCursor(t *testing.T) {
	tcs := []struct {
		name   string
		change string
	}{
		{name: "filter is bound", change: "filter"},
		{name: "store is bound", change: "store"},
		{name: "row boundary", change: "offset"},
		{name: "damaged cursor", change: "damaged"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := store.Open(privateHome(t))
			_, err := s.Append(ctx, records())
			require.NoError(t, err)
			page, err := s.Query(ctx, store.Query{Limit: 1})
			require.NoError(t, err)
			require.NotNil(t, page.NextCursor)
			query := store.Query{Limit: 1, Cursor: *page.NextCursor}
			switch tc.change {
			case "filter":
				query.SourceID = "acme"
			case "store", "offset":
				query.Cursor = changeCursor(t, query.Cursor, tc.change)
			case "damaged":
				query.Cursor = "!"
			}
			got, err := s.Query(ctx, query)
			require.ErrorIs(t, err, store.ErrInvalidCursor)
			assert.Empty(t, got.Records)
			assert.Nil(t, got.NextCursor)
		})
	}
}

func TestQueryLateAppendAndTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := store.Open(privateHome(t))
	written, err := s.Append(ctx, records())
	require.NoError(t, err)
	target := &store.TargetRef{StoreID: written.StoreID, CaptureID: "capture-1"}
	page, err := s.Query(ctx, store.Query{TargetRef: target})
	require.NoError(t, err)
	assert.Equal(t, []string{"event-1"}, recordIDs(page.Records))
	end, err := s.Query(ctx, store.Query{})
	require.NoError(t, err)
	require.NotNil(t, end.NextCursor)
	data, err := json.Marshal(map[string]any{"source_id": "acme", "target_ref": target})
	require.NoError(t, err)
	_, err = s.Append(ctx, []store.Record{{
		ID: "late", Kind: "observation", At: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Data: data,
	}})
	require.NoError(t, err)
	late, err := s.Query(ctx, store.Query{Cursor: *end.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, []string{"late"}, recordIDs(late.Records))
	targeted, err := s.Query(ctx, store.Query{TargetRef: target})
	require.NoError(t, err)
	assert.Equal(t, []string{"event-1", "late"}, recordIDs(targeted.Records))
}

func TestAppendValidation(t *testing.T) {
	tcs := []struct {
		name string
		data string
		kind string
		err  error
	}{
		{name: "object", data: `{}`, kind: "event"},
		{name: "array", data: `[]`, kind: "event", err: store.ErrCorrupt},
		{name: "null", data: `null`, kind: "event", err: store.ErrCorrupt},
		{name: "duplicate", data: `{"id":1,"id":2}`, kind: "event", err: store.ErrCorrupt},
		{name: "unknown kind", data: `{}`, kind: "unknown", err: store.ErrCorrupt},
		{name: "too large", data: `{"value":"` + strings.Repeat("x", 64<<10) + `"}`, kind: "event", err: store.ErrLineTooLong},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := store.Open(privateHome(t))
			_, err := s.Append(context.Background(), []store.Record{{
				ID: "acme", Kind: tc.kind, At: time.Now().UTC(), Data: json.RawMessage(tc.data),
			}})
			assert.ErrorIs(t, err, tc.err)
		})
	}
}

func TestQueryPreservesRawValues(t *testing.T) {
	t.Parallel()
	s := store.Open(privateHome(t))
	record := records()[1]
	record.Data = json.RawMessage(`{"source_id":"acme","target_ref":false,"action_id":17}`)
	_, err := s.Append(context.Background(), []store.Record{record})
	require.NoError(t, err)
	page, err := s.Query(context.Background(), store.Query{SourceID: "acme"})
	require.NoError(t, err)
	assert.Equal(t, []string{"event-1"}, recordIDs(page.Records))
	assert.JSONEq(t, string(record.Data), string(page.Records[0].Data))
}

func changeCursor(t *testing.T, value, change string) string {
	t.Helper()
	data, err := base64.RawURLEncoding.DecodeString(value)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	if change == "store" {
		fields["store_id"] = "another-store"
	} else {
		fields["offset"] = float64(1)
	}
	data, err = json.Marshal(fields)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(data)
}

func fileHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[relative] = fmt.Sprintf("%x:%o", sha256.Sum256(data), info.Mode().Perm())
		return nil
	})
	require.NoError(t, err)
	return result
}
