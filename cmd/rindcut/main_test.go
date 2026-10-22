package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/check"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

// main can't be imported, so the CLI is tested through run inside its package
var now = time.Date(2026, 10, 8, 9, 40, 0, 0, time.UTC)

type result struct {
	code           int
	stdout, stderr string
}

func cli(t *testing.T, rindcutHome, stdin string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	vars := map[string]string{"RINDCUT_HOME": rindcutHome, "HOME": filepath.Dir(rindcutHome), "PATH": "/usr/bin:/bin"}
	code := run(args, env{
		getenv: func(k string) string { return vars[k] },
		now:    func() time.Time { return now },
		stdin:  strings.NewReader(stdin), stdout: &out, stderr: &errOut,
	})
	return result{code, out.String(), errOut.String()}
}

func newHome(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".rindcut")
}

func TestExitCodes(t *testing.T) {
	t.Parallel()
	type want struct {
		code   int
		stdout string
		stderr string
	}
	tcs := []struct {
		name string
		args []string
		want want
	}{
		{name: "version", args: []string{"version"}, want: want{code: 0, stdout: "dev"}},
		{name: "no command", args: nil, want: want{code: 2, stderr: "usage:"}},
		{name: "unknown command", args: []string{"nope"}, want: want{code: 2, stderr: "unknown command"}},
		{name: "extra argument", args: []string{"check", "a", "b"}, want: want{code: 2, stderr: "want 1 argument"}},
		{name: "json error", args: []string{"--json", "check", "nope"}, want: want{code: 1, stdout: `"error": "nope: no such job"`}},
		{name: "status table", args: []string{"status"}, want: want{code: 0, stdout: "TICK SCHEDULED"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := cli(t, newHome(t), "", tc.args...)
			assert.Equal(t, tc.want.code, r.code, r.stderr)
			assert.Contains(t, r.stdout, tc.want.stdout)
			assert.Contains(t, r.stderr, tc.want.stderr)
		})
	}
}

func TestVersionCreatesNoHome(t *testing.T) {
	t.Parallel()
	h := newHome(t)
	require.Equal(t, 0, cli(t, h, "", "version").code)
	assert.NoDirExists(t, h)
	require.Equal(t, 0, cli(t, h, "", "status").code)
	info, err := os.Stat(h)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// seed records a session job for folder straight into the ledger, as the plugin's job_save would
func seed(t *testing.T, h, folder string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(h, 0o700))
	l := ledger.Open(filepath.Join(h, "ledger.jsonl"), filepath.Join(h, "ledger.lock"), func() time.Time { return now.Add(-time.Hour) }, "test",
		func(n int, err error) { t.Errorf("ledger line %d: %v", n, err) })
	_, err := l.Append(context.Background(), ledger.KindJob, job.Job{
		ID: "price", Name: "가격", Schedule: "0 9 * * *", Timezone: "UTC", Grace: check.Duration(30 * time.Minute),
		Evidence: job.EvidenceSession, Folder: folder, Marker: "오늘 가격 수집해줘",
		Checks: []check.Spec{{Kind: check.KindFile, Path: "out.csv"}}, Revision: 1,
	})
	require.NoError(t, err)
}

func TestHook(t *testing.T) {
	t.Parallel()
	type want struct {
		sessions int
		prompt   bool
		quick    bool
	}
	tcs := []struct {
		name     string
		inFolder bool
		want     want
	}{
		{name: "session in a job folder", inFolder: true, want: want{sessions: 1, prompt: true, quick: true}},
		{name: "other work", inFolder: false, want: want{sessions: 0, quick: true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			user := t.TempDir()
			h, folder := filepath.Join(user, ".rindcut"), t.TempDir()
			seed(t, h, folder)
			cwd := t.TempDir()
			if tc.inFolder {
				cwd = folder
			}
			transcript := filepath.Join(user, "t.jsonl")
			// A large transcript is read only at its head
			first := `{"type":"user","sessionId":"abc","cwd":"` + cwd + `","timestamp":"2026-10-08T00:00:03Z","message":{"role":"user","content":"오늘 가격 수집해줘"}}` + "\n"
			rest := strings.Repeat(`{"type":"assistant","message":{"role":"assistant","content":"`+strings.Repeat("x", 1000)+`"}}`+"\n", 5000)
			require.NoError(t, os.WriteFile(transcript, []byte(first+rest), 0o600))
			in := `{"session_id":"abc","transcript_path":"` + transcript + `","cwd":"` + cwd + `","hook_event_name":"SessionEnd","reason":"other"}`

			start := time.Now()
			r := cli(t, h, in, "hook", "session-end")
			quick := time.Since(start) < time.Second
			require.Equal(t, 0, r.code)
			require.Empty(t, r.stderr)

			out := cli(t, h, "", "ledger", "--kind", "session").stdout
			assert.Equal(t, tc.want, want{
				sessions: strings.Count(out, `"kind":"session"`),
				prompt:   strings.Contains(out, `"first_prompt":"오늘 가격 수집해줘"`),
				quick:    quick,
			})
		})
	}
}

func TestHookNeverFails(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name, in, event string
	}{
		{name: "empty", in: "", event: "session-end"},
		{name: "not json", in: "not json", event: "session-end"},
		{name: "no session id", in: `{"session_id":""}`, event: "session-end"},
		{name: "wrong subcommand", in: "{}", event: "session-start"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := cli(t, newHome(t), tc.in, "hook", tc.event)
			assert.Equal(t, 0, r.code)
			assert.NotEmpty(t, r.stderr, "the hook says why on stderr")
		})
	}
}

func TestHookRunID(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		run  string
	}{
		{name: "ordinary hook leaves reference empty"},
		{name: "producer supplies exact run", run: "run-one"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, folder := newHome(t), t.TempDir()
			seed(t, h, folder)
			transcript := filepath.Join(t.TempDir(), "session.jsonl")
			first := `{"type":"user","sessionId":"session-one","timestamp":"2026-10-08T09:01:00Z",` +
				`"message":{"role":"user","content":"오늘 가격 수집해줘"}}` + "\n"
			require.NoError(t, os.WriteFile(transcript, []byte(first), 0o600))
			input, err := json.Marshal(map[string]string{
				"session_id": "session-one", "run_id": tc.run, "transcript_path": transcript, "cwd": folder,
			})
			require.NoError(t, err)
			hook := cli(t, h, string(input), "hook", "session-end")
			require.Equal(t, 0, hook.code)
			require.Empty(t, hook.stderr)
			tick := cli(t, h, "", "tick")
			require.Equal(t, 0, tick.code, tick.stderr)
			out := cli(t, h, "", "ledger", "--kind", "verdict")
			require.Equal(t, 0, out.code, out.stderr)
			var e ledger.Entry
			require.NoError(t, json.Unmarshal([]byte(out.stdout), &e))
			var j ledger.Judgment
			require.NoError(t, e.Decode(&j))
			assert.Equal(t, tc.run, j.RunID)
			assert.Equal(t, []string{"session-one"}, j.Sessions)
		})
	}
}

func TestTickReportExport(t *testing.T) {
	t.Parallel()
	h := newHome(t)
	seed(t, h, t.TempDir())
	tick := cli(t, h, "", "--json", "tick")
	require.Equal(t, 0, tick.code, tick.stderr)
	var r struct {
		Busy     bool             `json:"busy"`
		Verdicts []ledger.Entry   `json:"verdicts"`
		Lag      []map[string]any `json:"lag"`
	}
	require.NoError(t, json.Unmarshal([]byte(tick.stdout), &r))
	assert.False(t, r.Busy)
	assert.Len(t, r.Verdicts, 1, "the 09:00 slot closed at 09:30")
	assert.FileExists(t, filepath.Join(h, "last-tick"))

	tcs := []struct {
		name string
		args []string
		want string
	}{
		{name: "report", args: []string{"report"}, want: "missed"},
		{name: "jobs", args: []string{"jobs"}, want: "missed"},
		{name: "export with nothing enabled", args: []string{"export"}, want: "EXPORTER"},
		{name: "check", args: []string{"check", "price"}, want: "verdict"},
		{name: "ledger of a job", args: []string{"ledger", "--job", "price", "--kind", "verdict"}, want: `"job_id":"price"`},
	}
	for _, tc := range tcs {
		res := cli(t, h, "", tc.args...)
		assert.Equal(t, 0, res.code, tc.name+": "+res.stderr)
		assert.Contains(t, res.stdout, tc.want, tc.name)
	}
}

func TestReportDetails(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name   string
		judged bool
		want   string
	}{
		{name: "unjudged counts", want: "1 closed slots with no verdict, 0 past the lookback"},
		{name: "late session", judged: true, want: "a session started late at 2026-10-08T09:35:00Z"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, folder := newHome(t), t.TempDir()
			seed(t, h, folder)
			if tc.judged {
				l := ledger.Open(filepath.Join(h, "ledger.jsonl"), filepath.Join(h, "ledger.lock"),
					func() time.Time { return now }, "test", func(n int, err error) { t.Errorf("line %d: %v", n, err) })
				slot := now.Add(-40 * time.Minute)
				_, err := l.Append(context.Background(), ledger.KindVerdict, ledger.Judgment{
					JobID: "price", JobRevision: 1, SlotKey: "price@2026-10-08T09:00:00Z", Slot: slot,
					WindowEnd: slot.Add(30 * time.Minute), Verdict: ledger.VerdictMissed})
				require.NoError(t, err)
				_, err = l.Append(context.Background(), ledger.KindSession, ledger.Session{
					SessionID: "s1", Cwd: folder, StartedAt: now.Add(-5 * time.Minute), FirstPrompt: "오늘 가격 수집해줘"})
				require.NoError(t, err)
			}
			r := cli(t, h, "", "report")
			assert.Equal(t, 0, r.code)
			assert.Empty(t, r.stderr)
			assert.Contains(t, r.stdout, tc.want)
		})
	}
}
