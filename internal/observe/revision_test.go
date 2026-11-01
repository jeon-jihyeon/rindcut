package observe_test

import (
	"testing"

	"github.com/jeon-jihyeon/rindcut/internal/observe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRevise(t *testing.T) {
	tcs := []struct {
		name         string
		collection   string
		enabled      bool
		fingerprint  string
		removed      bool
		wantRevision int64
		wantStop     bool
		wantChanged  bool
		invalid      bool
	}{
		{name: "same", collection: "c1", enabled: true, fingerprint: "f1", wantRevision: 1},
		{name: "observer_changed", collection: "c1", enabled: true, fingerprint: "f2", wantRevision: 2, wantChanged: true},
		{name: "collection_changed", collection: "c2", enabled: true, fingerprint: "f1", wantRevision: 1, wantStop: true},
		{name: "disabled", collection: "c1", fingerprint: "f1", wantRevision: 1, wantStop: true},
		{name: "removed", collection: "c1", enabled: true, fingerprint: "f1", removed: true, invalid: true},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prior := observe.Revision{Number: 1, Collection: "c1", Enabled: true, Removed: tc.removed,
				Observers: map[string]observe.ObserverRevision{"effect": {Fingerprint: "f1", Number: 1}}}
			got, err := observe.Revise(prior, 1, tc.enabled, tc.collection, map[string]string{"effect": tc.fingerprint})
			if tc.invalid {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, int64(2), got.Number)
			assert.Equal(t, tc.wantRevision, got.Observers["effect"])
			assert.Equal(t, tc.wantChanged, got.ChangedObservers["effect"])
			assert.Equal(t, tc.wantStop, got.StopAll)
			assert.Equal(t, int64(1), prior.Observers["effect"].Number)
		})
	}
}
