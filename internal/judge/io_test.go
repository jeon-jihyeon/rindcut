package judge_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTickIOFailure(t *testing.T) {
	t.Parallel()
	tcs := []struct{ name string }{{name: "read"}, {name: "append"}, {name: "backfill append"}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.save(baseJob(f.folder), time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
			path := filepath.Join(f.home, "ledger.lock")
			if tc.name == "read" {
				path = filepath.Join(f.home, "ledger.jsonl")
			}
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.Mkdir(path, 0o700))
			if tc.name == "backfill append" {
				f.transcript("session-one",
					userLine("session-one", f.folder, "2026-10-08T09:01:00Z", "오늘 가격 수집해줘"))
			}
			f.now = time.Date(2026, 10, 8, 9, 31, 0, 0, time.UTC)
			out, err := f.j.Tick(context.Background())
			assert.Error(t, err)
			assert.Empty(t, out.Verdicts)
		})
	}
}

func TestProbeFailure(t *testing.T) {
	t.Parallel()
	tcs := []struct{ name string }{
		{name: "invalid job"}, {name: "no scheduled slot"}, {name: "read"}, {name: "append"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			r := baseJob(f.folder)
			f.now = time.Date(2026, 10, 8, 9, 31, 0, 0, time.UTC)
			switch tc.name {
			case "invalid job":
				r.Grace = 0
			case "no scheduled slot":
				r.Schedule = "0 9 31 2 *"
			case "read":
				require.NoError(t, os.Mkdir(filepath.Join(f.home, "ledger.jsonl"), 0o700))
			case "append":
				require.NoError(t, os.Mkdir(filepath.Join(f.home, "ledger.lock"), 0o700))
			}
			_, err := f.j.Probe(context.Background(), r, f.now, "fingerprint")
			assert.Error(t, err)
		})
	}
}
