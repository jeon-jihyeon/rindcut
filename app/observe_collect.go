package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"slices"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/observe"
	"github.com/jeon-jihyeon/rindcut/internal/observe/command"
	obsstore "github.com/jeon-jihyeon/rindcut/internal/observe/store"
)

func (a App) observeCollect(ctx context.Context, store *obsstore.Store, state *observeState,
	sourceID, cycleID string) (observeWorkResult, error) {
	source := state.sources[sourceID]
	cp := state.checkpoints[sourceID]
	request := command.ReadRequest{Protocol: command.SourceProtocol, Operation: "read", SourceID: sourceID,
		SourceRevision: source.Revision, SourceInstance: source.Definition.SourceInstance,
		Stream: source.Definition.Stream, Generation: cp.Generation, Cursor: cp.Cursor, Limit: source.Definition.Reader.Limit}
	attempt := observeAttempt{AttemptID: rand.Text(), CycleID: cycleID, SourceID: sourceID,
		SourceRevision: source.Revision, Stage: "intent"}
	intent, err := observeRecord("cycle", a.observeNow(), attempt)
	if err != nil {
		return observeWorkResult{}, err
	}
	if err = appendObserve(ctx, store, state, []obsstore.Record{intent}); err != nil {
		return observeWorkResult{}, err
	}
	input, err := json.Marshal(request)
	if err != nil {
		return observeWorkResult{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(source.Definition.Reader.TimeoutSeconds)*time.Second)
	execution, runErr := command.Run(callCtx, source.Reader, input)
	cancel()
	attempt.Started, attempt.Exited, attempt.Stage = execution.Started, execution.Exited, "complete"
	page, decodeErr := command.DecodePage(execution.Output, request, source.Definition)
	if runErr == nil {
		runErr = decodeErr
	}
	if runErr != nil {
		attempt.ReasonCode = observeCode(runErr)
	}
	return a.observeCollectResult(ctx, store, state, source, cp, page, attempt)
}

func (a App) observeCollectResult(ctx context.Context, store *obsstore.Store, state *observeState,
	source observeSource, cp observeCheckpoint, page command.Page, attempt observeAttempt) (observeWorkResult, error) {
	at := a.observeNow()
	result := observeWorkResult{Reason: attempt.ReasonCode}
	records := []obsstore.Record{}
	if attempt.ReasonCode == "" {
		switch {
		case page.Status == "cursor_invalid":
			cp.Blocked, cp.Gap, attempt.ReasonCode = "cursor_invalid", true, "cursor_invalid"
		case page.Status == "unavailable":
			attempt.ReasonCode = "source_unavailable"
		case cp.Generation != nil && page.Generation != nil && *cp.Generation != *page.Generation:
			cp.Blocked, cp.Gap, attempt.ReasonCode = "generation_changed", true, "generation_changed"
		default:
			var err error
			records, result.Captured, err = observePageRecords(ctx, *state, source, page, at)
			if err != nil {
				return result, err
			}
			cp.Generation, cp.Cursor, cp.Coverage = page.Generation, page.NextCursor, page.Coverage
			cp.LastSuccessAt = &at
			result.Again = page.HasMore
			if !page.HasMore {
				cp.ReplaySourceRevision = 0
			}
		}
	}
	result.Reason = attempt.ReasonCode
	cpRecord, err := observeRecord("checkpoint", at, observeProgress{Checkpoint: &cp})
	if err != nil {
		return result, err
	}
	completion, err := observeRecord("cycle", at, attempt)
	if err != nil {
		return result, err
	}
	records = append(records, cpRecord, completion)
	if err = appendObserve(ctx, store, state, records); err != nil {
		return observeWorkResult{Reason: result.Reason}, err
	}
	return result, nil
}

func observePageRecords(ctx context.Context, state observeState, source observeSource, page command.Page,
	at time.Time) ([]obsstore.Record, int, error) {
	records := []obsstore.Record{}
	projections := map[observe.Identity][]observe.Projection{}
	captures := map[string]command.Capture{}
	seriesKeys := map[observeSeriesKey]bool{}
	count := 0
	for _, event := range page.Records {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		_, digest, err := command.Projection(event)
		if err != nil {
			return nil, 0, err
		}
		capture := command.Capture{Event: event, SourceID: source.SourceID, CaptureID: rand.Text(),
			CapturedAt: command.Timestamp(at), NormalizedDigest: digest, SourceRevision: source.Revision}
		projection := captureProjection(capture)
		existing := append(slices.Clone(state.projections[projection.Identity]), projections[projection.Identity]...)
		admission := observe.Admit(projection, existing)
		if admission.Kind == "replay" {
			capture = state.captures[admission.CaptureID]
			if capture.CaptureID == "" {
				capture = captures[admission.CaptureID]
			}
		} else {
			record, recordErr := observeRecord("event", at, capture)
			if recordErr != nil {
				return nil, 0, recordErr
			}
			record.ID = capture.CaptureID
			records = append(records, record)
			projections[projection.Identity] = append(projections[projection.Identity], projection)
			captures[capture.CaptureID] = capture
			count++
		}
		bindingRevision := admission.Binding(capture.SourceRevision, source.Revision, state.checkpoints[source.SourceID].ReplaySourceRevision)
		if bindingRevision == 0 {
			continue
		}
		binding := source
		if bindingRevision != source.Revision {
			binding = state.sourceRevisions[observeSourceKey{source.SourceID, bindingRevision}]
		}
		created, err := observeCaptureSeries(state, binding, capture, at, seriesKeys, observeEligibleObservers(admission, binding, source))
		if err != nil {
			return nil, 0, err
		}
		records = append(records, created...)
	}
	return records, count, nil
}

func observeCaptureSeries(state observeState, source observeSource, capture command.Capture,
	at time.Time, local map[observeSeriesKey]bool, eligible map[string]bool) ([]obsstore.Record, error) {
	records := []obsstore.Record{}
	for _, observer := range source.Definition.Observers {
		if !eligible[observer.ID] {
			continue
		}
		key := observeSeriesKey{capture.CaptureID, source.Revision, observer.ID, source.ObserverRevisions[observer.ID]}
		if !slices.Contains(observer.Types, capture.Type) || state.seriesKeys[key] != "" || local[key] {
			continue
		}
		capturedAt, err := time.Parse(time.RFC3339Nano, capture.CapturedAt)
		if err != nil {
			return nil, err
		}
		model, err := observe.NewSeries(observe.SeriesSpec{ID: rand.Text(), StoreID: state.snapshot.StoreID,
			SourceID: source.SourceID, SourceRevision: source.Revision, CaptureSourceRevision: capture.SourceRevision,
			CaptureID: capture.CaptureID, ObserverID: observer.ID, ObserverRevision: key.ObserverRevision,
			CriterionID: observer.CriterionID, CriterionRevision: observer.CriterionRevision,
			ParametersDigest: observeHash(observer.Parameters), ActorKind: observer.ActorKind, Mode: observer.Mode,
			Interval: time.Duration(observer.IntervalSeconds) * time.Second,
			Deadline: time.Duration(observer.DeadlineSeconds) * time.Second, CapturedAt: capturedAt})
		if err != nil {
			return nil, err
		}
		saved := observeSaveSeries(model)
		record, err := observeRecord("checkpoint", at, observeProgress{Series: &saved})
		if err != nil {
			return nil, err
		}
		records = append(records, record)
		local[key] = true
	}
	return records, nil
}

func observeEligibleObservers(admission observe.Admission, bound, current observeSource) map[string]bool {
	eligible := map[string]bool{}
	for _, observer := range current.Definition.Observers {
		eligible[observer.ID] = admission.AllowsObserver(bound.ObserverRevisions[observer.ID], current.ObserverRevisions[observer.ID])
	}
	return eligible
}
