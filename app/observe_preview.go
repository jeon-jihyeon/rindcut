package app

import (
	"slices"

	"github.com/jeon-jihyeon/rindcut/internal/observe/command"
)

func observeExecutions(source observeSource) []SourceExecution {
	out := []SourceExecution{observeExecutionView("reader", "", source.Reader)}
	for _, observer := range source.Definition.Observers {
		out = append(out, observeExecutionView("observer", observer.ID, source.Observers[observer.ID]))
	}
	return out
}

func observeExecutionView(role, observerID string, program command.Prepared) SourceExecution {
	out := SourceExecution{Role: role, ObserverID: observerID, Program: program.Program, Dir: program.Dir,
		Args: slices.Clone(program.Args), Fingerprints: []SourceFingerprint{}}
	for _, fingerprint := range program.Fingerprints {
		out.Fingerprints = append(out.Fingerprints, SourceFingerprint{Path: fingerprint.Path, Digest: fingerprint.Digest})
	}
	return out
}
