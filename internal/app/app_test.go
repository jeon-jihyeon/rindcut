package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/app"
	"github.com/jeon-jihyeon/rindcut/internal/check"
	"github.com/jeon-jihyeon/rindcut/internal/home"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/judge"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
	"github.com/jeon-jihyeon/rindcut/internal/lock"
)

var edt = time.FixedZone("EDT", -4*3600)

type world struct {
	a      app.App
	now    *time.Time
	folder string
	warn   *bytes.Buffer
	// refuse makes the OS scheduler refuse the next installs
	refuse *bool
}

func newWorld(t *testing.T) world {
	t.Helper()
	now := time.Date(2026, 10, 8, 9, 10, 0, 0, edt)
	refuse := false
	w := world{now: &now, folder: t.TempDir(), warn: &bytes.Buffer{}, refuse: &refuse}
	userHome := t.TempDir()
	w.a = app.App{
		Home: home.Dir(t.TempDir()), UserHome: userHome, Now: func() time.Time { return *w.now }, Version: "test", Warn: w.warn,
		Installer: judge.Installer{Binary: "/bin/rindcut", Home: "/h", UserHome: userHome, GOOS: "darwin", UID: 501,
			Exec: func(name string, args ...string) error {
				if *w.refuse && len(args) > 0 && args[0] == "bootstrap" {
					return errors.New("refused")
				}
				return nil
			}},
	}
	require.NoError(t, w.a.Ready())
	return w
}

func (w world) job() job.Job {
	return job.Job{
		Name: "가격 수집", Schedule: "0 9 * * *", Timezone: "America/New_York", Grace: check.Duration(30 * time.Minute),
		Evidence: job.EvidenceSession, Folder: w.folder, Marker: "오늘 가격 수집해줘",
		Checks: []check.Spec{{Kind: check.KindFile, Path: "out/{{date}}.csv"}},
	}
}

func (w world) saved(t *testing.T, j job.Job) job.Job {
	t.Helper()
	ctx := context.Background()
	_, err := w.a.Preview(ctx, j)
	require.NoError(t, err)
	s, err := w.a.Save(ctx, j)
	require.NoError(t, err)
	return s.Job
}

func TestSaveNeedsAFreshPreviewOfTheSameJob(t *testing.T) {
	t.Parallel()
	type want struct {
		saved     bool
		revision  int
		installed bool
	}
	tcs := []struct {
		name string
		// prepare previews or saves first and returns the job to save
		prepare func(t *testing.T, w world) job.Job
		want    want
	}{
		{name: "no preview", prepare: func(_ *testing.T, w world) job.Job { return w.job() }, want: want{}},
		{name: "previewed", prepare: func(t *testing.T, w world) job.Job {
			_, err := w.a.Preview(context.Background(), w.job())
			require.NoError(t, err)
			return w.job()
		}, want: want{saved: true, revision: 1, installed: true}},
		{name: "preview too old", prepare: func(t *testing.T, w world) job.Job {
			_, err := w.a.Preview(context.Background(), w.job())
			require.NoError(t, err)
			*w.now = w.now.Add(31 * time.Minute)
			return w.job()
		}, want: want{}},
		{name: "changed after the preview", prepare: func(t *testing.T, w world) job.Job {
			_, err := w.a.Preview(context.Background(), w.job())
			require.NoError(t, err)
			j := w.job()
			j.Grace = check.Duration(time.Hour)
			return j
		}, want: want{}},
		{name: "second revision, tick already scheduled", prepare: func(t *testing.T, w world) job.Job {
			first := w.saved(t, w.job())
			j := w.job()
			j.ID, j.Grace = first.ID, check.Duration(time.Hour)
			_, err := w.a.Preview(context.Background(), j)
			require.NoError(t, err)
			return j
		}, want: want{saved: true, revision: 2, installed: false}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			s, err := w.a.Save(context.Background(), tc.prepare(t, w))
			if !tc.want.saved {
				assert.ErrorIs(t, err, app.ErrNoPreview)
			}
			assert.Equal(t, tc.want, want{saved: err == nil, revision: s.Job.Revision, installed: s.Installed})
		})
	}
}

func TestSaveRetriesARefusedSchedule(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	*w.refuse = true
	_, err := w.a.Preview(ctx, w.job())
	require.NoError(t, err)
	_, err = w.a.Save(ctx, w.job())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "job saved")
	assert.False(t, w.a.Installer.Installed())

	*w.refuse = false
	j := w.job()
	j.Marker = "오늘 가격 수집해줘 다시"
	_, err = w.a.Preview(ctx, j)
	require.NoError(t, err)
	s, err := w.a.Save(ctx, j)
	require.NoError(t, err)
	assert.True(t, s.Installed, "the next save schedules the tick")
}

func TestRemoveAndJobs(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	saved := w.saved(t, w.job())

	list, err := w.a.Jobs(ctx, false)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NotNil(t, list[0].NextSlot)
	assert.Equal(t, "2026-10-09T09:00:00-04:00", list[0].NextSlot.Format(time.RFC3339))

	removed, err := w.a.Remove(ctx, saved.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, removed.Revision)
	_, err = w.a.Remove(ctx, saved.ID)
	assert.ErrorIs(t, err, app.ErrNoJob)

	active, err := w.a.Jobs(ctx, false)
	require.NoError(t, err)
	assert.Empty(t, active)
	all, err := w.a.Jobs(ctx, true)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Nil(t, all[0].NextSlot)
}

func (w world) verdict(t *testing.T, jobID string, slot time.Time, v ledger.Verdict) ledger.Entry {
	t.Helper()
	e, err := w.a.Ledger().Append(context.Background(), ledger.KindVerdict, ledger.Judgment{
		JobID: jobID, SlotKey: jobID + "@" + slot.Format(time.RFC3339), Slot: slot, Verdict: v, Trigger: judge.TriggerTick,
		Checks: []ledger.CheckRecord{{Kind: "file", Status: string(v), Message: string(v) + " check"}},
	})
	require.NoError(t, err)
	return e
}

func TestReport(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		// at is when the report runs, the job's 09:00 slot closes at 09:30
		at       time.Time
		verdict  bool
		settings string
		want     []string
	}{
		{name: "judged slot is not silent", at: time.Date(2026, 10, 9, 10, 0, 0, 0, edt), verdict: true, want: []string{}},
		{name: "unjudged closed slot is silent", at: time.Date(2026, 10, 9, 10, 0, 0, 0, edt), want: []string{"job"}},
		{name: "a slot within two ticks of closing isn't silent yet", at: time.Date(2026, 10, 9, 9, 35, 0, 0, edt), want: []string{}},
		{name: "broken exporter settings still give a report", at: time.Date(2026, 10, 9, 10, 0, 0, 0, edt), verdict: true, settings: "{", want: []string{}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			saved := w.saved(t, w.job())
			if tc.verdict {
				w.verdict(t, saved.ID, time.Date(2026, 10, 9, 9, 0, 0, 0, edt), ledger.VerdictPass)
			}
			if tc.settings != "" {
				require.NoError(t, os.WriteFile(w.a.Home.Path("exporters.json"), []byte(tc.settings), 0o600))
			}
			*w.now = tc.at
			r, err := w.a.Report(context.Background(), 24*time.Hour)
			require.NoError(t, err)
			silent := []string{}
			for range r.Silent {
				silent = append(silent, "job")
			}
			assert.Equal(t, tc.want, silent)
			assert.Equal(t, tc.settings != "", len(r.Lag) == 1 && r.Lag[0].Exporter == "settings")
		})
	}
}

func TestVerdictsAndCorrect(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	day := func(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, edt) }
	w.verdict(t, "a", day(5), ledger.VerdictPass)
	wrong := w.verdict(t, "a", day(6), ledger.VerdictFail)
	w.verdict(t, "b", day(7), ledger.VerdictMissed)

	_, err := w.a.Correct(ctx, ledger.Correction{VerdictID: wrong.ID, Verdict: ledger.VerdictPass, Reason: "renamed", By: "owner"})
	require.NoError(t, err)
	for _, c := range []ledger.Correction{
		{VerdictID: "nope", Verdict: ledger.VerdictPass, Reason: "x", By: "owner"},
		{VerdictID: wrong.ID, Verdict: "great", Reason: "x", By: "owner"},
		{VerdictID: wrong.ID, Verdict: ledger.VerdictPass},
	} {
		_, err := w.a.Correct(ctx, c)
		assert.Error(t, err, "%+v", c)
	}

	tcs := []struct {
		name string
		f    app.VerdictFilter
		want []string
	}{
		{name: "all newest first", f: app.VerdictFilter{}, want: []string{"b@missed", "a@pass*", "a@pass"}},
		{name: "one job", f: app.VerdictFilter{JobID: "a"}, want: []string{"a@pass*", "a@pass"}},
		{name: "by verdict after corrections", f: app.VerdictFilter{Verdict: ledger.VerdictFail}, want: []string{}},
		{name: "since", f: app.VerdictFilter{Since: day(6)}, want: []string{"b@missed", "a@pass*"}},
		{name: "limit", f: app.VerdictFilter{Limit: 1}, want: []string{"b@missed"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			list, err := w.a.Verdicts(ctx, tc.f)
			require.NoError(t, err)
			got := []string{}
			for _, v := range list {
				s := v.Judgment.JobID + "@" + string(v.Judgment.Verdict)
				if v.Correction != nil {
					s = v.Judgment.JobID + "@" + string(v.Correction.Verdict) + "*"
				}
				got = append(got, s)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSetExporter(t *testing.T) {
	t.Parallel()
	ok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(ok.Close)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(down.Close)
	tcs := []struct {
		name, exporter, raw string
		wantTest            string
	}{
		{name: "otlp reachable", exporter: "otlp", raw: `{"enabled":true,"endpoint":"` + ok.URL + `"}`, wantTest: "ok"},
		{name: "langfuse wrong key", exporter: "langfuse-score", raw: `{"enabled":true,"host":"` + down.URL + `","public_key":"pk-lf-1","secret_key":"sk-lf-secretvalue"}`, wantTest: "401"},
		{name: "disabled", exporter: "langfuse-score", raw: `{"enabled":false}`, wantTest: "off"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			r, err := w.a.SetExporter(context.Background(), tc.exporter, json.RawMessage(tc.raw))
			require.NoError(t, err)
			assert.Contains(t, r.Test, tc.wantTest)
			b, err := json.Marshal(r)
			require.NoError(t, err)
			assert.NotContains(t, string(b), "secretvalue")
		})
	}
	w := newWorld(t)
	_, err := w.a.SetExporter(context.Background(), "slack", json.RawMessage(`{}`))
	assert.Error(t, err)
}

func transcript(t *testing.T, path, cwd, prompt string) {
	t.Helper()
	line, err := json.Marshal(map[string]any{"type": "user", "sessionId": "s", "cwd": cwd, "timestamp": "2026-10-08T13:01:00Z",
		"message": map[string]any{"role": "user", "content": prompt}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(line, '\n'), 0o600))
}

func TestRecordSession(t *testing.T) {
	t.Parallel()
	type want struct {
		recorded bool
		complete bool
	}
	tcs := []struct {
		name  string
		inJob bool
		file  bool
		want  want
	}{
		{name: "session in a job folder", inJob: true, file: true, want: want{recorded: true, complete: true}},
		{name: "unreadable transcript is kept incomplete for the backfill", inJob: true, want: want{recorded: true}},
		{name: "other work leaves no trace", file: true, want: want{}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.saved(t, w.job())
			cwd := t.TempDir()
			if tc.inJob {
				cwd = w.folder
			}
			path := filepath.Join(t.TempDir(), "t.jsonl")
			if tc.file {
				transcript(t, path, cwd, "오늘 가격 수집해줘")
			}
			recorded, err := w.a.RecordSession(context.Background(), ledger.Session{SessionID: "s", Cwd: cwd, TranscriptPath: path})
			require.NoError(t, err)
			entries, err := w.a.Ledger().All(context.Background())
			require.NoError(t, err)
			complete := false
			for _, e := range entries {
				var s ledger.Session
				if e.Kind == ledger.KindSession && e.Decode(&s) == nil {
					complete = s.Complete()
				}
			}
			assert.Equal(t, tc.want, want{recorded: recorded, complete: complete})
		})
	}
	w := newWorld(t)
	_, err := w.a.RecordSession(context.Background(), ledger.Session{})
	assert.Error(t, err)
}

func TestTickLockAndStatus(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	st, err := w.a.Status(ctx)
	require.NoError(t, err)
	assert.Nil(t, st.LastTick)

	unlock, err := lock.Acquire(ctx, w.a.Home.Lock("tick"), 0)
	require.NoError(t, err)
	busy, err := w.a.Tick(ctx)
	require.NoError(t, err)
	assert.True(t, busy.Busy)
	unlock()

	w.saved(t, w.job())
	*w.now = time.Date(2026, 10, 9, 9, 31, 0, 0, edt)
	r, err := w.a.Tick(ctx)
	require.NoError(t, err)
	assert.False(t, r.Busy)
	assert.Len(t, r.Verdicts, 1)

	st, err = w.a.Status(ctx)
	require.NoError(t, err)
	require.NotNil(t, st.LastTick)
	assert.Equal(t, app.Status{Version: "test", Home: string(w.a.Home), TickInstalled: true, LastTick: st.LastTick, Jobs: 1}, st)
	assert.Empty(t, w.warn.String())
}

func TestExportFromStart(t *testing.T) {
	t.Parallel()
	var got atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { got.Add(1) }))
	t.Cleanup(srv.Close)
	w := newWorld(t)
	ctx := context.Background()
	_, err := w.a.SetExporter(ctx, "otlp", json.RawMessage(`{"enabled":true,"endpoint":"`+srv.URL+`"}`))
	require.NoError(t, err)
	w.verdict(t, "a", time.Date(2026, 10, 8, 9, 0, 0, 0, edt), ledger.VerdictPass)

	tcs := []struct {
		name      string
		fromStart bool
		pending   int
		sends     int
	}{
		{name: "first export sends the verdict", sends: 1},
		{name: "nothing new sends nothing", sends: 0},
		{name: "from start resends", fromStart: true, sends: 1},
	}
	for _, tc := range tcs {
		before := got.Load()
		lags, err := w.a.Export(ctx, "", tc.fromStart)
		require.NoError(t, err, tc.name)
		require.Len(t, lags, 1, tc.name)
		assert.Equal(t, tc.pending, lags[0].Pending, tc.name)
		assert.Equal(t, int32(tc.sends), got.Load()-before, tc.name)
	}
}

func TestLedgerDamageIsReported(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	require.NoError(t, os.WriteFile(w.a.Home.Ledger(), []byte("not json\n"), 0o600))
	_, err := w.a.Status(context.Background())
	require.NoError(t, err)
	assert.True(t, strings.Contains(w.warn.String(), "skipped ledger line 1"))
}
