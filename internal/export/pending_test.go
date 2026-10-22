package export_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/jeon-jihyeon/rindcut/internal/export"
	"github.com/jeon-jihyeon/rindcut/internal/export/mocks"
)

func TestRunnerPending(t *testing.T) {
	t.Parallel()
	type want struct {
		pending, dropped int
		failed           bool
	}
	tcs := []struct {
		name, pending string
		err           error
		want          want
	}{
		{name: "missing ledger entry stays visible", pending: "missing", want: want{pending: 1}},
		{name: "temporary retry failure", pending: "v000", err: errors.New("backend down"),
			want: want{pending: 1, failed: true}},
		{name: "permanent retry refusal", pending: "v000", err: export.PermanentError{Err: errors.New("refused")},
			want: want{dropped: 1}},
		{name: "correlation still missing", pending: "v000", err: export.ErrDeferred, want: want{pending: 1}},
		{name: "retry succeeds", pending: "v000"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			b, err := json.Marshal(export.Cursor{Last: "v001", Pending: []string{tc.pending}})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "x.json"), b, 0o600))
			ex := mocks.NewMockExporter(gomock.NewController(t))
			ex.EXPECT().Name().Return("x").AnyTimes()
			ex.EXPECT().Export(gomock.Any(), gomock.Any()).Return(tc.err).AnyTimes()
			r := export.Runner{Exporters: []export.Exporter{ex}, CursorDir: dir, Now: func() time.Time { return t0 }}
			lags := r.Run(context.Background(), verdicts(t, 2))
			require.Len(t, lags, 1)
			assert.Equal(t, tc.want, want{pending: lags[0].Pending, dropped: lags[0].Dropped,
				failed: lags[0].LastError != ""})
			b, err = os.ReadFile(filepath.Join(dir, "x.json"))
			require.NoError(t, err)
			var c export.Cursor
			require.NoError(t, json.Unmarshal(b, &c))
			assert.Equal(t, "v001", c.Last)
			assert.Len(t, c.Pending, tc.want.pending)
		})
	}
}

func TestRunnerCursorWriteFailure(t *testing.T) {
	t.Parallel()
	tcs := []struct{ name string }{
		{name: "parent"},
		{name: "temporary file"},
		{name: "rename"},
		{name: "timestamp encoding"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ex := mocks.NewMockExporter(gomock.NewController(t))
			ex.EXPECT().Name().Return("x").AnyTimes()
			ex.EXPECT().Export(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			r := export.Runner{Exporters: []export.Exporter{ex}, CursorDir: dir, Now: func() time.Time { return t0 }}
			switch tc.name {
			case "parent":
				r.CursorDir = filepath.Join(dir, "file")
				require.NoError(t, os.WriteFile(r.CursorDir, nil, 0o600))
			case "temporary file":
				require.NoError(t, os.Mkdir(filepath.Join(dir, "x.json.tmp"), 0o700))
			case "rename":
				require.NoError(t, os.Mkdir(filepath.Join(dir, "x.json"), 0o700))
			case "timestamp encoding":
				ex = mocks.NewMockExporter(gomock.NewController(t))
				ex.EXPECT().Name().Return("x").AnyTimes()
				ex.EXPECT().Export(gomock.Any(), gomock.Any()).Return(errors.New("backend down"))
				r.Exporters = []export.Exporter{ex}
				r.Now = func() time.Time { return time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }
			}
			lags := r.Run(context.Background(), verdicts(t, 1))
			require.Len(t, lags, 1)
			assert.NotEmpty(t, lags[0].LastError)
			assert.NotNil(t, lags[0].FailedAt)
		})
	}
}

func TestRunnerReplacedLedger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b, err := json.Marshal(export.Cursor{Last: "old-entry"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.json"), b, 0o600))
	es := verdicts(t, 2)
	ex := mocks.NewMockExporter(gomock.NewController(t))
	ex.EXPECT().Name().Return("x").AnyTimes()
	ex.EXPECT().Export(gomock.Any(), es[:2]).Return(nil)
	r := export.Runner{Exporters: []export.Exporter{ex}, CursorDir: dir, Now: func() time.Time { return t0 }}
	assert.Equal(t, []export.Lag{{Exporter: "x"}}, r.Run(context.Background(), es))
}

func TestRunnerPendingFailureSurvivesSuccess(t *testing.T) {
	t.Parallel()
	es := verdicts(t, 2)
	ex := mocks.NewMockExporter(gomock.NewController(t))
	ex.EXPECT().Name().Return("x").AnyTimes()
	failure := fmt.Errorf("%w: outcome exit 1", export.ErrDeferred)
	gomock.InOrder(
		ex.EXPECT().Export(gomock.Any(), es[:2]).Return(failure),
		ex.EXPECT().Export(gomock.Any(), es[:1]).Return(failure),
		ex.EXPECT().Export(gomock.Any(), es[1:2]).Return(nil),
		ex.EXPECT().Export(gomock.Any(), es[:1]).Return(nil),
	)
	r := export.Runner{Exporters: []export.Exporter{ex}, CursorDir: t.TempDir(), Now: func() time.Time { return t0 }}
	assert.Equal(t, []export.Lag{{Exporter: "x", Pending: 1, LastError: failure.Error(), FailedAt: &t0}},
		r.Run(context.Background(), es))
	assert.Equal(t, []export.Lag{{Exporter: "x"}}, r.Run(context.Background(), es))
}
