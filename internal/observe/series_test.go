package observe_test

import (
	"testing"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/observe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seriesSpec() observe.SeriesSpec {
	return observe.SeriesSpec{ID: "s1", StoreID: "store", SourceID: "acme", SourceRevision: 2, CaptureSourceRevision: 1,
		CaptureID: "c1", ObserverID: "effect", ObserverRevision: 1, CriterionID: "landed", CriterionRevision: "v1",
		ParametersDigest: "p1", ActorKind: "model", Mode: "until_terminal", Interval: time.Minute,
		Deadline: time.Hour, CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func TestSeriesApply(t *testing.T) {
	tcs := []struct {
		name   string
		result string
		mode   string
		due    bool
	}{
		{name: "pending", result: "pending", mode: "until_terminal", due: true},
		{name: "unknown", result: "unknown", mode: "until_terminal", due: true},
		{name: "confirmed", result: "confirmed", mode: "until_terminal"},
		{name: "refuted", result: "refuted", mode: "until_terminal"},
		{name: "watch_confirmed", result: "confirmed", mode: "watch", due: true},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := seriesSpec()
			spec.Mode = tc.mode
			s, err := observe.NewSeries(spec)
			require.NoError(t, err)
			at := spec.CapturedAt.Add(time.Minute)
			next, err := s.Apply(observe.Observation{ID: "o1", StoreID: "store", CaptureID: "c1", SeriesID: "s1",
				CriterionID: "landed", CriterionRevision: "v1", Result: tc.result, At: at, NextAt: at.Add(2 * time.Minute)})
			require.NoError(t, err)
			assert.False(t, next.Due(at.Add(time.Minute)))
			assert.Equal(t, tc.due, next.Due(at.Add(2*time.Minute)))
			assert.True(t, next.Overdue(at.Add(2*time.Hour)))
			assert.Equal(t, tc.result, next.Snapshot().Status)
			assert.Equal(t, int64(1), next.Snapshot().Spec.CaptureSourceRevision)
			assert.Equal(t, "model", next.Snapshot().Spec.ActorKind)
			assert.False(t, next.Stop(false).Due(at.Add(3*time.Hour)))
			assert.Equal(t, "cancelled_configuration", next.Stop(true).Snapshot().Status)
			assert.Equal(t, "pending", s.Snapshot().Status)
		})
	}
}

func TestSeriesSupersedes(t *testing.T) {
	tcs := []struct {
		name    string
		ref     string
		invalid bool
	}{{name: "known", ref: "o1"}, {name: "missing", ref: "other", invalid: true}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := seriesSpec()
			spec.Mode = "watch"
			s, err := observe.NewSeries(spec)
			require.NoError(t, err)
			o := observe.Observation{ID: "o1", StoreID: "store", CaptureID: "c1", SeriesID: "s1", CriterionID: "landed",
				CriterionRevision: "v1", Result: "confirmed", At: spec.CapturedAt}
			s, err = s.Apply(o)
			require.NoError(t, err)
			o.ID = "o2"
			o.At = o.At.Add(time.Minute)
			o.Result = "refuted"
			o.Supersedes = tc.ref
			_, err = s.Apply(o)
			assert.Equal(t, tc.invalid, err != nil)
		})
	}
}
