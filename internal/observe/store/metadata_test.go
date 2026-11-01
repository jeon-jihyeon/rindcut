package store_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeon-jihyeon/rindcut/internal/observe/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequiredMetadata(t *testing.T) {
	type testCase struct {
		name   string
		file   string
		parent string
		field  string
		change string
	}
	tcs := make([]testCase, 0, 30)
	for _, location := range []struct{ file, parent, field string }{
		{file: "head.json", field: "committed_offset"},
		{file: "head.json", field: "store_id"},
		{file: "commit.intent", parent: "previous", field: "committed_offset"},
		{file: "commit.intent", parent: "proposed", field: "committed_offset"},
		{file: "commit.intent", field: "previous"},
		{file: "commit.intent", field: "proposed"},
	} {
		for _, change := range []string{"missing", "null", "case", "duplicate_case", "unknown"} {
			tcs = append(tcs, testCase{name: location.file + "_" + location.parent + "_" + location.field + "_" + change,
				file: location.file, parent: location.parent, field: location.field, change: change})
		}
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			home := privateHome(t)
			s := store.Open(home)
			initial, err := s.Append(ctx, records())
			require.NoError(t, err)
			previous := map[string]any{"store_id": initial.StoreID, "committed_offset": initial.CommittedOffset}
			object := previous
			if tc.file == "commit.intent" {
				object = map[string]any{"previous": previous, "proposed": map[string]any{
					"store_id": initial.StoreID, "committed_offset": initial.CommittedOffset + 1}}
			}
			fields := object
			if tc.parent != "" {
				fields = object[tc.parent].(map[string]any)
			}
			mutateField(fields, tc.field, tc.change)
			writeJSONFile(t, filepath.Join(home, "observe", tc.file), object)
			before := fileHashes(t, home)
			_, readErr := s.Query(ctx, store.Query{})
			wantRead := store.ErrCorrupt
			if tc.file == "commit.intent" {
				wantRead = store.ErrIncomplete
			}
			assert.ErrorIs(t, readErr, wantRead)
			_, err = s.Prepare(ctx)
			assert.ErrorIs(t, err, store.ErrCorrupt)
			_, err = s.Append(ctx, []store.Record{records()[1]})
			assert.ErrorIs(t, err, store.ErrCorrupt)
			assert.Equal(t, before, fileHashes(t, home))
			ledger, err := os.ReadFile(filepath.Join(home, "observe", "ledger.jsonl"))
			require.NoError(t, err)
			assert.Contains(t, string(ledger), `"id":"event-1"`)
		})
	}
}

func TestRequiredCursorFields(t *testing.T) {
	type testCase struct{ name, field, change string }
	tcs := make([]testCase, 0, 20)
	for _, field := range []string{"protocol", "store_id", "offset", "filter"} {
		for _, change := range []string{"missing", "null", "case", "duplicate_case", "unknown"} {
			tcs = append(tcs, testCase{name: field + "_" + change, field: field, change: change})
		}
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			home := privateHome(t)
			s := store.Open(home)
			_, err := s.Append(ctx, records())
			require.NoError(t, err)
			page, err := s.Query(ctx, store.Query{})
			require.NoError(t, err)
			require.NotNil(t, page.NextCursor)
			data, err := base64.RawURLEncoding.DecodeString(*page.NextCursor)
			require.NoError(t, err)
			var object map[string]any
			require.NoError(t, json.Unmarshal(data, &object))
			mutateField(object, tc.field, tc.change)
			data, err = json.Marshal(object)
			require.NoError(t, err)
			before := fileHashes(t, home)
			_, err = s.Query(ctx, store.Query{Cursor: base64.RawURLEncoding.EncodeToString(data)})
			assert.ErrorIs(t, err, store.ErrInvalidCursor)
			assert.Equal(t, before, fileHashes(t, home))
		})
	}
}

func TestRawDataNullableFields(t *testing.T) {
	t.Parallel()
	s := store.Open(privateHome(t))
	record := records()[1]
	record.Kind = "checkpoint"
	record.Data = json.RawMessage(`{"generation":null,"cursor":null,"nested":{"unknown":null}}`)
	_, err := s.Append(context.Background(), []store.Record{record})
	require.NoError(t, err)
	snapshot, err := s.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, snapshot.Records, 1)
	assert.JSONEq(t, string(record.Data), string(snapshot.Records[0].Data))
}

func mutateField(object map[string]any, field, change string) {
	value := object[field]
	switch change {
	case "missing":
		delete(object, field)
	case "null":
		object[field] = nil
	case "case":
		delete(object, field)
		object[strings.ToUpper(field)] = value
	case "duplicate_case":
		object[strings.ToUpper(field)] = value
	case "unknown":
		object["unexpected"] = false
	}
}
