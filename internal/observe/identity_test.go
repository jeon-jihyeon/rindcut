package observe_test

import (
	"testing"

	"github.com/jeon-jihyeon/rindcut/internal/observe"
	"github.com/stretchr/testify/assert"
)

func TestAdmit(t *testing.T) {
	id := observe.Identity{SourceID: "acme", SourceInstance: "acme", Generation: "g1", Stream: "actions",
		Type: "acme.action", ID: "e1", ProjectionProfile: "v1"}
	other := id
	other.Type = "acme.other"
	tcs := []struct {
		name      string
		prior     []observe.Projection
		kind      string
		capture   string
		conflicts []string
	}{
		{name: "new", kind: "new", conflicts: []string{}},
		{name: "replay", prior: []observe.Projection{{Identity: id, Digest: "d1", CaptureID: "c1"}}, kind: "replay", capture: "c1", conflicts: []string{}},
		{name: "conflict", prior: []observe.Projection{{Identity: id, Digest: "d2", CaptureID: "c2"}}, kind: "conflict", conflicts: []string{"c2"}},
		{name: "different_type", prior: []observe.Projection{{Identity: other, Digest: "d1", CaptureID: "c1"}}, kind: "new", conflicts: []string{}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := observe.Admit(observe.Projection{Identity: id, Digest: "d1"}, tc.prior)
			assert.Equal(t, tc.kind, got.Kind)
			assert.Equal(t, tc.capture, got.CaptureID)
			assert.Equal(t, tc.conflicts, got.Conflicts)
		})
	}
}

func TestAdmissionBinding(t *testing.T) {
	tcs := []struct {
		name            string
		kind            string
		captureRevision int64
		sourceRevision  int64
		replayRevision  int64
		binding         int64
	}{
		{name: "new", kind: "new", captureRevision: 4, sourceRevision: 4, binding: 4},
		{name: "conflict", kind: "conflict", captureRevision: 4, sourceRevision: 4, binding: 4},
		{name: "ordinary_replay", kind: "replay", captureRevision: 1, sourceRevision: 2},
		{name: "explicit_replay", kind: "replay", captureRevision: 1, sourceRevision: 4, replayRevision: 3, binding: 3},
		{name: "same_epoch_capture", kind: "replay", captureRevision: 3, sourceRevision: 4, replayRevision: 3},
		{name: "newer_capture", kind: "replay", captureRevision: 4, sourceRevision: 4, replayRevision: 3},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := observe.Admission{Kind: tc.kind}
			assert.Equal(t, tc.binding, a.Binding(tc.captureRevision, tc.sourceRevision, tc.replayRevision))
		})
	}
}

func TestAdmissionAllowsObserver(t *testing.T) {
	tcs := []struct {
		name    string
		kind    string
		bound   int64
		current int64
		allowed bool
	}{
		{name: "new", kind: "new", bound: 2, current: 2, allowed: true},
		{name: "unchanged_replay", kind: "replay", bound: 1, current: 1, allowed: true},
		{name: "changed_replay", kind: "replay", bound: 1, current: 2},
		{name: "new_observer", kind: "replay", current: 1},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.allowed, (observe.Admission{Kind: tc.kind}).AllowsObserver(tc.bound, tc.current))
		})
	}
}
