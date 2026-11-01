package observe

import (
	"slices"
	"time"
)

type Due struct {
	SeriesID string
	At       time.Time
}

type Work struct {
	Kind string
	ID   string
}

type Rotation struct {
	NextLane      string
	AfterSourceID string
}

func (r Rotation) Next(sourceIDs []string, due []Due, cutoff time.Time) (Work, Rotation, bool) {
	sources := slices.Clone(sourceIDs)
	slices.Sort(sources)
	due = slices.Clone(due)
	slices.SortFunc(due, func(a, b Due) int {
		if n := a.At.Compare(b.At); n != 0 {
			return n
		}
		if a.SeriesID < b.SeriesID {
			return -1
		}
		if a.SeriesID > b.SeriesID {
			return 1
		}
		return 0
	})
	if len(due) > 0 && due[0].At.After(cutoff) {
		due = nil
	}
	if len(sources) == 0 && len(due) == 0 {
		return Work{}, r, false
	}
	if len(due) > 0 && (r.NextLane == "observation" || len(sources) == 0) {
		r.NextLane = "source"
		return Work{Kind: "observation", ID: due[0].SeriesID}, r, true
	}
	chosen := sources[0]
	for _, id := range sources {
		if id > r.AfterSourceID {
			chosen = id
			break
		}
	}
	r.AfterSourceID = chosen
	r.NextLane = "observation"
	return Work{Kind: "source", ID: chosen}, r, true
}
