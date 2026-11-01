package app

import (
	"context"
	"errors"
	"sort"
	"time"

	obsstore "github.com/jeon-jihyeon/rindcut/internal/observe/store"
)

func (a App) observeInterrupted(ctx context.Context, store *obsstore.Store, state *observeState,
	_ string, at time.Time) error {
	ids := make([]string, 0, len(state.attempts))
	for id, attempt := range state.attempts {
		if attempt.Stage == "intent" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		attempt := state.attempts[id]
		attempt.Stage, attempt.ReasonCode = "complete", "interrupted"
		attempt.Started, attempt.Exited = false, false
		records, err := observeInterruptedRecords(attempt, *state, at)
		if err != nil {
			return err
		}
		if err = appendObserve(ctx, store, state, records); err != nil {
			return err
		}
	}
	return nil
}

func observeInterruptedRecords(attempt observeAttempt, state observeState, at time.Time) ([]obsstore.Record, error) {
	if attempt.SeriesID != "" {
		saved, exists := state.series[attempt.SeriesID]
		if !exists {
			return nil, errors.New("store_corrupt")
		}
		model, err := observeRestoreSeries(saved, state)
		if err != nil {
			return nil, errors.New("store_corrupt")
		}
		if observeSeriesActive(saved, state) && model.Due(at) {
			execution, err := observeExecutionFor(saved, state, at)
			if err != nil {
				return nil, err
			}
			return observeVerificationRecords(saved, state, observeUnknown(execution.Request, "interrupted"), attempt, at)
		}
	}
	complete, err := observeRecord("cycle", at, attempt)
	if err != nil {
		return nil, err
	}
	return []obsstore.Record{complete}, nil
}
