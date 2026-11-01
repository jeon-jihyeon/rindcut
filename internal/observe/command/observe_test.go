package command_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jeon-jihyeon/rindcut/internal/observe/command"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func observerInput(t *testing.T) command.ObserveRequest {
	t.Helper()
	var e command.Event
	require.NoError(t, command.Decode([]byte(eventJSON), &e))
	_, sum, err := command.Projection(e)
	require.NoError(t, err)
	return command.ObserveRequest{Protocol: command.ObserverProtocol, SourceID: "acme", SourceRevision: 2,
		ObserverID: "effect", ObserverRevision: 1, CriterionID: "landed", CriterionRevision: "v1", SeriesID: "s1",
		Parameters: map[string]any{}, TargetRef: command.TargetRef{StoreID: "store", CaptureID: "c1"},
		Event: command.Capture{Event: e, SourceID: "acme", CaptureID: "c1", CapturedAt: "2026-01-01T00:00:00Z",
			NormalizedDigest: sum, SourceRevision: 1}, Now: "2026-01-01T00:00:01Z"}
}

func TestDecodeObservation(t *testing.T) {
	valid := `{"protocol":"rindcut.observer/v1","source_id":"acme","source_revision":2,"observer_id":"effect","observer_revision":1,"criterion_id":"landed","criterion_revision":"v1","series_id":"s1","target_ref":{"store_id":"store","capture_id":"c1"},"result":"pending","reason_code":"not_yet","evidence":[]}`
	tcs := []struct {
		name    string
		data    string
		invalid bool
	}{
		{name: "valid", data: valid},
		{
			name:    "wrong_protocol",
			data:    strings.Replace(valid, `"protocol":"rindcut.observer/v1"`, `"protocol":"other"`, 1),
			invalid: true,
		},
		{
			name:    "wrong_source",
			data:    strings.Replace(valid, `"source_id":"acme"`, `"source_id":"other"`, 1),
			invalid: true,
		},
		{
			name:    "wrong_observer",
			data:    strings.Replace(valid, `"observer_id":"effect"`, `"observer_id":"other"`, 1),
			invalid: true,
		},
		{
			name:    "wrong_observer_revision",
			data:    strings.Replace(valid, `"observer_revision":1`, `"observer_revision":2`, 1),
			invalid: true,
		},
		{
			name:    "wrong_criterion",
			data:    strings.Replace(valid, `"criterion_id":"landed"`, `"criterion_id":"other"`, 1),
			invalid: true,
		},
		{
			name:    "wrong_criterion_revision",
			data:    strings.Replace(valid, `"criterion_revision":"v1"`, `"criterion_revision":"v2"`, 1),
			invalid: true,
		},
		{
			name:    "wrong_series",
			data:    strings.Replace(valid, `"series_id":"s1"`, `"series_id":"other"`, 1),
			invalid: true,
		},
		{
			name:    "wrong_store",
			data:    strings.Replace(valid, `"store_id":"store"`, `"store_id":"other"`, 1),
			invalid: true,
		},
		{name: "wrong_capture", data: strings.Replace(valid, `"capture_id":"c1"`, `"capture_id":"c2"`, 1), invalid: true},
		{name: "wrong_revision", data: strings.Replace(valid, `"source_revision":2`, `"source_revision":1`, 1), invalid: true},
		{name: "authority", data: strings.Replace(valid, `"pending"`, `"approved"`, 1), invalid: true},
		{name: "arbitrary_reason", data: strings.Replace(valid, `"not_yet"`, `"contains credential"`, 1), invalid: true},
		{name: "future", data: strings.Replace(valid, `"evidence":[]`, `"evidence":[],"next_observe_at":"2026-01-01T00:00:02Z"`, 1)},
		{name: "past", data: strings.Replace(valid, `"evidence":[]`, `"evidence":[],"next_observe_at":"2026-01-01T00:00:01Z"`, 1), invalid: true},
		{name: "null_time", data: strings.Replace(valid, `"evidence":[]`, `"evidence":[],"next_observe_at":null`, 1), invalid: true},
		{name: "foreign_supersedes", data: strings.Replace(valid, `"evidence":[]`, `"evidence":[],"supersedes":{"store_id":"other","observation_id":"o1"}`, 1), invalid: true},
		{name: "extra", data: strings.Replace(valid, `"evidence":[]`, `"evidence":[],"verified":true`, 1), invalid: true},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := command.DecodeObservation([]byte(tc.data), observerInput(t))
			assert.Equal(t, tc.invalid, err != nil)
		})
	}
}

func TestDecodeCapture(t *testing.T) {
	t.Parallel()
	r := observerInput(t)
	data, err := json.Marshal(r)
	require.NoError(t, err)
	var got command.ObserveRequest
	require.NoError(t, command.Decode(data, &got))
	assert.Equal(t, r.SourceRevision, got.SourceRevision)
	assert.Equal(t, r.Event.SourceRevision, got.Event.SourceRevision)
	assert.Equal(t, r.TargetRef, got.TargetRef)
}
