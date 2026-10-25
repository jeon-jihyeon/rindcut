package ledger_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

func TestSummaryCriticCompatibility(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name   string
		critic string
		want   int
	}{
		{name: "legacy judgment has no critic"},
		{name: "approved is not a problem", critic: `,"critic":{"verdict":"approved"}`},
		{name: "rejected remains separate", critic: `,"critic":{"verdict":"rejected","source":"acme"}`, want: 1},
		{name: "inconclusive remains visible", critic: `,"critic":{"verdict":"inconclusive"}`, want: 1},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
			raw := `{"job_id":"report","slot_key":"report@2026-10-08T09:00:00Z",` +
				`"slot":"2026-10-08T09:00:00Z","verdict":"pass","checks":[]` + tc.critic + `}`
			var j ledger.Judgment
			require.NoError(t, json.Unmarshal([]byte(raw), &j))
			report := ledger.Summary([]ledger.Entry{{ID: "v1", Kind: ledger.KindVerdict, At: at,
				Data: json.RawMessage(raw)}}, at, at.Add(time.Hour))
			assert.Equal(t, 1, report.Totals.Pass)
			assert.Empty(t, report.Problems)
			assert.Len(t, report.CriticProblems, tc.want)
		})
	}
}
