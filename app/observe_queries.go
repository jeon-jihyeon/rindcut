package app

import (
	"context"
	"errors"

	obsstore "github.com/jeon-jihyeon/rindcut/internal/observe/store"
)

func (a App) ObserveRecords(ctx context.Context, input ObserveQuery) (ObservePage, error) {
	query := obsstore.Query{SourceID: input.SourceID, Type: input.Type, ActionID: input.ActionID,
		Kind: input.Kind, Cursor: input.Cursor, Limit: input.Limit}
	if input.TargetRef != nil {
		query.TargetRef = &obsstore.TargetRef{StoreID: input.TargetRef.StoreID, CaptureID: input.TargetRef.CaptureID}
	}
	page, err := obsstore.Open(a.Home).Query(ctx, query)
	if err != nil {
		return ObservePage{}, errors.New(observeCode(err))
	}
	out := ObservePage{Records: []ObserveRecord{}, NextCursor: page.NextCursor,
		HasMore: page.HasMore, StorePresent: page.StorePresent}
	for _, record := range page.Records {
		out.Records = append(out.Records, ObserveRecord{ID: record.ID, Kind: record.Kind, At: record.At, Data: record.Data})
	}
	return out, nil
}

func (a App) ObserveStatus(ctx context.Context) (ObserveStatus, error) {
	snapshot, err := obsstore.Open(a.Home).Read(ctx)
	if err != nil {
		return ObserveStatus{}, errors.New(observeCode(err))
	}
	state, err := loadObserve(ctx, snapshot)
	if err != nil {
		return ObserveStatus{}, err
	}
	out := ObserveStatus{StorePresent: snapshot.Present, StoreID: snapshot.StoreID, Sources: []ObserveSourceStatus{}}
	for _, id := range state.sourceIDs() {
		source := state.sources[id]
		checkpoint := state.checkpoints[id]
		status := ObserveSourceStatus{Source: observeSourceSummary(source, checkpoint, snapshot.StoreID),
			Generation: checkpoint.Generation, Blocked: checkpoint.Blocked, Gap: checkpoint.Gap}
		state.sourceCounts(&status, id, a.observeNow())
		out.Sources = append(out.Sources, status)
	}
	return out, nil
}

func appendObserve(ctx context.Context, store *obsstore.Store, state *observeState, records []obsstore.Record) error {
	meta, err := store.Append(ctx, records)
	if err != nil {
		return errors.New(observeCode(err))
	}
	state.snapshot = meta
	for _, record := range records {
		if err := state.accept(record); err != nil {
			return errors.New("store_corrupt")
		}
	}
	return nil
}
