package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jeon-jihyeon/rindcut/internal/observe"
	"github.com/jeon-jihyeon/rindcut/internal/observe/command"
)

type observeSourcePlan struct {
	source           observeSource
	checkpoint       observeCheckpoint
	changedObservers map[string]bool
	stopAll          bool
	preview          SourcePreview
}

func prepareObserveSource(ctx context.Context, in SourceRequest, state observeState) (observeSourcePlan, error) {
	var out observeSourcePlan
	if in.ExpectedRevision == nil || *in.ExpectedRevision < 0 || in.ExpectedStoreToken != "" {
		return out, errors.New("invalid_request")
	}
	d, err := command.ParseDefinition(in.Definition)
	if err != nil {
		return out, err
	}
	current, exists := state.sources[d.ID]
	reader, err := command.Prepare(ctx, d.Reader.Program, d.Reader.Args, d.Reader.FingerprintFiles)
	if err != nil {
		return out, err
	}
	source := observeSource{SourceID: d.ID, Revision: current.Revision + 1, Definition: d, Reader: reader,
		Observers: map[string]command.Prepared{}, ObserverRevisions: map[string]int64{}}
	for _, observer := range d.Observers {
		prepared, err := command.Prepare(ctx, observer.Program, observer.Args, observer.FingerprintFiles)
		if err != nil {
			return out, err
		}
		source.Observers[observer.ID] = prepared
	}
	source.Collection = observeHash(struct {
		Instance   string
		Stream     string
		Capability command.Capability
		Adapter    string
		Projection string
		Reader     command.Reader
		Prepared   command.Prepared
	}{d.SourceInstance, d.Stream, d.Capability, d.AdapterRevision, d.ProjectionProfile, d.Reader, reader})
	change, err := observe.Revise(observeRevision(current), *in.ExpectedRevision, d.Enabled, source.Collection,
		observeObserverHashes(source))
	if err != nil {
		return out, err
	}
	source.Revision = change.Number
	source.ObserverRevisions = change.Observers
	cp := state.checkpoints[d.ID]
	cp.SourceID, cp.SourceRevision = d.ID, source.Revision
	if change.StopAll {
		cp.ReplaySourceRevision = 0
	}
	if !exists {
		cp.Collection = source.Collection
		cp.Coverage = command.Coverage{Kind: "unknown", Limitations: []string{}}
	}
	if change.CollectionChanged {
		cp.Blocked = "collection_changed"
		cp.Gap = true
	}
	definition, err := json.Marshal(d)
	if err != nil {
		return out, errors.New("invalid_definition")
	}
	preview := SourcePreview{Operation: "configure", Definition: definition, ProposedRevision: source.Revision,
		CollectionIdentity: source.Collection, CheckpointToken: observeCheckpointToken(current, state.checkpoints[d.ID], state.snapshot.StoreID),
		Effects: []string{"source_revision"}, Recovery: in.Recovery, Executions: observeExecutions(source)}
	if change.CollectionChanged {
		preview.Effects = append(preview.Effects, "collection_recovery_required")
	}
	if !d.Enabled {
		preview.Effects = append(preview.Effects, "suspended_configuration")
	}
	return observeSourcePlan{source: source, checkpoint: cp, changedObservers: change.ChangedObservers,
		stopAll: change.StopAll, preview: preview}, nil
}

func observeRevision(source observeSource) observe.Revision {
	hashes := observeObserverHashes(source)
	observers := map[string]observe.ObserverRevision{}
	for id, number := range source.ObserverRevisions {
		observers[id] = observe.ObserverRevision{Number: number, Fingerprint: hashes[id]}
	}
	return observe.Revision{Number: source.Revision, Collection: source.Collection, Enabled: source.Definition.Enabled,
		Removed: source.Removed, Observers: observers}
}

func observeObserverHashes(source observeSource) map[string]string {
	out := map[string]string{}
	for _, observer := range source.Definition.Observers {
		out[observer.ID] = observeHash(struct {
			Definition command.Observer
			Prepared   command.Prepared
		}{
			observer, source.Observers[observer.ID]})
	}
	return out
}

func applyObserveRecovery(plan *observeSourcePlan, in SourceRequest, state observeState) error {
	if in.Recovery == nil {
		return nil
	}
	r := in.Recovery
	if r.CheckpointToken == "" || r.CheckpointToken != plan.preview.CheckpointToken {
		return errors.New("stale_preview")
	}
	current, exists := state.sources[plan.source.SourceID]
	if !exists {
		return errors.New("invalid_recovery")
	}
	switch r.Mode {
	case "backfill":
		if current.Collection != plan.source.Collection || r.CollectionIdentity != "" || r.Generation != "" {
			return errors.New("invalid_recovery")
		}
	case "reset":
		if r.CollectionIdentity != plan.source.Collection || r.Generation == "" {
			return errors.New("invalid_recovery")
		}
		generation := r.Generation
		plan.checkpoint.Generation = &generation
	default:
		return errors.New("invalid_recovery")
	}
	plan.checkpoint.Collection = plan.source.Collection
	plan.checkpoint.Cursor = nil
	plan.checkpoint.Blocked = ""
	plan.checkpoint.Gap = true
	if plan.source.Definition.Enabled {
		plan.checkpoint.ReplaySourceRevision = plan.source.Revision
	}
	plan.preview.Effects = append(plan.preview.Effects, "continuity_gap", r.Mode)
	return nil
}

func probeObserveGeneration(ctx context.Context, plan *observeSourcePlan, expected string) error {
	s := plan.source
	req := command.ReadRequest{Protocol: command.SourceProtocol, Operation: "read", SourceID: s.SourceID,
		SourceRevision: s.Revision, SourceInstance: s.Definition.SourceInstance, Stream: s.Definition.Stream, Limit: 1}
	page, err := command.Read(ctx, s.Reader, req, s.Definition)
	if err != nil {
		return err
	}
	if page.Status != "ok" || page.Generation == nil || *page.Generation != expected {
		return errors.New("stale_preview")
	}
	generation := *page.Generation
	plan.checkpoint.Generation = &generation
	return nil
}

func observeCheckpointToken(source observeSource, cp observeCheckpoint, storeID string) string {
	return observeHash(struct {
		StoreID    string
		SourceID   string
		Revision   int64
		Checkpoint observeCheckpoint
	}{
		storeID, source.SourceID, source.Revision, cp})
}

func observeSourceSummary(source observeSource, cp observeCheckpoint, storeID string) SourceSummary {
	d := source.Definition
	out := SourceSummary{ID: source.SourceID, Revision: source.Revision, Enabled: d.Enabled, Removed: source.Removed,
		SourceInstance: d.SourceInstance, Stream: d.Stream,
		Capability:      SourceCapability{Kind: d.Capability.Kind, Types: slices.Clone(d.Capability.Types), Limitations: slices.Clone(d.Capability.Limitations)},
		AdapterRevision: d.AdapterRevision, ProjectionProfile: d.ProjectionProfile, Description: d.Description,
		CollectionIdentity: source.Collection, CheckpointToken: observeCheckpointToken(source, cp, storeID),
		Observers: []SourceObserverSummary{}}
	for _, observer := range d.Observers {
		out.Observers = append(out.Observers, SourceObserverSummary{ID: observer.ID, Revision: source.ObserverRevisions[observer.ID],
			CriterionID: observer.CriterionID, CriterionRevision: observer.CriterionRevision, ActorKind: observer.ActorKind})
	}
	return out
}
