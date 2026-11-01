package app_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeon-jihyeon/rindcut/app"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func observeSourceRequest(t *testing.T) app.SourceRequest {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	d := map[string]any{"id": "acme", "enabled": true, "source_instance": "acme", "stream": "actions",
		"capability":       map[string]any{"kind": "event_stream", "types": []string{}, "limitations": []string{}},
		"adapter_revision": "v1", "projection_profile": "metadata",
		"reader": map[string]any{"program": exe, "args": []string{}, "allowed_attributes": []string{}},
		"observers": []any{map[string]any{"id": "effect", "types": []string{"acme.action"}, "criterion_id": "landed",
			"criterion_revision": "v1", "parameters": map[string]any{}, "program": exe, "args": []string{}, "deadline_seconds": 60}}}
	b, err := json.Marshal(d)
	require.NoError(t, err)
	revision := int64(0)
	return app.SourceRequest{Definition: b, ExpectedRevision: &revision}
}

func TestSourceRegistration(t *testing.T) {
	tcs := []struct {
		name    string
		change  string
		invalid bool
	}{
		{name: "matching"},
		{name: "definition_changed", change: "definition", invalid: true},
		{name: "missing_confirmation", change: "confirmation", invalid: true},
		{name: "stale_revision", change: "revision", invalid: true},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := filepath.Join(t.TempDir(), "absent")
			a := app.App{Home: home, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }, Warn: io.Discard}
			in := observeSourceRequest(t)
			preview, err := a.SourcePreview(context.Background(), in)
			require.NoError(t, err)
			require.Len(t, preview.Executions, 2)
			assert.Equal(t, "reader", preview.Executions[0].Role)
			assert.True(t, filepath.IsAbs(preview.Executions[0].Program))
			assert.True(t, filepath.IsAbs(preview.Executions[0].Dir))
			require.NotEmpty(t, preview.Executions[0].Fingerprints)
			assert.Equal(t, preview.Executions[0].Program, preview.Executions[0].Fingerprints[0].Path)
			assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, preview.Executions[0].Fingerprints[0].Digest)
			assert.Equal(t, "effect", preview.Executions[1].ObserverID)
			_, err = os.Stat(home)
			assert.True(t, os.IsNotExist(err))
			in.PreviewDigest, in.Confirmed = preview.PreviewDigest, true
			switch tc.change {
			case "definition":
				in.Definition = []byte(strings.Replace(string(in.Definition), `"enabled":true`, `"enabled":false`, 1))
			case "confirmation":
				in.Confirmed = false
			case "revision":
				revision := int64(1)
				in.ExpectedRevision = &revision
			}
			_, err = a.SourceSave(context.Background(), in)
			assert.Equal(t, tc.invalid, err != nil)
			sources, listErr := a.SourceList(context.Background(), true)
			require.NoError(t, listErr)
			count := 1
			if tc.invalid {
				count = 0
			}
			assert.Len(t, sources, count)
			public, err := json.Marshal(sources)
			require.NoError(t, err)
			assert.NotContains(t, string(public), "program")
			assert.NotContains(t, string(public), "fingerprints")
			assert.NotContains(t, string(public), "args")
		})
	}
}

func TestSourceRevisionAndRemoval(t *testing.T) {
	t.Parallel()
	a := app.App{Home: filepath.Join(t.TempDir(), "home"), Now: time.Now, Warn: io.Discard}
	ctx := context.Background()
	in := observeSourceRequest(t)
	preview, err := a.SourcePreview(ctx, in)
	require.NoError(t, err)
	in.PreviewDigest, in.Confirmed = preview.PreviewDigest, true
	first, err := a.SourceSave(ctx, in)
	require.NoError(t, err)
	require.NotNil(t, first.Source)
	assert.Equal(t, int64(1), first.Source.Observers[0].Revision)
	revision := first.Source.Revision
	in.ExpectedRevision = &revision
	in.Definition = []byte(strings.Replace(string(in.Definition), `"criterion_revision":"v1"`, `"criterion_revision":"v2"`, 1))
	preview, err = a.SourcePreview(ctx, in)
	require.NoError(t, err)
	in.PreviewDigest = preview.PreviewDigest
	second, err := a.SourceSave(ctx, in)
	require.NoError(t, err)
	require.NotNil(t, second.Source)
	assert.Equal(t, int64(2), second.Source.Revision)
	assert.Equal(t, int64(2), second.Source.Observers[0].Revision)
	assert.Equal(t, first.Source.CollectionIdentity, second.Source.CollectionIdentity)
	removed, err := a.SourceRemove(ctx, app.SourceRemoveRequest{ID: "acme", ExpectedRevision: 2, Confirmed: true})
	require.NoError(t, err)
	assert.True(t, removed.Removed)
	sources, err := a.SourceList(ctx, false)
	require.NoError(t, err)
	assert.Empty(t, sources)
	all, err := a.SourceList(ctx, true)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.True(t, all[0].Removed)
	revision = 3
	_, err = a.SourcePreview(ctx, in)
	require.Error(t, err)
	assert.Equal(t, "source_removed", err.Error())
}

func TestSourceStoreRecovery(t *testing.T) {
	t.Parallel()
	a := app.App{Home: filepath.Join(t.TempDir(), "home"), Now: time.Now, Warn: io.Discard}
	ctx := context.Background()
	in := observeSourceRequest(t)
	preview, err := a.SourcePreview(ctx, in)
	require.NoError(t, err)
	in.PreviewDigest, in.Confirmed = preview.PreviewDigest, true
	_, err = a.SourceSave(ctx, in)
	require.NoError(t, err)
	path := filepath.Join(a.Home, "observe", "ledger.jsonl")
	old, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(old, []byte("broken\n")...), 0600))
	recovery := app.SourceRequest{Operation: "recover_store", Recovery: &app.SourceRecovery{Mode: "new_store"}}
	preview, err = a.SourcePreview(ctx, recovery)
	require.NoError(t, err)
	recovery.PreviewDigest, recovery.ExpectedStoreToken, recovery.Confirmed = preview.PreviewDigest, preview.ExpectedStoreToken, true
	saved, err := a.SourceSave(ctx, recovery)
	require.NoError(t, err)
	require.NotNil(t, saved.Recovery)
	assert.Equal(t, "complete", saved.Recovery.Stage)
	sources, err := a.SourceList(ctx, true)
	require.NoError(t, err)
	assert.Empty(t, sources)
	quarantines, err := filepath.Glob(filepath.Join(a.Home, "observe-quarantine", "store-*", "ledger.jsonl"))
	require.NoError(t, err)
	require.Len(t, quarantines, 1)
	kept, err := os.ReadFile(quarantines[0])
	require.NoError(t, err)
	assert.Equal(t, append(old, []byte("broken\n")...), kept)
}
