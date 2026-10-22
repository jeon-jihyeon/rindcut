package export_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/export"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

func nodloopFiles(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "nodloop")
	script := `#!/bin/sh
echo "$@" >> "` + log + `"
if [ "$1" = trace ]; then
  if [ -f "` + dir + `/trace-error" ]; then printf 'private recorded text' >&2; exit 3; fi
  if [ -f "` + dir + `/block-parent" ]; then printf x > "` + dir + `/state"; fi
  if [ -f "` + dir + `/block-rename" ]; then mkdir "` + dir + `/bindings.json"; fi
  if [ -f "` + dir + `/remove-binary" ]; then rm "$0"; fi
  if [ -f "` + dir + `/$4" ]; then cat "` + dir + `/$4"; else printf '[]'; fi
fi
if [ "$1" = feedback ] && [ -f "` + dir + `/outcome-error" ]; then exit 3; fi
`
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700))
	return bin, dir, log
}

func writeRuns(t *testing.T, dir, session, rows string) {
	t.Helper()
	var encoded []byte
	items := []map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(rows, "\n"), "\n") {
		if line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 3 {
			encoded = []byte(rows)
			break
		}
		items = append(items, map[string]string{"id": cols[0], "time": cols[1], "name": cols[2], "session_id": session})
	}
	if encoded == nil {
		var err error
		encoded, err = json.Marshal(items)
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, session), encoded, 0o600))
}

func TestNodloopCorrelation(t *testing.T) {
	t.Parallel()
	type args struct {
		rows, explicit string
		traceError     bool
		removeBinary   bool
	}
	type want struct {
		run              string
		failed, deferred bool
	}
	tcs := []struct {
		name string
		args args
		want want
	}{
		{name: "single candidate", args: args{rows: "r1\t2026-10-08T09:01:00Z\trun\n"}, want: want{run: "r1"}},
		{name: "newer run outside window", args: args{rows: "r1\t2026-10-08T09:01:00Z\trun\n" +
			"r2\t2026-10-08T10:00:00Z\trun\n"}, want: want{run: "r1"}},
		{name: "slot boundary", args: args{rows: "r1\t2026-10-08T09:00:00Z\trun\n"}, want: want{run: "r1"}},
		{name: "grace boundary", args: args{rows: "r1\t2026-10-08T09:30:00Z\trun\n"}, want: want{run: "r1"}},
		{name: "after grace by a nanosecond", args: args{rows: "r1\t2026-10-08T09:30:00.000000001Z\trun\n"},
			want: want{failed: true, deferred: true}},
		{name: "offset compared as time", args: args{rows: "r1\t2026-10-08T10:01:00+01:00\trun\n"},
			want: want{run: "r1"}},
		{name: "duplicate identity", args: args{rows: strings.Repeat("r1\t2026-10-08T09:01:00Z\trun\n", 2)},
			want: want{run: "r1"}},
		{name: "no trace yet", want: want{failed: true, deferred: true}},
		{name: "before slot", args: args{rows: "r1\t2026-10-08T08:59:59Z\trun\n"},
			want: want{failed: true, deferred: true}},
		{name: "late completion needs explicit identity", args: args{rows: "r1\t2026-10-08T10:00:00Z\trun\n"},
			want: want{failed: true, deferred: true}},
		{name: "ambiguous", args: args{rows: "r1\t2026-10-08T09:01:00Z\trun\n" +
			"r2\t2026-10-08T09:02:00Z\trun\n"}, want: want{failed: true, deferred: true}},
		{name: "another kind", args: args{rows: "x1\t2026-10-08T09:01:00Z\tcompaction\n"},
			want: want{failed: true, deferred: true}},
		{name: "explicit late completion", args: args{rows: "r1\t2026-10-08T10:00:00Z\trun\n", explicit: "r1"},
			want: want{run: "r1"}},
		{name: "explicit absent never falls back", args: args{rows: "r1\t2026-10-08T09:01:00Z\trun\n",
			explicit: "r2"}, want: want{failed: true, deferred: true}},
		{name: "malformed row", args: args{rows: "not a row\n"}, want: want{failed: true}},
		{name: "null is not an array", args: args{rows: "null"}, want: want{failed: true}},
		{name: "zero time", args: args{rows: "r1\t0001-01-01T00:00:00Z\trun\n"}, want: want{failed: true}},
		{name: "explicit run from another session", args: args{
			rows: `[{"id":"r1","time":"2026-10-08T09:01:00Z","name":"run","session_id":"another"}]`, explicit: "r1"},
			want: want{failed: true, deferred: true}},
		{name: "invalid time", args: args{rows: "r1\tinvalid\trun\n"}, want: want{failed: true}},
		{name: "invalid time never leaks record text", args: args{rows: "r1\tprivate recorded text\trun\n"},
			want: want{failed: true}},
		{name: "empty identity", args: args{rows: "\t2026-10-08T09:01:00Z\trun\n"}, want: want{failed: true}},
		{name: "overlong row", args: args{rows: strings.Repeat("a", 65537)}, want: want{failed: true}},
		{name: "trace command fails", args: args{traceError: true}, want: want{failed: true}},
		{name: "binary disappears before outcome", args: args{rows: "r1\t2026-10-08T09:01:00Z\trun\n",
			removeBinary: true}, want: want{failed: true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin, dir, log := nodloopFiles(t)
			writeRuns(t, dir, "sess-1", tc.args.rows)
			if tc.args.traceError {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "trace-error"), nil, 0o600))
			}
			if tc.args.removeBinary {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "remove-binary"), nil, 0o600))
			}
			e := verdict(t, "v1", ledger.VerdictFail, "sess-1")
			var j ledger.Judgment
			require.NoError(t, e.Decode(&j))
			j.RunID = tc.args.explicit
			e = entry(t, e.ID, e.Kind, j)
			es := []ledger.Entry{e}
			n := export.NewNodloop(bin, filepath.Join(dir, "bindings.json"), es)
			err := n.Export(context.Background(), es)
			calls, readErr := os.ReadFile(log)
			require.NoError(t, readErr)
			var run string
			for _, line := range strings.Split(string(calls), "\n") {
				if strings.HasPrefix(line, "feedback outcome --trace ") {
					run = strings.Fields(line)[3]
				}
			}
			assert.Equal(t, tc.want, want{run: run, failed: err != nil, deferred: errors.Is(err, export.ErrDeferred)})
			assert.NotContains(t, fmt.Sprint(err), "private recorded text")
		})
	}
}

func TestNodloopPendingAcrossRestarts(t *testing.T) {
	t.Parallel()
	bin, dir, log := nodloopFiles(t)
	ctx := context.Background()
	es := []ledger.Entry{
		verdict(t, "v1", ledger.VerdictFail, "late"),
		verdict(t, "v2", ledger.VerdictPass, "ready"),
		entry(t, "c1", ledger.KindCorrection,
			ledger.Correction{VerdictID: "v1", Verdict: ledger.VerdictMissed, Reason: "wrong session", By: "ann"}),
	}
	writeRuns(t, dir, "ready", "r2\t2026-10-08T09:02:00Z\trun\n")
	state := filepath.Join(t.TempDir(), "cursors")
	for range 2 {
		n := export.NewNodloop(bin, filepath.Join(state, "nodloop-bindings.json"), es)
		r := export.Runner{Exporters: []export.Exporter{n}, CursorDir: state, Now: func() time.Time { return t0 }}
		assert.Equal(t, []export.Lag{{Exporter: export.NameNodloop, Pending: 2}}, r.Run(ctx, es))
		assert.Equal(t, []export.Lag{{Exporter: export.NameNodloop, Pending: 2}},
			export.Lags(state, []string{export.NameNodloop}, es))
	}
	writeRuns(t, dir, "late", "r1\t2026-10-08T09:01:00Z\trun\n")
	n := export.NewNodloop(bin, filepath.Join(state, "nodloop-bindings.json"), es)
	r := export.Runner{Exporters: []export.Exporter{n}, CursorDir: state, Now: func() time.Time { return t0 }}
	assert.Equal(t, []export.Lag{{Exporter: export.NameNodloop}}, r.Run(ctx, es))
	assert.Equal(t, []export.Lag{{Exporter: export.NameNodloop}}, r.Run(ctx, es))
	calls, err := os.ReadFile(log)
	require.NoError(t, err)
	var outcomes []string
	for _, line := range strings.Split(string(calls), "\n") {
		if strings.HasPrefix(line, "feedback ") {
			outcomes = append(outcomes, line)
		}
	}
	require.Len(t, outcomes, 3)
	assert.Contains(t, outcomes[0], "--trace r2 --result confirmed --actor code")
	assert.Contains(t, outcomes[1], "--trace r1 --result refuted --actor code")
	assert.Contains(t, outcomes[2], "--trace r1 --result inconclusive --actor code")
	b, err := os.ReadFile(filepath.Join(state, "nodloop.json"))
	require.NoError(t, err)
	var cursor export.Cursor
	require.NoError(t, json.Unmarshal(b, &cursor))
	assert.Equal(t, "c1", cursor.Last)
	assert.Empty(t, cursor.Pending)
}

func TestNodloopBindingSurvivesReset(t *testing.T) {
	t.Parallel()
	bin, dir, log := nodloopFiles(t)
	ctx := context.Background()
	writeRuns(t, dir, "sess-1", "r1\t2026-10-08T09:01:00Z\trun\n")
	es := []ledger.Entry{verdict(t, "v1", ledger.VerdictFail, "sess-1")}
	state := t.TempDir()
	binding := filepath.Join(state, "nodloop-bindings.json")
	r := export.Runner{Exporters: []export.Exporter{export.NewNodloop(bin, binding, es)},
		CursorDir: state, Now: func() time.Time { return t0 }}
	assert.Equal(t, []export.Lag{{Exporter: export.NameNodloop}}, r.Run(ctx, es))
	info, err := os.Stat(binding)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	writeRuns(t, dir, "sess-1", "r2\t2026-10-08T09:02:00Z\trun\n")
	es = append(es, entry(t, "c1", ledger.KindCorrection,
		ledger.Correction{VerdictID: "v1", Verdict: ledger.VerdictPass, Reason: "checked", By: "ann"}))
	r.Exporters = []export.Exporter{export.NewNodloop(bin, binding, es)}
	assert.Equal(t, []export.Lag{{Exporter: export.NameNodloop}}, r.Run(ctx, es))
	require.NoError(t, export.Reset(state, export.NameNodloop))
	r.Exporters = []export.Exporter{export.NewNodloop(bin, binding, es)}
	assert.Equal(t, []export.Lag{{Exporter: export.NameNodloop}}, r.Run(ctx, es))
	calls, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(calls), "trace list"))
	assert.Equal(t, 4, strings.Count(string(calls), "feedback outcome --trace r1"))
	assert.NotContains(t, string(calls), "--trace r2")
}

func TestNodloopBindingErrors(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name                     string
		initial                  string
		directory, tempDirectory bool
		blockParent, blockRename bool
	}{
		{name: "malformed binding", initial: "{"},
		{name: "null binding", initial: "null"},
		{name: "saved binding has no run", initial: `{"v1":{"job_id":"price-watch","revision":2,` +
			`"slot_key":"price-watch@2026-10-08T09:00:00Z","session":"sess-1"}}`},
		{name: "binding cannot be read", directory: true},
		{name: "temporary binding cannot be written", tempDirectory: true},
		{name: "binding parent cannot be created", blockParent: true},
		{name: "binding cannot be renamed", blockRename: true},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin, dir, log := nodloopFiles(t)
			writeRuns(t, dir, "sess-1", "r1\t2026-10-08T09:01:00Z\trun\n")
			path := filepath.Join(dir, "bindings.json")
			if tc.blockParent {
				path = filepath.Join(dir, "state", "bindings.json")
				require.NoError(t, os.WriteFile(filepath.Join(dir, "block-parent"), nil, 0o600))
			}
			if tc.blockRename {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "block-rename"), nil, 0o600))
			}
			if tc.initial != "" {
				require.NoError(t, os.WriteFile(path, []byte(tc.initial), 0o600))
			}
			if tc.directory {
				require.NoError(t, os.Mkdir(path, 0o700))
			}
			if tc.tempDirectory {
				require.NoError(t, os.Mkdir(path+".tmp", 0o700))
			}
			es := []ledger.Entry{verdict(t, "v1", ledger.VerdictFail, "sess-1")}
			n := export.NewNodloop(bin, path, es)
			err := n.Export(context.Background(), es)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "nodloop binding")
			calls, _ := os.ReadFile(log)
			assert.NotContains(t, string(calls), "feedback outcome")
			if tc.blockRename {
				assert.NoFileExists(t, path+".tmp")
			}
		})
	}
}

func TestNodloopBindingIdentity(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name     string
		revision int
		explicit string
	}{
		{name: "ledger revision changed", revision: 3},
		{name: "explicit run changed", revision: 2, explicit: "r2"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin, dir, log := nodloopFiles(t)
			writeRuns(t, dir, "sess-1", "r1\t2026-10-08T09:01:00Z\trun\n")
			es := []ledger.Entry{verdict(t, "v1", ledger.VerdictFail, "sess-1")}
			path := filepath.Join(dir, "bindings.json")
			require.NoError(t, export.NewNodloop(bin, path, es).Export(context.Background(), es))
			var j ledger.Judgment
			require.NoError(t, es[0].Decode(&j))
			j.JobRevision, j.RunID = tc.revision, tc.explicit
			es[0] = entry(t, "v1", ledger.KindVerdict, j)
			err := export.NewNodloop(bin, path, es).Export(context.Background(), es)
			require.ErrorContains(t, err, "binding identity changed")
			calls, readErr := os.ReadFile(log)
			require.NoError(t, readErr)
			assert.Equal(t, 1, strings.Count(string(calls), "feedback outcome"))
		})
	}
}

func TestNodloopIgnoredEntries(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		kind ledger.Kind
		data string
	}{
		{name: "session stays local", kind: ledger.KindSession, data: `{"session_id":"session-one"}`},
		{name: "unreadable verdict", kind: ledger.KindVerdict, data: `{`},
		{name: "unreadable correction", kind: ledger.KindCorrection, data: `{`},
		{name: "correction without original verdict", kind: ledger.KindCorrection,
			data: `{"verdict_id":"missing","verdict":"pass","reason":"checked","by":"ann"}`},
		{name: "unknown verdict", kind: ledger.KindVerdict,
			data: `{"verdict":"invalid","sessions":["session-one"]}`},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin, dir, log := nodloopFiles(t)
			es := []ledger.Entry{{ID: "entry-one", Kind: tc.kind, Data: json.RawMessage(tc.data)}}
			err := export.NewNodloop(bin, filepath.Join(dir, "bindings.json"), es).Export(context.Background(), es)
			assert.NoError(t, err)
			assert.NoFileExists(t, log)
		})
	}
}

func TestNodloopUnacknowledgedOutcome(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name        string
		marker      string
		wantPending int
		wantError   string
	}{
		{name: "outcome rejected or record storage unavailable", marker: "outcome-error", wantPending: 1,
			wantError: "nodloop feedback: exit status 3"},
		{name: "old binary rejects machine format", marker: "trace-error", wantPending: 1,
			wantError: "check binary compatibility and record access"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin, dir, _ := nodloopFiles(t)
			writeRuns(t, dir, "sess-1", "r1\t2026-10-08T09:01:00Z\trun\n")
			require.NoError(t, os.WriteFile(filepath.Join(dir, tc.marker), nil, 0o600))
			es := []ledger.Entry{verdict(t, "v1", ledger.VerdictFail, "sess-1")}
			state := t.TempDir()
			binding := filepath.Join(state, "nodloop-bindings.json")
			r := export.Runner{Exporters: []export.Exporter{export.NewNodloop(bin, binding, es)},
				CursorDir: state, Now: func() time.Time { return t0 }}
			lags := r.Run(context.Background(), es)
			require.Len(t, lags, 1)
			assert.Equal(t, tc.wantPending, lags[0].Pending)
			assert.Zero(t, lags[0].Dropped)
			assert.Contains(t, lags[0].LastError, tc.wantError)
			assert.NotContains(t, lags[0].LastError, "private recorded text")
			assert.Equal(t, &t0, lags[0].FailedAt)
			assert.Equal(t, lags, export.Lags(state, []string{export.NameNodloop}, es))
			require.NoError(t, os.Remove(filepath.Join(dir, tc.marker)))
			r.Exporters = []export.Exporter{export.NewNodloop(bin, binding, es)}
			assert.Equal(t, []export.Lag{{Exporter: export.NameNodloop}}, r.Run(context.Background(), es))
		})
	}
}
