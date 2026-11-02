package observe_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/observe"
)

func BenchmarkSeriesReplay(b *testing.B) {
	for _, count := range []int{1000, 2000, 4000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			spec := seriesSpec()
			spec.Mode = "watch"
			observations := make([]observe.Observation, count)
			for i := range observations {
				observations[i] = observe.Observation{ID: fmt.Sprint(i), StoreID: "store", CaptureID: "c1",
					SeriesID: "s1", CriterionID: "landed", CriterionRevision: "v1", Result: "confirmed",
					At: spec.CapturedAt.Add(time.Duration(i) * time.Minute)}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				series, err := observe.NewSeries(spec)
				if err != nil {
					b.Fatal(err)
				}
				if _, err = series.ApplyAll(observations); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
