package observe

import "errors"

type ObserverRevision struct {
	Fingerprint string
	Number      int64
}

type Revision struct {
	Number     int64
	Collection string
	Enabled    bool
	Removed    bool
	Observers  map[string]ObserverRevision
}

type RevisionChange struct {
	Number            int64
	Observers         map[string]int64
	ChangedObservers  map[string]bool
	CollectionChanged bool
	StopAll           bool
}

func Revise(prior Revision, expected int64, enabled bool, collection string, observers map[string]string) (RevisionChange, error) {
	if prior.Removed {
		return RevisionChange{}, errors.New("source_removed")
	}
	if expected < 0 || prior.Number != expected {
		return RevisionChange{}, errors.New("stale_revision")
	}
	if collection == "" || prior.Number >= 9007199254740991 {
		return RevisionChange{}, errors.New("invalid_revision")
	}
	if !validObserverRevisions(prior.Observers, observers) {
		return RevisionChange{}, errors.New("invalid_revision")
	}
	out := RevisionChange{Number: prior.Number + 1, Observers: map[string]int64{}, ChangedObservers: map[string]bool{},
		CollectionChanged: prior.changedCollection(collection)}
	for id, old := range prior.Observers {
		out.Observers[id] = old.Number
	}
	for id, fingerprint := range observers {
		old, exists := prior.Observers[id]
		if !exists || old.Fingerprint != fingerprint {
			out.Observers[id]++
			out.ChangedObservers[id] = true
		}
	}
	for id, old := range prior.Observers {
		if _, exists := observers[id]; !exists && old.Fingerprint != "" {
			out.ChangedObservers[id] = true
		}
	}
	out.StopAll = out.CollectionChanged || !enabled
	return out, nil
}

func (r Revision) changedCollection(collection string) bool {
	return r.Number > 0 && r.Collection != collection
}

func validObserverRevisions(prior map[string]ObserverRevision, observers map[string]string) bool {
	for id, fingerprint := range observers {
		if id == "" || fingerprint == "" || prior[id].Number >= 9007199254740991 || prior[id].Number < 0 {
			return false
		}
	}
	return true
}
