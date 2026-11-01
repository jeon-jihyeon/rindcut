package observe

import (
	"errors"
	"slices"
	"time"
)

type SeriesSpec struct {
	ID                    string
	StoreID               string
	SourceID              string
	SourceRevision        int64
	CaptureSourceRevision int64
	CaptureID             string
	ObserverID            string
	ObserverRevision      int64
	CriterionID           string
	CriterionRevision     string
	ParametersDigest      string
	ActorKind             string
	Mode                  string
	Interval              time.Duration
	Deadline              time.Duration
	CapturedAt            time.Time
}

type SeriesState struct {
	Spec              SeriesSpec
	Status            string
	DueAt             time.Time
	LastObservationID string
	Observations      []string
}

type Observation struct {
	ID                string
	StoreID           string
	CaptureID         string
	SeriesID          string
	CriterionID       string
	CriterionRevision string
	Result            string
	At                time.Time
	NextAt            time.Time
	Supersedes        string
}

type Series struct{ state SeriesState }

func NewSeries(spec SeriesSpec) (Series, error) {
	return RestoreSeries(SeriesState{Spec: spec, Status: "pending", DueAt: spec.CapturedAt, Observations: []string{}})
}

func RestoreSeries(state SeriesState) (Series, error) {
	s := state.Spec
	if !s.validIdentity() || !s.validPolicy() {
		return Series{}, errors.New("invalid_series")
	}
	switch state.Status {
	case "pending", "unknown", "confirmed", "refuted", "suspended_configuration", "cancelled_configuration":
	default:
		return Series{}, errors.New("invalid_series")
	}
	state.Observations = slices.Clone(state.Observations)
	return Series{state: state}, nil
}

func (s SeriesSpec) validIdentity() bool {
	for _, id := range []string{s.ID, s.StoreID, s.SourceID, s.CaptureID, s.ObserverID, s.CriterionID,
		s.CriterionRevision, s.ParametersDigest} {
		if id == "" {
			return false
		}
	}
	return s.SourceRevision > 0 && s.CaptureSourceRevision > 0 && s.ObserverRevision > 0
}

func (s SeriesSpec) validPolicy() bool {
	return s.Interval >= time.Second && s.Deadline >= time.Second && s.Interval <= 365*24*time.Hour &&
		s.Deadline <= 365*24*time.Hour && !s.CapturedAt.IsZero() &&
		(s.Mode == "until_terminal" || s.Mode == "watch") && (s.ActorKind == "code" || s.ActorKind == "model")
}

func (s Series) Snapshot() SeriesState {
	state := s.state
	state.Observations = slices.Clone(state.Observations)
	return state
}

func (s Series) Due(cutoff time.Time) bool {
	return s.Active() && !s.state.DueAt.IsZero() && !s.state.DueAt.After(cutoff)
}

func (s Series) Active() bool {
	status := s.state.Status
	if status == "suspended_configuration" || status == "cancelled_configuration" {
		return false
	}
	if s.state.Spec.Mode == "until_terminal" && (status == "confirmed" || status == "refuted") {
		return false
	}
	return true
}

func (s Series) Overdue(now time.Time) bool {
	return now.After(s.state.Spec.CapturedAt.Add(s.state.Spec.Deadline))
}

func (s Series) Apply(observation Observation) (Series, error) {
	spec := s.state.Spec
	if !s.Due(observation.At) || observation.ID == "" || slices.Contains(s.state.Observations, observation.ID) ||
		!observation.matches(spec) || observation.At.Before(spec.CapturedAt) {
		return Series{}, errors.New("invalid_observation")
	}
	switch observation.Result {
	case "pending", "unknown", "confirmed", "refuted":
	default:
		return Series{}, errors.New("invalid_observation")
	}
	if !observation.NextAt.IsZero() && !observation.NextAt.After(observation.At) {
		return Series{}, errors.New("invalid_observation")
	}
	if observation.Supersedes != "" && !slices.Contains(s.state.Observations, observation.Supersedes) {
		return Series{}, errors.New("invalid_supersedes")
	}
	state := s.Snapshot()
	state.Status = observation.Result
	state.LastObservationID = observation.ID
	state.Observations = append(state.Observations, observation.ID)
	state.DueAt = observation.At.Add(spec.Interval)
	if observation.NextAt.After(state.DueAt) {
		state.DueAt = observation.NextAt
	}
	return Series{state: state}, nil
}

func (o Observation) matches(s SeriesSpec) bool {
	return o.StoreID == s.StoreID && o.CaptureID == s.CaptureID && o.SeriesID == s.ID &&
		o.CriterionID == s.CriterionID && o.CriterionRevision == s.CriterionRevision
}

func (s Series) Stop(removed bool) Series {
	state := s.Snapshot()
	state.Status = "suspended_configuration"
	if removed {
		state.Status = "cancelled_configuration"
	}
	return Series{state: state}
}
