package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jeon-jihyeon/rindcut/internal/observe/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStoreRecovery(t *testing.T) {
	tcs := []struct {
		name string
		stop int
	}{
		{name: "planned", stop: 0},
		{name: "after quarantine rename", stop: 1},
		{name: "quarantined", stop: 2},
		{name: "empty ledger", stop: 3},
		{name: "new head", stop: 4},
		{name: "initialized", stop: 5},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			home := privateHome(t)
			s := store.Open(home)
			old, err := s.Append(ctx, records())
			require.NoError(t, err)
			page, err := s.Query(ctx, store.Query{})
			require.NoError(t, err)
			require.NotNil(t, page.NextCursor)
			oldPath := filepath.Join(home, "observe", "ledger.jsonl")
			oldBytes, err := os.ReadFile(oldPath)
			require.NoError(t, err)
			oldBytes[0] = '!'
			require.NoError(t, os.WriteFile(oldPath, oldBytes, 0o600))
			preview, err := s.RecoveryPreview(ctx)
			require.NoError(t, err)
			id := "0123456789abcdef0123456789abcdef"
			intent := map[string]any{
				"token": preview.Token, "previous_store_id": old.StoreID, "new_store_id": id,
				"quarantine": "store-" + id, "stage": "planned",
			}
			writeJSONFile(t, filepath.Join(home, "observe-recovery.json"), intent)
			stageRecovery(t, home, id, tc.stop, intent)
			before := fileHashes(t, home)
			_, err = s.Query(ctx, store.Query{})
			assert.ErrorIs(t, err, store.ErrRecoveryPending)
			_, err = s.Read(ctx)
			assert.ErrorIs(t, err, store.ErrRecoveryPending)
			assert.Equal(t, before, fileHashes(t, home))
			resumable, err := s.RecoveryPreview(ctx)
			require.NoError(t, err)
			assert.Equal(t, id, resumable.NewStoreID)
			unlock, err := s.AcquireControl(ctx)
			require.NoError(t, err)
			result, err := s.Recover(ctx, resumable.Token)
			unlock()
			require.NoError(t, err)
			assert.Equal(t, "complete", result.Stage)
			assert.Equal(t, id, result.NewStoreID)
			current, err := s.Read(ctx)
			require.NoError(t, err)
			assert.Equal(t, id, current.StoreID)
			assert.Empty(t, current.Records)
			assert.Equal(t, int64(0), current.CommittedOffset)
			preserved, err := os.ReadFile(filepath.Join(home, result.Quarantine, "ledger.jsonl"))
			require.NoError(t, err)
			assert.Equal(t, oldBytes, preserved)
			entries, err := os.ReadDir(filepath.Join(home, "observe-quarantine"))
			require.NoError(t, err)
			assert.Len(t, entries, 1)
			_, err = s.Query(ctx, store.Query{Cursor: *page.NextCursor})
			assert.ErrorIs(t, err, store.ErrInvalidCursor)
			assert.NoFileExists(t, filepath.Join(home, "observe-recovery.json"))
		})
	}
}

func stageRecovery(t *testing.T, home, id string, stop int, intent map[string]any) {
	t.Helper()
	if stop < 1 {
		return
	}
	root := filepath.Join(home, "observe-quarantine")
	require.NoError(t, os.Mkdir(root, 0o700))
	require.NoError(t, os.Rename(filepath.Join(home, "observe"), filepath.Join(root, "store-"+id)))
	if stop < 2 {
		return
	}
	intent["stage"] = "quarantined"
	writeJSONFile(t, filepath.Join(home, "observe-recovery.json"), intent)
	if stop < 3 {
		return
	}
	require.NoError(t, os.Mkdir(filepath.Join(home, "observe"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "observe", "ledger.jsonl"), nil, 0o600))
	if stop < 4 {
		return
	}
	writeJSONFile(t, filepath.Join(home, "observe", "head.json"),
		map[string]any{"store_id": id, "committed_offset": 0})
	if stop < 5 {
		return
	}
	intent["stage"] = "initialized"
	writeJSONFile(t, filepath.Join(home, "observe-recovery.json"), intent)
}

func TestRecoveryPreviewStale(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home := privateHome(t)
	s := store.Open(home)
	_, err := s.Append(ctx, records())
	require.NoError(t, err)
	preview, err := s.RecoveryPreview(ctx)
	require.NoError(t, err)
	_, err = s.Append(ctx, []store.Record{records()[1]})
	require.NoError(t, err)
	before := fileHashes(t, home)
	_, err = s.Recover(ctx, preview.Token)
	assert.ErrorIs(t, err, store.ErrStalePreview)
	assert.Equal(t, before, fileHashes(t, home))
}

func TestRecoveryMissingOriginal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home := privateHome(t)
	s := store.Open(home)
	old, err := s.Append(ctx, records())
	require.NoError(t, err)
	preview, err := s.RecoveryPreview(ctx)
	require.NoError(t, err)
	id := "0123456789abcdef0123456789abcdef"
	intent := map[string]any{
		"token": preview.Token, "previous_store_id": old.StoreID, "new_store_id": id,
		"quarantine": "store-" + id, "stage": "planned",
	}
	writeJSONFile(t, filepath.Join(home, "observe-recovery.json"), intent)
	stageRecovery(t, home, id, 2, intent)
	require.NoError(t, os.RemoveAll(filepath.Join(home, "observe-quarantine", "store-"+id)))
	_, err = s.Recover(ctx, preview.Token)
	assert.ErrorIs(t, err, store.ErrIncomplete)
	assert.NoDirExists(t, filepath.Join(home, "observe"))
}

func TestInvalidRecoveryIntent(t *testing.T) {
	tcs := []struct {
		name string
		data string
	}{
		{name: "malformed", data: `!`},
		{name: "missing fields", data: `{}`},
		{name: "path injection", data: `{"token":"a","new_store_id":"../outside","quarantine":"../outside","stage":"planned"}`},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := privateHome(t)
			s := store.Open(home)
			_, err := s.Append(context.Background(), records())
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(home, "observe-recovery.json"), []byte(tc.data), 0o600))
			before := fileHashes(t, home)
			_, err = s.Query(context.Background(), store.Query{})
			assert.ErrorIs(t, err, store.ErrIncomplete)
			_, err = s.RecoveryPreview(context.Background())
			assert.ErrorIs(t, err, store.ErrIncomplete)
			assert.Equal(t, before, fileHashes(t, home))
		})
	}
}
