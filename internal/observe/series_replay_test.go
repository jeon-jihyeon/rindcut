package observe_test

import (
	"testing"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/observe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSeriesApplyAll(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name       string
		mode       string
		first      string
		secondID   string
		secondAt   time.Duration
		secondNext time.Duration
		store      string
		result     string
		supersedes string
		wantStatus string
	}{
		{name: "late correction", mode: "watch", first: "confirmed", secondID: "o2", secondAt: time.Hour,
			store: "store", result: "refuted", supersedes: "o1", wantStatus: "refuted"},
		{name: "pending then confirmed", mode: "until_terminal", first: "pending", secondID: "o2",
			secondAt: time.Minute, store: "store", result: "confirmed", wantStatus: "confirmed"},
		{name: "duplicate", mode: "watch", first: "pending", secondID: "o1",
			secondAt: time.Minute, store: "store", result: "confirmed"},
		{name: "before due", mode: "watch", first: "pending", secondID: "o2",
			secondAt: time.Second, store: "store", result: "confirmed"},
		{name: "terminal", mode: "until_terminal", first: "confirmed", secondID: "o2",
			secondAt: time.Minute, store: "store", result: "refuted"},
		{name: "unknown correction", mode: "watch", first: "pending", secondID: "o2", secondAt: time.Minute,
			store: "store", result: "confirmed", supersedes: "missing"},
		{name: "foreign store", mode: "watch", first: "pending", secondID: "o2", secondAt: time.Minute,
			store: "foreign", result: "confirmed"},
		{name: "invalid result", mode: "watch", first: "pending", secondID: "o2", secondAt: time.Minute,
			store: "store", result: "approved"},
		{name: "next time in past", mode: "watch", first: "pending", secondID: "o2", secondAt: time.Minute,
			secondNext: time.Second, store: "store", result: "confirmed"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := seriesSpec()
			spec.Mode = tc.mode
			series, err := observe.NewSeries(spec)
			require.NoError(t, err)
			before := series.Snapshot()
			second := observe.Observation{ID: tc.secondID, StoreID: tc.store, CaptureID: "c1", SeriesID: "s1",
				CriterionID: "landed", CriterionRevision: "v1", Result: tc.result,
				At: spec.CapturedAt.Add(tc.secondAt), Supersedes: tc.supersedes}
			if tc.secondNext != 0 {
				second.NextAt = spec.CapturedAt.Add(tc.secondNext)
			}
			got, err := series.ApplyAll([]observe.Observation{
				{ID: "o1", StoreID: "store", CaptureID: "c1", SeriesID: "s1", CriterionID: "landed",
					CriterionRevision: "v1", Result: tc.first, At: spec.CapturedAt}, second,
			})
			assert.Equal(t, tc.wantStatus == "", err != nil)
			assert.Equal(t, tc.wantStatus, got.Snapshot().Status)
			assert.Equal(t, before, series.Snapshot())
		})
	}
}
