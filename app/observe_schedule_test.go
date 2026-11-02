package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jeon-jihyeon/rindcut/app"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSourceSchedule(t *testing.T) {
	tcs := []struct {
		name      string
		disabled  bool
		confirmed bool
		changed   bool
		refuse    bool
	}{
		{name: "source_without_jobs", confirmed: true},
		{name: "disabled", disabled: true, confirmed: true},
		{name: "unconfirmed"},
		{name: "schedule_target_changed", confirmed: true, changed: true},
		{name: "schedule_failure_preserves_registration", confirmed: true, refuse: true},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			var calls atomic.Int32
			run := w.a.Installer.Run
			w.a.Installer.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				calls.Add(1)
				return run(ctx, name, args...)
			}
			request := observeSourceRequest(t)
			if tc.disabled {
				request.Definition = []byte(strings.Replace(string(request.Definition), `"enabled":true`, `"enabled":false`, 1))
			}
			preview, err := w.a.SourcePreview(t.Context(), request)
			require.NoError(t, err)
			assert.Zero(t, calls.Load())
			assert.Equal(t, !tc.disabled, preview.Schedule != nil)
			if !tc.disabled {
				assert.Contains(t, preview.Effects, "tick_schedule_ensure")
				assert.Equal(t, 300, preview.Schedule.IntervalSeconds)
			}
			request.PreviewDigest, request.Confirmed = preview.PreviewDigest, tc.confirmed
			if tc.changed {
				w.a.Installer.Home = filepath.Join(t.TempDir(), "changed")
			}
			*w.refuse = tc.refuse
			saved, err := w.a.SourceSave(t.Context(), request)
			valid := tc.confirmed && !tc.changed
			assert.Equal(t, !valid, err != nil)
			sources, listErr := w.a.SourceList(t.Context(), false)
			require.NoError(t, listErr)
			count := 0
			if valid {
				count = 1
			}
			assert.Len(t, sources, count)
			if !valid || tc.disabled {
				assert.Zero(t, calls.Load())
				assert.Nil(t, saved.Schedule)
				return
			}
			require.NotNil(t, saved.Source)
			require.NotNil(t, saved.Schedule)
			assert.Equal(t, int64(1), saved.Source.Revision)
			if !tc.refuse {
				assert.Equal(t, "ready", saved.Schedule.State)
				return
			}
			assert.NotEqual(t, "ready", saved.Schedule.State)
			assert.NotEmpty(t, saved.Schedule.Reason)
			path := filepath.Join(w.a.Home, "observe", "ledger.jsonl")
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			*w.refuse = false
			require.NoError(t, w.a.Install(t.Context()))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestSourceAndJobsShareSchedule(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var starts atomic.Int32
	run := w.a.Installer.Run
	w.a.Installer.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if args[0] == "bootstrap" {
			starts.Add(1)
		}
		return run(ctx, name, args...)
	}
	j := w.saved(t, w.job())
	request := observeSourceRequest(t)
	preview, err := w.a.SourcePreview(t.Context(), request)
	require.NoError(t, err)
	request.PreviewDigest, request.Confirmed = preview.PreviewDigest, true
	saved, err := w.a.SourceSave(t.Context(), request)
	require.NoError(t, err)
	require.NotNil(t, saved.Schedule)
	assert.Equal(t, "ready", saved.Schedule.State)
	assert.Equal(t, int32(1), starts.Load())
	_, err = w.a.SourceRemove(t.Context(), app.SourceRemoveRequest{ID: "acme", ExpectedRevision: 1, Confirmed: true})
	require.NoError(t, err)
	status, err := w.a.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "ready", status.Schedule.State)
	assert.Equal(t, 1, status.Jobs)
	_, err = w.a.Remove(t.Context(), j.ID)
	require.NoError(t, err)
	status, err = w.a.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "ready", status.Schedule.State)
	assert.Zero(t, status.Jobs)
}

func TestTickContinuesObservationAfterJudgeFailure(t *testing.T) {
	t.Parallel()
	f := newObserveFixture(t)
	f.app.Warn = os.Stderr
	path := filepath.Join(f.app.Home, "ledger.jsonl")
	require.NoError(t, os.Mkdir(path, 0o700))
	result, err := f.app.Tick(t.Context())
	require.Error(t, err)
	require.NotNil(t, result.Observation)
	assert.Equal(t, 1, result.Observation.Captured)
	page, err := f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "event"})
	require.NoError(t, err)
	assert.Len(t, page.Records, 1)
}

func TestConcurrentSourceAndJobSchedule(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var starts atomic.Int32
	run := w.a.Installer.Run
	w.a.Installer.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if args[0] == "bootstrap" {
			starts.Add(1)
		}
		return run(ctx, name, args...)
	}
	request := observeSourceRequest(t)
	preview, err := w.a.SourcePreview(t.Context(), request)
	require.NoError(t, err)
	request.PreviewDigest, request.Confirmed = preview.PreviewDigest, true
	_, err = w.a.Preview(t.Context(), w.job())
	require.NoError(t, err)
	errors := make(chan error, 2)
	var group sync.WaitGroup
	group.Go(func() { _, err := w.a.SourceSave(t.Context(), request); errors <- err })
	group.Go(func() { _, err := w.a.Save(t.Context(), w.job()); errors <- err })
	group.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), starts.Load())
	sources, err := w.a.SourceList(t.Context(), false)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, int64(1), sources[0].Revision)
}
