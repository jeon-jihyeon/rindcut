package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jeon-jihyeon/rindcut/internal/observe/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommittedUnicodeIntegrity(t *testing.T) {
	tcs := []struct {
		name     string
		from, to []byte
	}{
		{name: "invalid_utf8", from: []byte("acme"), to: []byte{0xff, 'c', 'm', 'e'}},
		{name: "unpaired_high", from: []byte(`\ud000`), to: []byte(`\ud800`)},
		{name: "unpaired_low", from: []byte(`\ud000`), to: []byte(`\udc00`)},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := privateHome(t)
			s := store.Open(home)
			record := records()[1]
			record.Data = json.RawMessage(`{"source_id":"acme","value":"\ud000"}`)
			_, err := s.Append(t.Context(), []store.Record{record})
			require.NoError(t, err)
			path := filepath.Join(home, "observe", "ledger.jsonl")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Contains(t, string(data), string(tc.from))
			require.NoError(t, os.WriteFile(path, bytes.Replace(data, tc.from, tc.to, 1), 0o600))
			before := fileHashes(t, home)
			_, err = s.Query(t.Context(), store.Query{})
			assert.ErrorIs(t, err, store.ErrCorrupt)
			_, err = s.Read(t.Context())
			assert.ErrorIs(t, err, store.ErrCorrupt)
			assert.Equal(t, before, fileHashes(t, home))
		})
	}
}

func TestValidUnicodeRecordsRemainReadable(t *testing.T) {
	for _, value := range []string{`\ud83d\ude00`, `\\ud800`, `acme`, `\u00ff`} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			s := store.Open(privateHome(t))
			record := records()[1]
			record.Data = json.RawMessage(`{"value":"` + value + `"}`)
			_, err := s.Append(t.Context(), []store.Record{record})
			require.NoError(t, err)
			page, err := s.Query(t.Context(), store.Query{})
			require.NoError(t, err)
			assert.Len(t, page.Records, 1)
		})
	}
}

func TestCommittedIntegrity(t *testing.T) {
	tcs := []struct {
		name   string
		change string
		err    error
	}{
		{name: "unpublished tail", change: "tail"},
		{name: "short prefix", change: "short", err: store.ErrCorrupt},
		{name: "invalid committed row", change: "row", err: store.ErrCorrupt},
		{name: "missing head", change: "head", err: store.ErrCorrupt},
		{name: "duplicate head key", change: "duplicate", err: store.ErrCorrupt},
		{name: "unfinished commit", change: "intent", err: store.ErrIncomplete},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := privateHome(t)
			s := store.Open(home)
			ctx := context.Background()
			snapshot, err := s.Append(ctx, records())
			require.NoError(t, err)
			path := filepath.Join(home, "observe", "ledger.jsonl")
			switch tc.change {
			case "tail":
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				require.NoError(t, err)
				_, err = f.WriteString("unfinished")
				require.NoError(t, err)
				require.NoError(t, f.Close())
			case "short":
				require.NoError(t, os.Truncate(path, snapshot.CommittedOffset-1))
			case "row":
				f, err := os.OpenFile(path, os.O_WRONLY, 0)
				require.NoError(t, err)
				_, err = f.WriteAt([]byte("!"), 0)
				require.NoError(t, err)
				require.NoError(t, f.Close())
			case "head":
				require.NoError(t, os.Remove(filepath.Join(home, "observe", "head.json")))
			case "duplicate":
				require.NoError(t, os.WriteFile(filepath.Join(home, "observe", "head.json"),
					[]byte(`{"store_id":"a","store_id":"b","committed_offset":0}`), 0o600))
			case "intent":
				require.NoError(t, os.WriteFile(filepath.Join(home, "observe", "commit.intent"), []byte(`{}`), 0o600))
			}
			before := fileHashes(t, home)
			_, err = s.Query(ctx, store.Query{})
			assert.ErrorIs(t, err, tc.err)
			_, err = s.Read(ctx)
			assert.ErrorIs(t, err, tc.err)
			assert.Equal(t, before, fileHashes(t, home))
		})
	}
}

func TestAppendHeadFailure(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	s := store.Open(home)
	ctx := context.Background()
	initial, err := s.Append(ctx, records())
	require.NoError(t, err)
	ledgerPath := filepath.Join(home, "observe", "ledger.jsonl")
	before, err := os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(home, "observe", "head.json.tmp"), 0o700))
	_, err = s.Append(ctx, []store.Record{records()[1]})
	require.Error(t, err)
	_, err = s.Query(ctx, store.Query{})
	assert.ErrorIs(t, err, store.ErrIncomplete)
	restored, err := s.Prepare(ctx)
	require.NoError(t, err)
	assert.Equal(t, initial.CommittedOffset, restored.CommittedOffset)
	after, err := os.ReadFile(ledgerPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	require.NoError(t, os.Remove(filepath.Join(home, "observe", "head.json.tmp")))
	_, err = s.Append(ctx, []store.Record{records()[1]})
	require.NoError(t, err)
}

func TestCommitReplay(t *testing.T) {
	tcs := []struct {
		name      string
		published bool
		count     int
	}{
		{name: "previous head truncates tail", count: 5},
		{name: "published head retains batch", published: true, count: 6},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			home := privateHome(t)
			s := store.Open(home)
			initial, err := s.Append(ctx, records())
			require.NoError(t, err)
			line, err := json.Marshal(records()[1])
			require.NoError(t, err)
			line = append(line, '\n')
			previous := map[string]any{"store_id": initial.StoreID, "committed_offset": initial.CommittedOffset}
			proposed := map[string]any{
				"store_id": initial.StoreID, "committed_offset": initial.CommittedOffset + int64(len(line)),
			}
			writeJSONFile(t, filepath.Join(home, "observe", "commit.intent"),
				map[string]any{"previous": previous, "proposed": proposed})
			f, err := os.OpenFile(filepath.Join(home, "observe", "ledger.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = f.Write(line)
			require.NoError(t, err)
			require.NoError(t, f.Sync())
			require.NoError(t, f.Close())
			if tc.published {
				writeJSONFile(t, filepath.Join(home, "observe", "head.json"), proposed)
			}
			_, err = s.Read(ctx)
			assert.ErrorIs(t, err, store.ErrIncomplete)
			restored, err := s.Prepare(ctx)
			require.NoError(t, err)
			assert.Len(t, restored.Records, tc.count)
			assert.NoFileExists(t, filepath.Join(home, "observe", "commit.intent"))
		})
	}
}

func TestStateSymlink(t *testing.T) {
	tcs := []struct {
		name string
		path string
	}{
		{name: "ledger", path: "observe/ledger.jsonl"},
		{name: "head", path: "observe/head.json"},
		{name: "append lock", path: "observe-append.lock"},
		{name: "recovery intent", path: "observe-recovery.json"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := privateHome(t)
			s := store.Open(home)
			_, err := s.Append(context.Background(), records())
			require.NoError(t, err)
			target := filepath.Join(privateHome(t), "target")
			require.NoError(t, os.WriteFile(target, []byte("untouched"), 0o600))
			path := filepath.Join(home, tc.path)
			err = os.Remove(path)
			require.True(t, err == nil || os.IsNotExist(err))
			require.NoError(t, os.Symlink(target, path))
			_, err = s.Query(context.Background(), store.Query{})
			require.Error(t, err)
			data, err := os.ReadFile(target)
			require.NoError(t, err)
			assert.Equal(t, "untouched", string(data))
		})
	}
}

func TestUnprivateHomeWithObservationArtifacts(t *testing.T) {
	tcs := []struct {
		name      string
		directory bool
	}{
		{name: "observe", directory: true},
		{name: "observe-append.lock"},
		{name: "observe-control.lock"},
		{name: "observe-recovery.json"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := privateHome(t)
			require.NoError(t, os.Chmod(home, 0o755))
			path := filepath.Join(home, tc.name)
			if tc.directory {
				require.NoError(t, os.Mkdir(path, 0o700))
			} else {
				require.NoError(t, os.WriteFile(path, nil, 0o600))
			}
			before := fileHashes(t, home)
			_, err := store.Open(home).Query(context.Background(), store.Query{})
			assert.ErrorIs(t, err, store.ErrCorrupt)
			assert.Equal(t, before, fileHashes(t, home))
		})
	}
}

func TestHomeSymlink(t *testing.T) {
	t.Parallel()
	root := privateHome(t)
	target := privateHome(t)
	home := filepath.Join(root, "home")
	require.NoError(t, os.Symlink(target, home))
	_, err := store.Open(home).Read(context.Background())
	assert.ErrorIs(t, err, store.ErrCorrupt)
	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	_, err = f.Write(append(data, '\n'))
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())
}
