package app_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jeon-jihyeon/rindcut/app"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserveResetExecutesOnlyConfirmedPlan(t *testing.T) {
	type args struct {
		generation string
		confirmed  bool
		stale      bool
		changed    bool
	}
	type want struct {
		err      string
		calls    int
		revision int64
	}
	tcs := []struct {
		name string
		args args
		want want
	}{
		{name: "confirmed", args: args{generation: "g1", confirmed: true}, want: want{err: "<nil>", calls: 1, revision: 2}},
		{name: "stale", args: args{generation: "g1", confirmed: true, stale: true},
			want: want{err: "stale_preview", revision: 1}},
		{name: "changed", args: args{generation: "g1", confirmed: true, changed: true},
			want: want{err: "stale_preview", revision: 1}},
		{name: "unconfirmed", args: args{generation: "g1"}, want: want{err: "confirmation_required", revision: 1}},
		{name: "generation mismatch", args: args{generation: "g2", confirmed: true},
			want: want{err: "stale_preview", calls: 1, revision: 1}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newObserveFixture(t)
			marker := filepath.Join(t.TempDir(), "reader-calls")
			reader := f.definition["reader"].(map[string]any)
			reader["args"] = append(reader["args"].([]string), "--observe-marker", marker)
			definition, err := json.Marshal(f.definition)
			require.NoError(t, err)
			revision := int64(1)
			request := app.SourceRequest{Definition: definition, ExpectedRevision: &revision}
			preview, err := f.app.SourcePreview(t.Context(), request)
			require.NoError(t, err)
			request.Recovery = &app.SourceRecovery{Mode: "reset", CheckpointToken: preview.CheckpointToken,
				CollectionIdentity: preview.CollectionIdentity, Generation: tc.args.generation}
			preview, err = f.app.SourcePreview(t.Context(), request)
			require.NoError(t, err)
			assert.NoFileExists(t, marker)
			request.PreviewDigest, request.Confirmed = preview.PreviewDigest, tc.args.confirmed
			if tc.args.stale {
				request.PreviewDigest = "stale"
			}
			if tc.args.changed {
				reader["args"] = append(reader["args"].([]string), "changed")
				request.Definition, err = json.Marshal(f.definition)
				require.NoError(t, err)
				changed := request
				changed.Recovery = nil
				fresh, previewErr := f.app.SourcePreview(t.Context(), changed)
				require.NoError(t, previewErr)
				request.Recovery.CollectionIdentity = fresh.CollectionIdentity
			}
			_, err = f.app.SourceSave(t.Context(), request)
			assert.Equal(t, tc.want.err, fmt.Sprint(err))
			calls, readErr := os.ReadFile(marker)
			assert.True(t, readErr == nil || errors.Is(readErr, os.ErrNotExist))
			assert.Equal(t, tc.want.calls, bytes.Count(calls, []byte("\n")))
			sources, err := f.app.SourceList(t.Context(), false)
			require.NoError(t, err)
			require.Len(t, sources, 1)
			assert.Equal(t, tc.want.revision, sources[0].Revision)
			page, err := f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "event"})
			require.NoError(t, err)
			assert.Empty(t, page.Records)
		})
	}
}
