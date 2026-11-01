package observe_test

import (
	"testing"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/observe"
	"github.com/stretchr/testify/assert"
)

func TestRotation(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tcs := []struct {
		name     string
		rotation observe.Rotation
		sources  []string
		due      []observe.Due
		want     observe.Work
		exists   bool
	}{
		{name: "source_first", sources: []string{"b", "a"}, due: []observe.Due{{SeriesID: "s", At: now}}, want: observe.Work{Kind: "source", ID: "a"}, exists: true},
		{name: "rotate", rotation: observe.Rotation{AfterSourceID: "a"}, sources: []string{"b", "a"}, want: observe.Work{Kind: "source", ID: "b"}, exists: true},
		{name: "wrap", rotation: observe.Rotation{AfterSourceID: "b"}, sources: []string{"b", "a"}, want: observe.Work{Kind: "source", ID: "a"}, exists: true},
		{name: "alternate", rotation: observe.Rotation{NextLane: "observation"}, sources: []string{"a"}, due: []observe.Due{{SeriesID: "b", At: now}, {SeriesID: "a", At: now}}, want: observe.Work{Kind: "observation", ID: "a"}, exists: true},
		{name: "future", due: []observe.Due{{SeriesID: "a", At: now.Add(time.Second)}}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			work, _, ok := tc.rotation.Next(tc.sources, tc.due, now)
			assert.Equal(t, tc.exists, ok)
			assert.Equal(t, tc.want, work)
		})
	}
}
