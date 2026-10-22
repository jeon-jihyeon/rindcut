package app_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func brokenLedger(t *testing.T) world {
	t.Helper()
	w := newWorld(t)
	require.NoError(t, os.Mkdir(w.a.Home.Path("ledger.jsonl"), 0o700))
	return w
}

func TestPreviewLedgerFailure(t *testing.T) {
	t.Parallel()
	tcs := []struct{ name string }{{name: "read"}, {name: "append"}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			path := w.a.Home.Path("ledger.jsonl")
			if tc.name == "append" {
				path = w.a.Home.Lock("ledger")
			}
			require.NoError(t, os.Mkdir(path, 0o700))
			_, err := w.a.Preview(context.Background(), w.job())
			assert.Error(t, err)
		})
	}
}

func TestSaveLedgerFailure(t *testing.T) {
	t.Parallel()
	tcs := []struct{ name string }{{name: "read"}, {name: "append"}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			path := w.a.Home.Path("ledger.jsonl")
			if tc.name == "append" {
				_, err := w.a.Preview(context.Background(), w.job())
				require.NoError(t, err)
				path = w.a.Home.Lock("ledger")
				require.NoError(t, os.Remove(path))
			}
			require.NoError(t, os.Mkdir(path, 0o700))
			_, err := w.a.Save(context.Background(), w.job())
			assert.Error(t, err)
		})
	}
}

func TestReportLedgerFailure(t *testing.T) {
	t.Parallel()
	w := brokenLedger(t)
	_, err := w.a.Report(context.Background(), 0)
	assert.Error(t, err)
}

func TestExportLedgerFailure(t *testing.T) {
	t.Parallel()
	w := brokenLedger(t)
	_, err := w.a.Export(context.Background(), "", false)
	assert.Error(t, err)
}

func TestSetExporterFileFailure(t *testing.T) {
	t.Parallel()
	tcs := []struct{ name string }{{name: "read"}, {name: "write"}, {name: "unknown name"}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			name := "nodloop"
			switch tc.name {
			case "read":
				require.NoError(t, os.Mkdir(w.a.Home.Path("exporters.json"), 0o700))
			case "write":
				require.NoError(t, os.Mkdir(w.a.Home.Path("exporters.json.tmp"), 0o700))
			case "unknown name":
				name = "unknown"
			}
			_, err := w.a.SetExporter(context.Background(), name, []byte(`{"enabled":false}`))
			assert.Error(t, err)
		})
	}
}
