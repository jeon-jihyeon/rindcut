package ledger_test

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
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

func TestAppendJudgmentBudget(t *testing.T) {
	t.Parallel()
	type args struct {
		kind    ledger.Kind
		text    string
		pointer bool
	}
	tcs := []struct {
		name string
		args args
	}{
		{name: "escaped verdict", args: args{kind: ledger.KindVerdict, text: strings.Repeat("<\\\n", 4096)}},
		{name: "multibyte probe", args: args{kind: ledger.KindProbe, text: strings.Repeat("文", 4096), pointer: true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, path, _ := open(t)
			j := ledger.Judgment{JobID: "report", JobRevision: 3, SlotKey: "report@slot", Verdict: ledger.VerdictFail,
				Sessions: []string{"session"}, Fingerprint: "definition", Trigger: "check", Note: "no session matched"}
			for range 10 {
				j.Checks = append(j.Checks, ledger.CheckRecord{Kind: "http", Status: "fail", Message: "status mismatch",
					Observed: map[string]any{"body": tc.args.text, "stdout": tc.args.text, "match": "found", "match_offset": 5000,
						"status": 404, "size": int64(9007199254740993)}})
			}
			var value any = j
			if tc.args.pointer {
				value = &j
			}
			e, err := l.Append(context.Background(), tc.args.kind, value)
			require.NoError(t, err)
			line, err := json.Marshal(e)
			require.NoError(t, err)
			assert.Less(t, len(line), 64<<10)
			assert.True(t, strings.Contains(string(line), `"size":9007199254740993`))
			stored, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(line)+"\n", string(stored))
			var got ledger.Judgment
			require.NoError(t, e.Decode(&got))
			assert.Equal(t, j.JobID, got.JobID)
			assert.Equal(t, j.JobRevision, got.JobRevision)
			assert.Equal(t, j.SlotKey, got.SlotKey)
			assert.Equal(t, j.Verdict, got.Verdict)
			assert.Equal(t, j.Sessions, got.Sessions)
			assert.Equal(t, j.Fingerprint, got.Fingerprint)
			assert.Equal(t, j.Note, got.Note)
			require.Len(t, got.Checks, 10)
			cuts := 0
			for _, c := range got.Checks {
				assert.Equal(t, "fail", c.Status)
				assert.Equal(t, "found", c.Observed["match"])
				assert.Equal(t, float64(5000), c.Observed["match_offset"])
				assert.Equal(t, float64(404), c.Observed["status"])
				text, ok := c.Observed["body"].(string)
				require.True(t, ok)
				assert.True(t, utf8.ValidString(text))
				if c.Truncated {
					cuts++
				}
			}
			assert.Positive(t, cuts)
			assert.Equal(t, tc.args.text, j.Checks[0].Observed["body"])
		})
	}
}

func TestAppendMandatoryBudget(t *testing.T) {
	t.Parallel()
	for _, kind := range []ledger.Kind{ledger.KindVerdict, ledger.KindProbe} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			l, path, _ := open(t)
			_, err := l.Append(context.Background(), kind, ledger.Judgment{
				JobID: "report", Verdict: ledger.VerdictPass, Fingerprint: strings.Repeat("x", 64<<10)})
			assert.ErrorIs(t, err, ledger.ErrLineTooLong)
			_, err = os.Stat(path)
			assert.True(t, os.IsNotExist(err))
		})
	}
}

func TestAppendExactJudgmentBoundary(t *testing.T) {
	t.Parallel()
	for _, extra := range []int{-1, 0} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			t.Parallel()
			l, _, _ := open(t)
			j := ledger.Judgment{JobID: "report", Verdict: ledger.VerdictPass, Fingerprint: "x"}
			e, err := l.Append(context.Background(), ledger.KindProbe, j)
			require.NoError(t, err)
			b, err := json.Marshal(e)
			require.NoError(t, err)
			j.Fingerprint = strings.Repeat("x", (64<<10)-len(b)+1+extra)
			_, err = l.Append(context.Background(), ledger.KindProbe, j)
			assert.Equal(t, extra == 0, errors.Is(err, ledger.ErrLineTooLong))
		})
	}
}

func TestAppendJudgmentMessages(t *testing.T) {
	t.Parallel()
	l, _, _ := open(t)
	text := strings.Repeat("文", 64<<10)
	j := ledger.Judgment{JobID: "report", Verdict: ledger.VerdictFail, Note: "no session matched",
		Checks: []ledger.CheckRecord{{Kind: "command", Status: "fail", Message: text,
			Observed: map[string]any{"exit_code": 1}}}}
	e, err := l.Append(context.Background(), ledger.KindVerdict, j)
	require.NoError(t, err)
	var got ledger.Judgment
	require.NoError(t, e.Decode(&got))
	require.Len(t, got.Checks, 1)
	assert.Equal(t, j.Note, got.Note)
	assert.Equal(t, "fail", got.Checks[0].Status)
	assert.True(t, got.Checks[0].Truncated)
	assert.True(t, utf8.ValidString(got.Checks[0].Message))
	assert.Less(t, len(got.Checks[0].Message), len(text))
	assert.Equal(t, text, j.Checks[0].Message)
}

func TestAppendBudgetErrors(t *testing.T) {
	t.Parallel()
	type args struct {
		kind    ledger.Kind
		data    any
		version string
		at      time.Time
	}
	large := strings.Repeat("x", 64<<10)
	tcs := []struct {
		name string
		args args
		want string
	}{
		{name: "nil judgment with large metadata",
			args: args{kind: ledger.KindProbe, data: (*ledger.Judgment)(nil), version: large, at: t0},
			want: "ledger entry over 64 KB"},
		{name: "untyped payload remains intact",
			args: args{kind: ledger.KindProbe, data: map[string]any{"unknown": large}, version: "test", at: t0},
			want: "ledger entry over 64 KB"},
		{name: "correction reason remains intact",
			args: args{kind: ledger.KindCorrection, data: ledger.Correction{Reason: large}, version: "test", at: t0},
			want: "ledger entry over 64 KB"},
		{name: "note remains intact",
			args: args{kind: ledger.KindVerdict, data: ledger.Judgment{Note: large}, version: "test", at: t0},
			want: "mandatory fields exceed"},
		{name: "exact match remains intact",
			args: args{kind: ledger.KindProbe, data: ledger.Judgment{Checks: []ledger.CheckRecord{
				{Observed: map[string]any{"match": large, "body": large}}}}, version: "test", at: t0},
			want: "mandatory fields exceed"},
		{name: "unsupported payload",
			args: args{kind: ledger.KindProbe, data: make(chan int), version: "test", at: t0}, want: "unsupported type"},
		{name: "invalid entry timestamp",
			args: args{kind: ledger.KindProbe, data: ledger.Judgment{}, version: "test",
				at: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}, want: "year outside"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "ledger.jsonl")
			sk := &skips{}
			l := ledger.Open(path, filepath.Join(dir, "lock"), func() time.Time { return tc.args.at }, tc.args.version, sk.add)
			_, err := l.Append(context.Background(), tc.args.kind, tc.args.data)
			assert.ErrorContains(t, err, tc.want)
			_, err = os.Stat(path)
			assert.True(t, os.IsNotExist(err))
		})
	}
}

func TestAppendKeepsOutputEnds(t *testing.T) {
	t.Parallel()
	for _, kind := range []ledger.Kind{ledger.KindVerdict, ledger.KindProbe} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			l, _, _ := open(t)
			stdout := "collected 3 items\n" + strings.Repeat("文", 40<<10) + "\nFAILED test_rows"
			j := ledger.Judgment{JobID: "report", Verdict: ledger.VerdictFail,
				Checks: []ledger.CheckRecord{{Kind: "command", Status: "fail", Message: "exited 1",
					Observed: map[string]any{"stdout": stdout, "exit_code": 1}}}}
			e, err := l.Append(context.Background(), kind, j)
			require.NoError(t, err)
			var got ledger.Judgment
			require.NoError(t, e.Decode(&got))
			require.Len(t, got.Checks, 1)
			text, ok := got.Checks[0].Observed["stdout"].(string)
			require.True(t, ok)
			assert.True(t, utf8.ValidString(text))
			assert.True(t, strings.HasPrefix(text, "collected 3 items\n"))
			assert.True(t, strings.HasSuffix(text, "\nFAILED test_rows"))
			assert.Contains(t, text, "…")
			assert.Less(t, len(text), len(stdout))
			assert.True(t, got.Checks[0].Truncated)
		})
	}
}
