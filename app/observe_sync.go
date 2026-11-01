package app

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/observe"
	obsstore "github.com/jeon-jihyeon/rindcut/internal/observe/store"
)

type observeWorkResult struct {
	Captured     int
	Observations int
	Again        bool
	Reason       string
}

func (a App) ObserveSync(parent context.Context) (ObserveCycle, error) {
	return a.observeSync(parent)
}

func (a App) observeSync(parent context.Context) (ObserveCycle, error) {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	store := obsstore.Open(a.Home)
	initial, initialErr := store.Read(ctx)
	if initialErr == nil && !initial.Present {
		return ObserveCycle{Errors: []string{}}, nil
	}
	if initialErr != nil && !errors.Is(initialErr, obsstore.ErrIncomplete) {
		return ObserveCycle{}, errors.New(observeCode(initialErr))
	}
	unlock, err := store.AcquireControl(ctx)
	if err != nil {
		return ObserveCycle{}, errors.New(observeCode(err))
	}
	defer unlock()
	snapshot, err := store.Prepare(ctx)
	if err != nil {
		return ObserveCycle{}, errors.New(observeCode(err))
	}
	state, err := loadObserve(ctx, snapshot)
	if err != nil {
		return ObserveCycle{}, err
	}
	if !snapshot.Present || len(state.sources) == 0 {
		return ObserveCycle{Errors: []string{}}, nil
	}
	return a.observeCycle(ctx, store, &state)
}

func (a App) observeCycle(ctx context.Context, store *obsstore.Store, state *observeState) (ObserveCycle, error) {
	workCtx, stop := observeWorkContext(ctx)
	defer stop()
	cutoff := a.observeNow()
	cycle := ObserveCycle{ID: rand.Text(), StartedAt: cutoff, Errors: []string{}}
	if err := a.observeInterrupted(ctx, store, state, cycle.ID, cutoff); err != nil {
		return cycle, err
	}
	sources, due, err := observeRunnable(*state, cutoff)
	if err != nil {
		return cycle, err
	}
	var workErr error
	for cycle.Attempts < 100 && workCtx.Err() == nil {
		rotation := observe.Rotation{NextLane: state.rotation.NextLane, AfterSourceID: state.rotation.AfterSourceID}
		work, next, ok := rotation.Next(sources, due, cutoff)
		if !ok {
			break
		}
		record, recordErr := observeRecord("checkpoint", a.observeNow(), observeProgress{Rotation: &observeRotation{
			NextLane: next.NextLane, AfterSourceID: next.AfterSourceID}})
		if recordErr != nil {
			return cycle, recordErr
		}
		if err = appendObserve(ctx, store, state, []obsstore.Record{record}); err != nil {
			return cycle, err
		}
		var result observeWorkResult
		result, workErr = a.observeWork(workCtx, store, state, work, cycle.ID)
		cycle.Attempts++
		cycle.Captured += result.Captured
		cycle.Observations += result.Observations
		if result.Reason != "" {
			cycle.Errors = append(cycle.Errors, result.Reason)
		}
		if workErr != nil {
			break
		}
		sources, due = observeCompleted(sources, due, work, result.Again)
	}
	cycle.FinishedAt = a.observeNow()
	cycle.Incomplete = workErr != nil || workCtx.Err() != nil || len(sources) > 0 || len(due) > 0
	if ctx.Err() != nil {
		return cycle, ctx.Err()
	}
	record, err := observeRecord("cycle", cycle.FinishedAt, cycle)
	if err != nil {
		return cycle, err
	}
	return cycle, errors.Join(workErr, appendObserve(ctx, store, state, []obsstore.Record{record}))
}

func observeWorkContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithTimeout(ctx, 59*time.Second)
	}
	reserve := max(time.Duration(0), min(time.Second, time.Until(deadline)/10))
	return context.WithDeadline(ctx, deadline.Add(-reserve))
}

func observeRunnable(state observeState, cutoff time.Time) ([]string, []observe.Due, error) {
	sources := []string{}
	for _, id := range state.sourceIDs() {
		source := state.sources[id]
		if source.Definition.Enabled && !source.Removed && state.checkpoints[id].Blocked == "" {
			sources = append(sources, id)
		}
	}
	due := []observe.Due{}
	for id, saved := range state.series {
		model, err := observeRestoreSeries(saved, state)
		if err != nil {
			return nil, nil, errors.New("store_corrupt")
		}
		if model.Due(cutoff) {
			due = append(due, observe.Due{SeriesID: id, At: saved.DueAt})
		}
	}
	return sources, due, nil
}

func observeCompleted(sources []string, due []observe.Due, work observe.Work, again bool) ([]string, []observe.Due) {
	if work.Kind == "source" && !again {
		for i, id := range sources {
			if id == work.ID {
				sources = append(sources[:i], sources[i+1:]...)
				break
			}
		}
	}
	if work.Kind == "observation" {
		for i, item := range due {
			if item.SeriesID == work.ID {
				due = append(due[:i], due[i+1:]...)
				break
			}
		}
	}
	return sources, due
}

func (a App) observeWork(ctx context.Context, store *obsstore.Store, state *observeState,
	work observe.Work, cycleID string) (observeWorkResult, error) {
	if work.Kind == "source" {
		return a.observeCollect(ctx, store, state, work.ID, cycleID)
	}
	return a.observeVerify(ctx, store, state, work.ID, cycleID)
}
