package app

import (
	"context"
	"errors"
	"time"

	obsstore "github.com/jeon-jihyeon/rindcut/internal/observe/store"
)

func (a App) SourcePreview(ctx context.Context, in SourceRequest) (SourcePreview, error) {
	store := obsstore.Open(a.Home)
	if in.Operation == "recover_store" {
		return previewObserveRecovery(ctx, store, in)
	}
	if in.Operation != "" && in.Operation != "configure" {
		return SourcePreview{}, errors.New("invalid_request")
	}
	snapshot, err := store.Read(ctx)
	if err != nil {
		return SourcePreview{}, errors.New(observeCode(err))
	}
	state, err := loadObserve(ctx, snapshot)
	if err != nil {
		return SourcePreview{}, err
	}
	plan, err := prepareObserveSource(ctx, in, state)
	if err != nil {
		return SourcePreview{}, err
	}
	if err = applyObserveRecovery(&plan, in, state); err != nil {
		return SourcePreview{}, err
	}
	plan.preview.PreviewDigest = observePlanDigest(plan, state)
	return plan.preview, nil
}

func observePlanDigest(plan observeSourcePlan, state observeState) string {
	return observeHash(struct {
		Source     observeSource
		Checkpoint observeCheckpoint
		Recovery   *SourceRecovery
		StoreID    string
		Offset     int64
	}{
		plan.source, plan.checkpoint, plan.preview.Recovery, state.snapshot.StoreID, state.snapshot.CommittedOffset})
}

func previewObserveRecovery(ctx context.Context, store *obsstore.Store, in SourceRequest) (SourcePreview, error) {
	if len(in.Definition) != 0 || in.ExpectedRevision != nil || in.Recovery == nil || in.Recovery.Mode != "new_store" ||
		in.Recovery.CheckpointToken != "" || in.Recovery.CollectionIdentity != "" || in.Recovery.Generation != "" {
		return SourcePreview{}, errors.New("invalid_request")
	}
	recovery, err := store.RecoveryPreview(ctx)
	if err != nil {
		return SourcePreview{}, errors.New(observeCode(err))
	}
	if in.ExpectedStoreToken != "" && in.ExpectedStoreToken != recovery.Token {
		return SourcePreview{}, errors.New("stale_preview")
	}
	out := SourcePreview{Operation: "recover_store", ExpectedStoreToken: recovery.Token, Recovery: in.Recovery,
		Effects: []string{"continuity_lost", "empty_store", "source_registration_required"}, StoreRecovery: observeRecoveryView(recovery)}
	out.PreviewDigest = observeHash(struct {
		Operation string
		Token     string
		Recovery  *SourceRecovery
	}{
		out.Operation, recovery.Token, in.Recovery})
	return out, nil
}

func observeRecoveryView(r obsstore.Recovery) *StoreRecovery {
	return &StoreRecovery{Stage: r.Stage, StoreID: r.StoreID, NewStoreID: r.NewStoreID, ContinuityLost: r.ContinuityLost}
}

func (a App) SourceSave(ctx context.Context, in SourceRequest) (SourceSaved, error) {
	if !in.Confirmed || in.PreviewDigest == "" {
		return SourceSaved{}, errors.New("confirmation_required")
	}
	store := obsstore.Open(a.Home)
	unlock, err := store.AcquireControl(ctx)
	if err != nil {
		return SourceSaved{}, errors.New(observeCode(err))
	}
	defer unlock()
	if in.Operation == "recover_store" {
		return saveObserveRecovery(ctx, store, in)
	}
	if in.Operation != "" && in.Operation != "configure" {
		return SourceSaved{}, errors.New("invalid_request")
	}
	snapshot, err := store.Prepare(ctx)
	if err != nil {
		return SourceSaved{}, errors.New(observeCode(err))
	}
	state, err := loadObserve(ctx, snapshot)
	if err != nil {
		return SourceSaved{}, err
	}
	plan, err := prepareObserveSource(ctx, in, state)
	if err != nil {
		return SourceSaved{}, err
	}
	if err = applyObserveRecovery(&plan, in, state); err != nil {
		return SourceSaved{}, err
	}
	if err = validateObserveSave(ctx, &plan, state, in.PreviewDigest); err != nil {
		return SourceSaved{}, err
	}
	records, err := observeSourceRecords(plan, state, a.observeNow())
	if err != nil {
		return SourceSaved{}, err
	}
	if err = appendObserve(ctx, store, &state, records); err != nil {
		return SourceSaved{}, err
	}
	summary := observeSourceSummary(plan.source, plan.checkpoint, state.snapshot.StoreID)
	return SourceSaved{Source: &summary}, nil
}

func validateObserveSave(ctx context.Context, plan *observeSourcePlan, state observeState, digest string) error {
	// A stale plan must be rejected before its reader can produce any effects
	if digest != observePlanDigest(*plan, state) {
		return errors.New("stale_preview")
	}
	if plan.preview.Recovery == nil || plan.preview.Recovery.Mode != "reset" {
		return nil
	}
	return probeObserveGeneration(ctx, plan, plan.preview.Recovery.Generation)
}

func saveObserveRecovery(ctx context.Context, store *obsstore.Store, in SourceRequest) (SourceSaved, error) {
	preview, err := previewObserveRecovery(ctx, store, in)
	if err != nil {
		return SourceSaved{}, err
	}
	if in.ExpectedStoreToken == "" || in.PreviewDigest != preview.PreviewDigest {
		return SourceSaved{}, errors.New("stale_preview")
	}
	recovery, err := store.Recover(ctx, in.ExpectedStoreToken)
	if err != nil {
		return SourceSaved{}, errors.New(observeCode(err))
	}
	return SourceSaved{Recovery: observeRecoveryView(recovery)}, nil
}

func observeSourceRecords(plan observeSourcePlan, state observeState, at time.Time) ([]obsstore.Record, error) {
	source, err := observeRecord("source", at, plan.source)
	if err != nil {
		return nil, err
	}
	cp, err := observeRecord("checkpoint", at, observeProgress{Checkpoint: &plan.checkpoint})
	if err != nil {
		return nil, err
	}
	records := []obsstore.Record{source, cp}
	for _, saved := range state.series {
		if saved.SourceID != plan.source.SourceID || !plan.stopAll && !plan.changedObservers[saved.ObserverID] {
			continue
		}
		model, err := observeRestoreSeries(saved, state)
		if err != nil {
			return nil, err
		}
		stopped := observeSaveSeries(model.Stop(plan.source.Removed))
		r, err := observeRecord("checkpoint", at, observeProgress{Series: &stopped})
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, nil
}

func (a App) SourceList(ctx context.Context, includeRemoved bool) ([]SourceSummary, error) {
	snapshot, err := obsstore.Open(a.Home).Read(ctx)
	if err != nil {
		return nil, errors.New(observeCode(err))
	}
	state, err := loadObserve(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	out := []SourceSummary{}
	for _, id := range state.sourceIDs() {
		source := state.sources[id]
		if source.Removed && !includeRemoved {
			continue
		}
		out = append(out, observeSourceSummary(source, state.checkpoints[id], snapshot.StoreID))
	}
	return out, nil
}

func (a App) SourceRemove(ctx context.Context, in SourceRemoveRequest) (SourceSummary, error) {
	if !in.Confirmed || in.ExpectedRevision < 1 {
		return SourceSummary{}, errors.New("confirmation_required")
	}
	store := obsstore.Open(a.Home)
	unlock, err := store.AcquireControl(ctx)
	if err != nil {
		return SourceSummary{}, errors.New(observeCode(err))
	}
	defer unlock()
	snapshot, err := store.Prepare(ctx)
	if err != nil {
		return SourceSummary{}, errors.New(observeCode(err))
	}
	state, err := loadObserve(ctx, snapshot)
	if err != nil {
		return SourceSummary{}, err
	}
	source, ok := state.sources[in.ID]
	if !ok || source.Removed {
		return SourceSummary{}, errors.New("source_not_found")
	}
	if source.Revision != in.ExpectedRevision {
		return SourceSummary{}, errors.New("stale_revision")
	}
	if source.Revision >= 9007199254740991 {
		return SourceSummary{}, errors.New("invalid_revision")
	}
	source.Revision++
	source.Removed = true
	source.Definition.Enabled = false
	cp := state.checkpoints[in.ID]
	cp.SourceRevision = source.Revision
	cp.ReplaySourceRevision = 0
	plan := observeSourcePlan{source: source, checkpoint: cp, stopAll: true}
	records, err := observeSourceRecords(plan, state, a.observeNow())
	if err != nil {
		return SourceSummary{}, err
	}
	if err = appendObserve(ctx, store, &state, records); err != nil {
		return SourceSummary{}, err
	}
	return observeSourceSummary(source, cp, state.snapshot.StoreID), nil
}

func (a App) observeNow() time.Time {
	if a.Now == nil {
		return time.Now().UTC()
	}
	return a.Now().UTC()
}
