package export_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/jeon-jihyeon/rindcut/internal/export"
	"github.com/jeon-jihyeon/rindcut/internal/export/mocks"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

var t0 = time.Date(2026, 10, 8, 0, 40, 0, 0, time.UTC)

func entry(t *testing.T, id string, kind ledger.Kind, data any) ledger.Entry {
	t.Helper()
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	return ledger.Entry{ID: id, Kind: kind, At: t0, Version: "test", Data: raw}
}

func verdict(t *testing.T, id string, v ledger.Verdict, sessions ...string) ledger.Entry {
	t.Helper()
	return entry(t, id, ledger.KindVerdict, ledger.Judgment{
		JobID: "price-watch", JobRevision: 2, SlotKey: "price-watch@2026-10-08T09:00:00+09:00",
		Slot: t0.Add(-40 * time.Minute), WindowEnd: t0.Add(-10 * time.Minute), Verdict: v, Sessions: sessions, Trigger: "tick",
		Checks: []ledger.CheckRecord{{Kind: "file", Status: string(v), Message: "no file", Observed: map[string]any{"exists": false, "path": "/x"}}},
	})
}

func verdicts(t *testing.T, n int) []ledger.Entry {
	t.Helper()
	out := make([]ledger.Entry, 0, n+1)
	for i := range n {
		out = append(out, verdict(t, fmt.Sprintf("v%03d", i), ledger.VerdictPass))
	}
	// Sessions never leave the machine, so they are never sent and never counted
	return append(out, entry(t, "s1", ledger.KindSession, ledger.Session{SessionID: "s"}))
}

func ids(entries []ledger.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func TestRunnerBatches(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ex := mocks.NewMockExporter(ctrl)
	ex.EXPECT().Name().Return("ok").AnyTimes()
	var sizes []int
	ex.EXPECT().Export(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, es []ledger.Entry) error {
		sizes = append(sizes, len(es))
		return nil
	}).Times(2)
	r := export.Runner{Exporters: []export.Exporter{ex}, CursorDir: t.TempDir(), Now: func() time.Time { return t0 }}

	lags := r.Run(context.Background(), verdicts(t, 150))
	assert.Equal(t, []int{100, 50}, sizes)
	assert.Equal(t, []export.Lag{{Exporter: "ok"}}, lags)
}

func TestRunnerFailures(t *testing.T) {
	t.Parallel()
	transient := errors.New("backend down")
	refused := export.PermanentError{Err: errors.New("400 bad trace")}
	type want struct {
		sent []string
		lag  export.Lag
	}
	tcs := []struct {
		name string
		// fail answers one Export call with the ids it got
		fail func(ids []string) error
		want want
	}{
		{
			name: "transient failure keeps everything pending",
			fail: func([]string) error { return transient },
			want: want{lag: export.Lag{Exporter: "x", Pending: 3, LastError: "backend down", FailedAt: &t0}},
		},
		{
			name: "a permanent refusal drops only the refused entry",
			fail: func(ids []string) error {
				for _, id := range ids {
					if id == "v001" {
						return refused
					}
				}
				return nil
			},
			want: want{sent: []string{"v000", "v002"}, lag: export.Lag{Exporter: "x", Dropped: 1, LastDropped: "v001: 400 bad trace"}},
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			ex := mocks.NewMockExporter(ctrl)
			ex.EXPECT().Name().Return("x").AnyTimes()
			var sent []string
			ex.EXPECT().Export(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, es []ledger.Entry) error {
				if err := tc.fail(ids(es)); err != nil {
					return err
				}
				if len(es) == 1 {
					sent = append(sent, es[0].ID)
				}
				return nil
			}).AnyTimes()
			r := export.Runner{Exporters: []export.Exporter{ex}, CursorDir: t.TempDir(), Now: func() time.Time { return t0 }}
			lags := r.Run(context.Background(), verdicts(t, 3))
			require.Len(t, lags, 1)
			assert.Equal(t, tc.want, want{sent: sent, lag: lags[0]})
		})
	}
}

func TestRunnerCursorAndReset(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ex := mocks.NewMockExporter(ctrl)
	ex.EXPECT().Name().Return("ok").AnyTimes()
	var batches [][]string
	ex.EXPECT().Export(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, es []ledger.Entry) error {
		batches = append(batches, ids(es))
		return nil
	}).AnyTimes()
	dir := t.TempDir()
	r := export.Runner{Exporters: []export.Exporter{ex}, CursorDir: dir, Now: func() time.Time { return t0 }}
	entries := verdicts(t, 2)
	r.Run(context.Background(), entries)

	more := append(append([]ledger.Entry(nil), entries...), verdict(t, "zz", ledger.VerdictFail))
	assert.Equal(t, []export.Lag{{Exporter: "ok", Pending: 1}}, export.Lags(dir, []string{"ok"}, more))
	r.Run(context.Background(), more)
	assert.Equal(t, []string{"zz"}, batches[len(batches)-1], "a second run sends only what's new")

	require.NoError(t, export.Reset(dir, "ok"))
	require.NoError(t, export.Reset(dir, "never-ran"))
	assert.Equal(t, []export.Lag{{Exporter: "ok", Pending: 3}}, export.Lags(dir, []string{"ok"}, more))
}

func TestCarried(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		kind ledger.Kind
		want bool
	}{
		{kind: ledger.KindVerdict, want: true},
		{kind: ledger.KindCorrection, want: true},
		{kind: ledger.KindSession, want: false},
		{kind: ledger.KindProbe, want: false},
		{kind: ledger.KindJob, want: false},
	}
	for _, tc := range tcs {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, export.Carried(tc.kind))
		})
	}
}
