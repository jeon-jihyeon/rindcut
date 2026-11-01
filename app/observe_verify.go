package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/observe"
	"github.com/jeon-jihyeon/rindcut/internal/observe/command"
	obsstore "github.com/jeon-jihyeon/rindcut/internal/observe/store"
)

type observeExecution struct {
	Request command.ObserveRequest
	Program command.Prepared
	Timeout time.Duration
}

func (a App) observeVerify(ctx context.Context, store *obsstore.Store, state *observeState,
	seriesID, cycleID string) (observeWorkResult, error) {
	saved, exists := state.series[seriesID]
	if !exists {
		return observeWorkResult{}, errors.New("store_corrupt")
	}
	model, err := observeRestoreSeries(saved, *state)
	if err != nil {
		return observeWorkResult{}, errors.New("store_corrupt")
	}
	if !model.Due(a.observeNow()) || !observeSeriesActive(saved, *state) {
		return observeWorkResult{}, nil
	}
	execution, err := observeExecutionFor(saved, *state, a.observeNow())
	if err != nil {
		return observeWorkResult{}, err
	}
	input, err := json.Marshal(execution.Request)
	if err != nil {
		return observeWorkResult{}, errors.New("invalid_request")
	}
	attempt := observeAttempt{AttemptID: rand.Text(), CycleID: cycleID, SourceID: saved.SourceID, SeriesID: seriesID,
		Stage: "intent", SourceRevision: saved.SourceRevision, ObserverID: saved.ObserverID, ObserverRevision: saved.ObserverRevision}
	intent, err := observeRecord("cycle", a.observeNow(), attempt)
	if err != nil {
		return observeWorkResult{}, err
	}
	if err = appendObserve(ctx, store, state, []obsstore.Record{intent}); err != nil {
		return observeWorkResult{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, execution.Timeout)
	facts, callErr := command.Run(callCtx, execution.Program, input)
	cancel()
	response := observeUnknown(execution.Request, "invalid_response")
	reason := ""
	if callErr != nil {
		reason = observeCode(callErr)
		response.ReasonCode = reason
	} else {
		response, callErr = command.DecodeObservation(facts.Output, execution.Request)
		if callErr != nil {
			reason = observeCode(callErr)
			response = observeUnknown(execution.Request, reason)
		}
	}
	attempt.Stage, attempt.Started, attempt.Exited = "complete", facts.Started, facts.Exited
	records, err := observeVerificationRecords(saved, *state, response, attempt, a.observeNow())
	if err != nil {
		reason = "invalid_response"
		response = observeUnknown(execution.Request, reason)
		records, err = observeVerificationRecords(saved, *state, response, attempt, a.observeNow())
	}
	if err != nil {
		return observeWorkResult{}, err
	}
	if err = appendObserve(ctx, store, state, records); err != nil {
		return observeWorkResult{}, err
	}
	return observeWorkResult{Observations: 1, Reason: reason}, nil
}

func observeSeriesActive(saved observeSeries, state observeState) bool {
	current, exists := state.sources[saved.SourceID]
	_, registered := current.Observers[saved.ObserverID]
	return exists && registered && current.Definition.Enabled && !current.Removed &&
		current.ObserverRevisions[saved.ObserverID] == saved.ObserverRevision
}

func observeExecutionFor(saved observeSeries, state observeState, now time.Time) (observeExecution, error) {
	source, exists := state.sourceRevisions[observeSourceKey{SourceID: saved.SourceID, Revision: saved.SourceRevision}]
	if !exists {
		return observeExecution{}, errors.New("store_corrupt")
	}
	var observer command.Observer
	for _, candidate := range source.Definition.Observers {
		if candidate.ID == saved.ObserverID {
			observer = candidate
			break
		}
	}
	program, exists := source.Observers[saved.ObserverID]
	if !exists || source.ObserverRevisions[saved.ObserverID] != saved.ObserverRevision ||
		!observeSeriesMatchesDefinition(saved, observer) {
		return observeExecution{}, errors.New("store_corrupt")
	}
	capture, exists := state.captures[saved.CaptureID]
	if !exists || capture.SourceID != saved.SourceID || capture.SourceRevision != saved.CaptureSourceRevision {
		return observeExecution{}, errors.New("store_corrupt")
	}
	request := command.ObserveRequest{Protocol: command.ObserverProtocol, SourceID: saved.SourceID,
		SourceRevision: saved.SourceRevision, ObserverID: saved.ObserverID, ObserverRevision: saved.ObserverRevision,
		CriterionID: saved.CriterionID, CriterionRevision: saved.CriterionRevision, SeriesID: saved.ID,
		Parameters: observer.Parameters, TargetRef: command.TargetRef{StoreID: saved.StoreID, CaptureID: saved.CaptureID},
		Event: capture, Now: command.Timestamp(now)}
	if saved.LastObservationID != "" {
		previous := saved.LastObservationID
		request.PreviousObservationID = &previous
	}
	if err := request.Validate(); err != nil {
		return observeExecution{}, errors.New("store_corrupt")
	}
	return observeExecution{Request: request, Program: program, Timeout: time.Duration(observer.TimeoutSeconds) * time.Second}, nil
}

func observeUnknown(r command.ObserveRequest, reason string) command.ObservationResponse {
	return command.ObservationResponse{Protocol: command.ObserverProtocol, SourceID: r.SourceID, SourceRevision: r.SourceRevision,
		ObserverID: r.ObserverID, ObserverRevision: r.ObserverRevision, CriterionID: r.CriterionID, CriterionRevision: r.CriterionRevision,
		SeriesID: r.SeriesID, TargetRef: r.TargetRef, Result: "unknown", ReasonCode: reason, Evidence: []command.Evidence{}}
}

func observeVerificationRecords(saved observeSeries, state observeState, response command.ObservationResponse,
	attempt observeAttempt, at time.Time) ([]obsstore.Record, error) {
	model, err := observeRestoreSeries(saved, state)
	if err != nil {
		return nil, errors.New("store_corrupt")
	}
	if !observeValidSupersedes(saved, state, response.Supersedes) {
		return nil, errors.New("invalid_response")
	}
	id := rand.Text()
	observation := observe.Observation{ID: id, StoreID: saved.StoreID, CaptureID: saved.CaptureID, SeriesID: saved.ID,
		CriterionID: saved.CriterionID, CriterionRevision: saved.CriterionRevision, Result: response.Result, At: at}
	if response.NextObserveAt != nil {
		observation.NextAt, err = time.Parse(time.RFC3339Nano, *response.NextObserveAt)
		if err != nil {
			return nil, errors.New("invalid_response")
		}
	}
	if response.Supersedes != nil {
		observation.Supersedes = response.Supersedes.ObservationID
	}
	updated, err := model.Apply(observation)
	if err != nil {
		return nil, err
	}
	capture := state.captures[saved.CaptureID]
	public := observeObservation{ObservationResponse: response, ObservationID: id, ObservedAt: at,
		CaptureSourceRevision: saved.CaptureSourceRevision, ActorKind: saved.ActorKind, Type: capture.Type, ActionID: capture.ActionID}
	record, err := observeRecord("observation", at, public)
	if err != nil {
		return nil, err
	}
	record.ID = id
	series := observeSaveSeries(updated)
	checkpoint, err := observeRecord("checkpoint", at, observeProgress{Series: &series})
	if err != nil {
		return nil, err
	}
	attempt.ReasonCode = response.ReasonCode
	complete, err := observeRecord("cycle", at, attempt)
	if err != nil {
		return nil, err
	}
	return []obsstore.Record{record, checkpoint, complete}, nil
}

func observeValidSupersedes(saved observeSeries, state observeState, ref *command.ObservationRef) bool {
	if ref == nil {
		return true
	}
	prior, exists := state.observations[ref.ObservationID]
	return exists && ref.StoreID == saved.StoreID && prior.TargetRef.StoreID == saved.StoreID &&
		prior.TargetRef.CaptureID == saved.CaptureID && prior.SeriesID == saved.ID &&
		prior.CriterionID == saved.CriterionID && prior.CriterionRevision == saved.CriterionRevision
}
