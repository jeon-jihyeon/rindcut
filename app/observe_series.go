package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/observe"
	"github.com/jeon-jihyeon/rindcut/internal/observe/command"
)

func observeRestoreSeries(saved observeSeries, state observeState) (observe.Series, error) {
	ids := state.observationIDs[saved.ID]
	return observe.RestoreSeries(observe.SeriesState{Spec: observe.SeriesSpec{ID: saved.ID,
		StoreID: saved.StoreID, SourceID: saved.SourceID, SourceRevision: saved.SourceRevision,
		CaptureSourceRevision: saved.CaptureSourceRevision, CaptureID: saved.CaptureID,
		ObserverID: saved.ObserverID, ObserverRevision: saved.ObserverRevision, CriterionID: saved.CriterionID,
		CriterionRevision: saved.CriterionRevision, ParametersDigest: saved.ParametersDigest, ActorKind: saved.ActorKind,
		Mode: saved.Mode, Interval: time.Duration(saved.IntervalSeconds) * time.Second,
		Deadline: time.Duration(saved.DeadlineSeconds) * time.Second, CapturedAt: saved.CapturedAt},
		Status: saved.Status, DueAt: saved.DueAt, LastObservationID: saved.LastObservationID, Observations: ids})
}

func observeSaveSeries(model observe.Series) observeSeries {
	state := model.Snapshot()
	spec := state.Spec
	return observeSeries{ID: spec.ID, StoreID: spec.StoreID, SourceID: spec.SourceID,
		SourceRevision: spec.SourceRevision, CaptureSourceRevision: spec.CaptureSourceRevision,
		CaptureID: spec.CaptureID, ObserverID: spec.ObserverID, ObserverRevision: spec.ObserverRevision,
		CriterionID: spec.CriterionID, CriterionRevision: spec.CriterionRevision, ParametersDigest: spec.ParametersDigest,
		ActorKind: spec.ActorKind, Mode: spec.Mode, IntervalSeconds: int(spec.Interval / time.Second),
		DeadlineSeconds: int(spec.Deadline / time.Second), CapturedAt: spec.CapturedAt,
		Status: state.Status, DueAt: state.DueAt, LastObservationID: state.LastObservationID}
}

func (s *observeState) acceptSeries(saved observeSeries) error {
	if err := s.validateSeriesSpec(saved); err != nil {
		return err
	}
	model, err := observeRestoreSeries(saved, *s)
	if err != nil {
		return err
	}
	if prior, exists := s.models[saved.ID]; exists && prior.Snapshot().Spec != model.Snapshot().Spec {
		return errors.New("invalid_series")
	}
	key := observeSeriesKey{saved.CaptureID, saved.SourceRevision, saved.ObserverID, saved.ObserverRevision}
	if id, exists := s.seriesKeys[key]; exists && id != saved.ID {
		return errors.New("invalid_series")
	}
	s.series[saved.ID] = saved
	s.seriesKeys[key] = saved.ID
	s.models[saved.ID] = model
	return nil
}

func (s observeState) validateSeriesSpec(saved observeSeries) error {
	capture, exists := s.captures[saved.CaptureID]
	if !exists || saved.StoreID != s.snapshot.StoreID || saved.SourceID != capture.SourceID ||
		saved.CaptureSourceRevision != capture.SourceRevision {
		return errors.New("invalid_series")
	}
	capturedAt, err := time.Parse(time.RFC3339Nano, capture.CapturedAt)
	if err != nil || !saved.CapturedAt.Equal(capturedAt) {
		return errors.New("invalid_series")
	}
	source, exists := s.sourceRevisions[observeSourceKey{saved.SourceID, saved.SourceRevision}]
	if !exists || saved.ObserverRevision != source.ObserverRevisions[saved.ObserverID] {
		return errors.New("invalid_series")
	}
	for _, observer := range source.Definition.Observers {
		if observeSeriesMatchesDefinition(saved, observer) {
			return nil
		}
	}
	return errors.New("invalid_series")
}

func observeSeriesMatchesDefinition(saved observeSeries, observer command.Observer) bool {
	return observer.ID == saved.ObserverID && saved.CriterionID == observer.CriterionID &&
		saved.CriterionRevision == observer.CriterionRevision && saved.ParametersDigest == observeHash(observer.Parameters) &&
		saved.ActorKind == observer.ActorKind && saved.Mode == observer.Mode &&
		saved.IntervalSeconds == observer.IntervalSeconds && saved.DeadlineSeconds == observer.DeadlineSeconds
}

func (s observeState) validateSeries(ctx context.Context, saved observeSeries) error {
	model, err := observeRestoreSeries(saved, s)
	if err != nil || saved.DueAt.IsZero() {
		return errors.New("invalid_series")
	}
	replayed, err := observe.NewSeries(model.Snapshot().Spec)
	if err != nil {
		return err
	}
	observations := make([]observe.Observation, 0, len(s.observationIDs[saved.ID]))
	for _, id := range s.observationIDs[saved.ID] {
		if err := ctx.Err(); err != nil {
			return err
		}
		observation := s.observations[id]
		value, err := observeObservationValue(observation)
		if err != nil {
			return err
		}
		observations = append(observations, value)
	}
	replayed, err = replayed.ApplyAll(observations)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	switch saved.Status {
	case "suspended_configuration":
		replayed = replayed.Stop(false)
	case "cancelled_configuration":
		replayed = replayed.Stop(true)
	}
	actual, expected := model.Snapshot(), replayed.Snapshot()
	if actual.Status != expected.Status || !actual.DueAt.Equal(expected.DueAt) ||
		actual.LastObservationID != expected.LastObservationID {
		return errors.New("invalid_series")
	}
	return nil
}

func observeObservationValue(observation observeObservation) (observe.Observation, error) {
	value := observe.Observation{ID: observation.ObservationID, StoreID: observation.TargetRef.StoreID,
		CaptureID: observation.TargetRef.CaptureID, SeriesID: observation.SeriesID,
		CriterionID: observation.CriterionID, CriterionRevision: observation.CriterionRevision,
		Result: observation.Result, At: observation.ObservedAt}
	if observation.NextObserveAt != nil {
		next, err := time.Parse(time.RFC3339Nano, *observation.NextObserveAt)
		if err != nil {
			return observe.Observation{}, errors.New("invalid_observation")
		}
		value.NextAt = next
	}
	if observation.Supersedes != nil {
		value.Supersedes = observation.Supersedes.ObservationID
	}
	return value, nil
}

func observeObservationMatches(observation observeObservation, series observeSeries) bool {
	return observation.Protocol == command.ObserverProtocol && observation.SourceID == series.SourceID &&
		observation.SourceRevision == series.SourceRevision && observation.ObserverID == series.ObserverID &&
		observation.ObserverRevision == series.ObserverRevision && observation.CriterionID == series.CriterionID &&
		observation.CriterionRevision == series.CriterionRevision && observation.TargetRef.StoreID == series.StoreID &&
		observation.TargetRef.CaptureID == series.CaptureID && observation.CaptureSourceRevision == series.CaptureSourceRevision &&
		observation.ActorKind == series.ActorKind
}

func (s observeState) validObservationPayload(observation observeObservation, series observeSeries) bool {
	capture := s.captures[series.CaptureID]
	if observation.Type != capture.Type || !observeOptionalEqual(observation.ActionID, capture.ActionID) {
		return false
	}
	source := s.sourceRevisions[observeSourceKey{series.SourceID, series.SourceRevision}]
	var parameters map[string]any
	for _, observer := range source.Definition.Observers {
		if observer.ID == series.ObserverID {
			parameters = observer.Parameters
			break
		}
	}
	request := command.ObserveRequest{Protocol: command.ObserverProtocol, SourceID: series.SourceID,
		SourceRevision: series.SourceRevision, ObserverID: series.ObserverID, ObserverRevision: series.ObserverRevision,
		CriterionID: series.CriterionID, CriterionRevision: series.CriterionRevision, SeriesID: series.ID,
		Parameters: parameters, TargetRef: observation.TargetRef, Event: capture, Now: command.Timestamp(observation.ObservedAt)}
	data, err := json.Marshal(observation.ObservationResponse)
	if err != nil {
		return false
	}
	_, err = command.DecodeObservation(data, request)
	return err == nil
}

func observeOptionalEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (s observeState) sourceCounts(status *ObserveSourceStatus, id string, now time.Time) {
	for identity, projections := range s.projections {
		if identity.SourceID == id && len(projections) > 1 {
			status.Conflicts += len(projections) - 1
		}
	}
	for _, saved := range s.series {
		if saved.SourceID != id {
			continue
		}
		model := s.models[saved.ID]
		if saved.Status == "pending" || saved.Status == "unknown" {
			status.Pending++
		}
		if model.Active() && model.Overdue(now) {
			status.Overdue++
		}
	}
	status.LastReasonCode = s.lastReason[id]
	if at, ok := s.lastAttempt[id]; ok {
		status.LastAttemptAt = &at
	}
	cp := s.checkpoints[id]
	status.LastSuccessAt = cp.LastSuccessAt
	if cp.LastSuccessAt != nil {
		status.Coverage = &ObserveCoverage{Kind: cp.Coverage.Kind,
			Limitations: cp.Coverage.Limitations, WindowStart: cp.Coverage.WindowStart,
			WindowEnd: cp.Coverage.WindowEnd, ReportedBy: "source"}
	}
}
