package judge_test

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

	"github.com/jeon-jihyeon/rindcut/internal/check"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/judge"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

var edt = time.FixedZone("EDT", -4*3600)

func TestDecide(t *testing.T) {
	t.Parallel()
	pass, fail, errd := check.Result{Status: check.StatusPass}, check.Result{Status: check.StatusFail}, check.Result{Status: check.StatusError}
	type want struct {
		verdict ledger.Verdict
		noted   bool
	}
	tcs := []struct {
		name    string
		results []check.Result
		ev      job.Evidence
		matched bool
		want    want
	}{
		{name: "all pass with session", results: []check.Result{pass, pass}, ev: job.EvidenceSession, matched: true, want: want{verdict: ledger.VerdictPass}},
		{name: "all pass, session unmatched", results: []check.Result{pass}, ev: job.EvidenceSession, want: want{verdict: ledger.VerdictPass, noted: true}},
		{name: "fail with session", results: []check.Result{pass, fail}, ev: job.EvidenceSession, matched: true, want: want{verdict: ledger.VerdictFail}},
		{name: "fail without session", results: []check.Result{fail}, ev: job.EvidenceSession, want: want{verdict: ledger.VerdictMissed, noted: true}},
		{name: "fail on cloud job", results: []check.Result{fail}, ev: job.EvidenceNone, want: want{verdict: ledger.VerdictFail}},
		{name: "fail beats error", results: []check.Result{errd, fail}, ev: job.EvidenceNone, want: want{verdict: ledger.VerdictFail}},
		{name: "error only", results: []check.Result{pass, errd}, ev: job.EvidenceSession, matched: true, want: want{verdict: ledger.VerdictInconclusive}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, note := judge.Decide(tc.results, tc.ev, tc.matched)
			assert.Equal(t, tc.want, want{verdict: v, noted: note != ""})
		})
	}
}

func baseJob(folder string) job.Job {
	return job.Job{
		ID: "price-watch", Name: "가격 수집", Schedule: "0 9 * * *", Timezone: "America/New_York",
		Grace: check.Duration(30 * time.Minute), Evidence: job.EvidenceSession,
		Folder: folder, Marker: "오늘 가격 수집해줘",
		Checks:   []check.Spec{{Kind: check.KindFile, Path: "out/{{date}}.csv"}},
		Revision: 1,
	}
}

func TestMatches(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))
	other := t.TempDir()
	j := baseJob(dir)
	slots, err := j.Slots(time.Date(2026, 10, 8, 0, 0, 0, 0, edt), time.Date(2026, 10, 9, 0, 0, 0, 0, edt))
	require.NoError(t, err)
	s := slots[0]
	tcs := []struct {
		name   string
		cwd    string
		start  time.Time
		prompt string
		want   bool
	}{
		{name: "match", cwd: dir, start: s.At.Add(time.Minute), prompt: "오늘 가격 수집해줘", want: true},
		{name: "symlinked folder", cwd: link, start: s.At.Add(time.Minute), prompt: "오늘 가격 수집해줘", want: true},
		{name: "marker with spaces", cwd: dir, start: s.At.Add(time.Minute), prompt: "  오늘 가격 수집해줘  ", want: true},
		{name: "other folder", cwd: other, start: s.At.Add(time.Minute), prompt: "오늘 가격 수집해줘"},
		{name: "before the slot", cwd: dir, start: s.At.Add(-time.Second), prompt: "오늘 가격 수집해줘"},
		{name: "after the window", cwd: dir, start: s.End.Add(time.Second), prompt: "오늘 가격 수집해줘"},
		{name: "hand typed prompt", cwd: dir, start: s.At.Add(time.Minute), prompt: "가격 파일 열어줘"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := judge.Matches(j, s, ledger.Session{Cwd: tc.cwd, StartedAt: tc.start, FirstPrompt: tc.prompt})
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSessionFolders(t *testing.T) {
	t.Parallel()
	a, b := baseJob("/work/a"), baseJob("/work/b")
	b.ID = "other"
	removed := b
	removed.Revision, removed.Removed = 2, true
	cloud := baseJob("")
	cloud.ID, cloud.Evidence = "cloud", job.EvidenceNone
	folders := judge.SessionFolders([]job.Job{a, b, removed, cloud})
	assert.Equal(t, []string{"/work/a"}, folders)
	assert.True(t, judge.InFolders("/work/a", folders))
	assert.False(t, judge.InFolders("/work/b", folders))
}

type fixture struct {
	t       *testing.T
	j       judge.Judge
	l       *ledger.Ledger
	now     time.Time
	userDir string
	folder  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home, userDir, folder := t.TempDir(), t.TempDir(), t.TempDir()
	f := &fixture{t: t, userDir: userDir, folder: folder}
	clock := func() time.Time { return f.now }
	f.l = ledger.Open(filepath.Join(home, "ledger.jsonl"), filepath.Join(home, "ledger.lock"), clock, "test",
		func(n int, err error) { t.Errorf("ledger line %d: %v", n, err) })
	f.j = judge.Judge{Ledger: f.l, Now: clock, UserHome: userDir, Dir: home}
	return f
}

func (f *fixture) save(r job.Job, at time.Time) {
	f.now = at
	_, err := f.l.Append(context.Background(), ledger.KindJob, r)
	require.NoError(f.t, err)
}

func (f *fixture) tick(at time.Time) judge.TickResult {
	f.now = at
	r, err := f.j.Tick(context.Background())
	require.NoError(f.t, err)
	return r
}

func (f *fixture) output(day string) {
	at, err := time.Parse(time.RFC3339, day+"T09:05:00-04:00")
	require.NoError(f.t, err)
	f.file("out/"+day+".csv", at)
}

func (f *fixture) file(name string, mtime time.Time) {
	path := filepath.Join(f.folder, name)
	require.NoError(f.t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(f.t, os.WriteFile(path, []byte("row"), 0o600))
	require.NoError(f.t, os.Chtimes(path, mtime, mtime))
}

func (f *fixture) session(s ledger.Session) {
	if s.Cwd == "" {
		s.Cwd = f.folder
	}
	_, err := f.l.Append(context.Background(), ledger.KindSession, s)
	require.NoError(f.t, err)
}

func (f *fixture) transcript(id string, lines ...map[string]any) {
	path := filepath.Join(f.userDir, ".claude", "projects", "-x", id+".jsonl")
	require.NoError(f.t, os.MkdirAll(filepath.Dir(path), 0o700))
	var b strings.Builder
	for _, l := range lines {
		raw, err := json.Marshal(l)
		require.NoError(f.t, err)
		b.Write(append(raw, '\n'))
	}
	require.NoError(f.t, os.WriteFile(path, []byte(b.String()), 0o600))
}

func userLine(id, cwd, at, text string) map[string]any {
	return map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "timestamp": at,
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}
}

func summary(t *testing.T, r judge.TickResult) []string {
	t.Helper()
	out := []string{}
	for _, e := range r.Verdicts {
		var j ledger.Judgment
		require.NoError(t, e.Decode(&j))
		out = append(out, fmt.Sprintf("%s r%d %s %v", j.Slot.In(edt).Format("01-02T15"), j.JobRevision, j.Verdict, j.Sessions))
	}
	return out
}

func TestTick(t *testing.T) {
	t.Parallel()
	start := func(day string) time.Time {
		at, _ := time.Parse(time.RFC3339, day+"T09:01:00-04:00")
		return at
	}
	tcs := []struct {
		name  string
		setup func(f *fixture)
		at    time.Time
		want  []string
	}{
		{
			name: "fills a gap once per slot",
			setup: func(f *fixture) {
				f.save(baseJob(f.folder), time.Date(2026, 10, 5, 12, 0, 0, 0, edt))
				f.session(ledger.Session{SessionID: "s6", StartedAt: start("2026-10-06"), FirstPrompt: "오늘 가격 수집해줘"})
				f.output("2026-10-06")
				f.session(ledger.Session{SessionID: "s7", StartedAt: start("2026-10-07"), FirstPrompt: "오늘 가격 수집해줘"})
			},
			at:   time.Date(2026, 10, 8, 9, 31, 0, 0, edt),
			want: []string{"10-06T09 r1 pass [s6]", "10-07T09 r1 fail [s7]", "10-08T09 r1 missed []"},
		},
		{
			name:  "waits for the grace",
			setup: func(f *fixture) { f.save(baseJob(f.folder), time.Date(2026, 10, 7, 12, 0, 0, 0, edt)) },
			at:    time.Date(2026, 10, 8, 9, 29, 0, 0, edt),
			want:  []string{},
		},
		{
			name: "uses the revision in force",
			setup: func(f *fixture) {
				r1 := baseJob(f.folder)
				f.save(r1, time.Date(2026, 10, 6, 12, 0, 0, 0, edt))
				r2 := r1
				r2.Revision, r2.Schedule = 2, "0 10 * * *"
				f.save(r2, time.Date(2026, 10, 7, 9, 45, 0, 0, edt))
			},
			at:   time.Date(2026, 10, 8, 11, 0, 0, 0, edt),
			want: []string{"10-07T09 r1 missed []", "10-07T10 r2 missed []", "10-08T10 r2 missed []"},
		},
		{
			name: "stops at removal and the 7 day lookback",
			setup: func(f *fixture) {
				r1 := baseJob(f.folder)
				f.save(r1, time.Date(2026, 9, 1, 12, 0, 0, 0, edt))
				r2 := r1
				r2.Revision, r2.Removed = 2, true
				f.save(r2, time.Date(2026, 10, 4, 12, 0, 0, 0, edt))
			},
			at:   time.Date(2026, 10, 8, 12, 0, 0, 0, edt),
			want: []string{"10-02T09 r1 missed []", "10-03T09 r1 missed []", "10-04T09 r1 missed []"},
		},
		{
			name: "backfills a killed session in the job folder only",
			setup: func(f *fixture) {
				f.save(baseJob(f.folder), time.Date(2026, 10, 7, 12, 0, 0, 0, edt))
				f.transcript("abc",
					map[string]any{"type": "user", "isMeta": true, "sessionId": "abc", "cwd": f.folder, "timestamp": "2026-10-08T13:01:00Z",
						"message": map[string]any{"role": "user", "content": "<local-command-caveat>x"}},
					userLine("abc", f.folder, "2026-10-08T13:01:01Z", "오늘 가격 수집해줘\n시트에 넣어"))
				f.transcript("elsewhere", userLine("elsewhere", "/somewhere/else", "2026-10-08T13:01:01Z", "오늘 가격 수집해줘"))
			},
			at:   time.Date(2026, 10, 8, 9, 31, 0, 0, edt),
			want: []string{"10-08T09 r1 fail [abc]"},
		},
		{
			name: "an undated fresh file written after a gap passes only its latest slot",
			setup: func(f *fixture) {
				r := baseJob(f.folder)
				r.Checks = []check.Spec{{Kind: check.KindFile, Path: "out/report.md"}}
				f.save(r, time.Date(2026, 10, 5, 12, 0, 0, 0, edt))
				f.file("out/report.md", time.Date(2026, 10, 8, 9, 5, 0, 0, edt))
			},
			at:   time.Date(2026, 10, 8, 9, 31, 0, 0, edt),
			want: []string{"10-06T09 r1 inconclusive []", "10-07T09 r1 inconclusive []", "10-08T09 r1 pass []"},
		},
		{
			name: "a complete backfill replaces an incomplete hook record",
			setup: func(f *fixture) {
				f.save(baseJob(f.folder), time.Date(2026, 10, 7, 12, 0, 0, 0, edt))
				f.session(ledger.Session{SessionID: "abc", Source: ledger.SourceHook})
				f.transcript("abc", userLine("abc", f.folder, "2026-10-08T13:01:01Z", "오늘 가격 수집해줘"))
				f.output("2026-10-08")
			},
			at:   time.Date(2026, 10, 8, 9, 31, 0, 0, edt),
			want: []string{"10-08T09 r1 pass [abc]"},
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tc.setup(f)
			assert.Equal(t, tc.want, summary(t, f.tick(tc.at)))
			assert.Empty(t, f.tick(tc.at).Verdicts, "a second tick at the same time must add nothing")
		})
	}
}

func TestProbe(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.now = time.Date(2026, 10, 8, 9, 10, 0, 0, edt)
	f.output("2026-10-08")
	e, err := f.j.Probe(context.Background(), baseJob(f.folder), f.now, "fp")
	require.NoError(t, err)
	assert.Equal(t, ledger.KindProbe, e.Kind)
	var j ledger.Judgment
	require.NoError(t, e.Decode(&j))
	assert.Equal(t, ledger.Judgment{
		JobID: "price-watch", JobRevision: 1, SlotKey: "price-watch@2026-10-08T09:00:00-04:00",
		Verdict: ledger.VerdictPass, Trigger: judge.TriggerCheck, Fingerprint: "fp",
	}, ledger.Judgment{JobID: j.JobID, JobRevision: j.JobRevision, SlotKey: j.SlotKey, Verdict: j.Verdict, Trigger: j.Trigger, Fingerprint: j.Fingerprint})
	assert.Empty(t, f.tick(time.Date(2026, 10, 8, 9, 31, 0, 0, edt)).Verdicts, "a probe of an unsaved job must not create slots")
}

func TestInstall(t *testing.T) {
	t.Parallel()
	refuse := errors.New("refused")
	type want struct {
		failed    bool
		installed bool
		last      string
	}
	tcs := []struct {
		name   string
		goos   string
		failOn string
		want   want
	}{
		{name: "macOS", goos: "darwin", want: want{installed: true, last: "launchctl bootstrap gui/501"}},
		{name: "macOS refused leaves no plist", goos: "darwin", failOn: "bootstrap", want: want{failed: true, last: "launchctl bootstrap gui/501"}},
		{name: "linux", goos: "linux", want: want{installed: true, last: "systemctl --user enable --now rindcut-tick.timer"}},
		{name: "linux refused leaves no units", goos: "linux", failOn: "enable", want: want{failed: true, last: "systemctl --user enable --now rindcut-tick.timer"}},
		{name: "unsupported OS", goos: "windows", want: want{failed: true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran []string
			in := judge.Installer{Binary: "/home/ann/.rindcut/bin/rindcut", Home: "/home/ann/.rindcut", UserHome: t.TempDir(), GOOS: tc.goos, UID: 501,
				Exec: func(name string, args ...string) error {
					ran = append(ran, name+" "+strings.Join(args, " "))
					if tc.failOn != "" && len(args) > 1 && (args[0] == tc.failOn || args[1] == tc.failOn) {
						return refuse
					}
					return nil
				}}
			err := in.Install()
			last := ""
			if len(ran) > 0 {
				last = ran[len(ran)-1]
			}
			assert.Equal(t, tc.want, want{failed: err != nil, installed: in.Installed(), last: strings.Split(last, " /")[0]})
		})
	}
}

func TestScheduleFiles(t *testing.T) {
	t.Parallel()
	in := judge.Installer{Binary: "/home/ann/.rindcut/bin/rindcut", Home: "/home/ann/.rindcut", UserHome: t.TempDir(), UID: 501,
		Exec: func(string, ...string) error { return nil }}
	plist := string(in.Plist())
	for _, want := range []string{"<string>/home/ann/.rindcut/bin/rindcut</string><string>tick</string>", "<integer>300</integer>", "<key>RunAtLoad</key><true/>"} {
		assert.Contains(t, plist, want)
	}
	service, timer := in.Units()
	assert.Contains(t, string(service), `Environment="RINDCUT_HOME=/home/ann/.rindcut"`)
	assert.Contains(t, string(timer), "Persistent=true")

	in.GOOS = "linux"
	require.NoError(t, in.Install())
	require.NoError(t, in.Uninstall())
	assert.False(t, in.Installed())
}

func TestScheduled(t *testing.T) {
	t.Parallel()
	r1 := baseJob("/work/a")
	r1.Since = time.Date(2026, 10, 1, 12, 0, 0, 0, edt)
	r2 := r1
	r2.Revision, r2.Schedule, r2.Since = 2, "0 10 * * *", time.Date(2026, 10, 3, 9, 30, 0, 0, edt)
	r3 := r2
	r3.Revision, r3.Removed, r3.Since = 3, true, time.Date(2026, 10, 5, 12, 0, 0, 0, edt)
	other := baseJob("/work/b")
	other.ID, other.Schedule, other.Since = "other", "30 9 * * *", r1.Since
	got := []string{}
	for _, s := range judge.Scheduled([]job.Job{r3, other, r1, r2}, time.Date(2026, 10, 2, 0, 0, 0, 0, edt),
		time.Date(2026, 10, 6, 0, 0, 0, 0, edt)) {
		got = append(got, s.JobID+" "+s.At.In(edt).Format("01-02T15:04"))
	}
	assert.Equal(t, []string{
		"price-watch 10-02T09:00", "other 10-02T09:30", "price-watch 10-03T09:00", "other 10-03T09:30", "price-watch 10-03T10:00",
		"other 10-04T09:30", "price-watch 10-04T10:00", "other 10-05T09:30", "price-watch 10-05T10:00",
	}, got)
}

func TestLate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daily := baseJob(dir)
	hourly := baseJob(dir)
	hourly.Schedule = "0 * * * *"
	never := baseJob(dir)
	never.Schedule = "0 9 31 2 *"
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, edt)
	s := job.Slot{JobID: "price-watch", At: at, End: at.Add(30 * time.Minute)}
	sess := func(id string, start time.Time, prompt string) ledger.Session {
		return ledger.Session{SessionID: id, Cwd: dir, StartedAt: start, FirstPrompt: prompt}
	}
	marker := "오늘 가격 수집해줘"
	type args struct {
		j        job.Job
		sessions []ledger.Session
	}
	type want struct {
		id string
		ok bool
	}
	tcs := []struct {
		name string
		args args
		want want
	}{
		{name: "right after the window", args: args{j: daily, sessions: []ledger.Session{sess("a", s.End.Add(time.Second), marker)}},
			want: want{id: "a", ok: true}},
		{name: "earliest of two", args: args{j: daily, sessions: []ledger.Session{
			sess("b", at.Add(5*time.Hour), marker), sess("a", at.Add(2*time.Hour), marker)}}, want: want{id: "a", ok: true}},
		{name: "inside the window is a match, not late", args: args{j: daily, sessions: []ledger.Session{sess("a", s.End, marker)}}},
		{name: "at the next slot", args: args{j: daily, sessions: []ledger.Session{sess("a", at.AddDate(0, 0, 1), marker)}}},
		{name: "hourly catch-up in the next slot's window", args: args{j: hourly,
			sessions: []ledger.Session{sess("a", at.Add(70*time.Minute), marker)}}},
		{name: "another prompt", args: args{j: daily, sessions: []ledger.Session{sess("a", at.Add(time.Hour), "가격 파일 열어줘")}}},
		{name: "incomplete record", args: args{j: daily, sessions: []ledger.Session{{SessionID: "a", Cwd: dir, FirstPrompt: marker}}}},
		{name: "no next slot", args: args{j: never, sessions: []ledger.Session{sess("a", at.AddDate(0, 3, 0), marker)}},
			want: want{id: "a", ok: true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := judge.Late(tc.args.j, s, tc.args.sessions)
			assert.Equal(t, tc.want, want{id: got.SessionID, ok: ok})
		})
	}
}

func TestPin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.file("check.sh", time.Date(2026, 10, 8, 9, 0, 0, 0, edt))
	r := baseJob(f.folder)
	r.Checks = []check.Spec{{Kind: check.KindFile, Path: "out/{{date}}.csv"}, {Kind: check.KindCommand, Run: "sh check.sh"}}
	pinned := f.j.Pin(r)
	assert.Nil(t, pinned.Checks[0].Scripts)
	assert.Len(t, pinned.Checks[1].Scripts, 1)
	assert.Nil(t, r.Checks[1].Scripts, "the caller's job stays as it was")
	assert.Contains(t, pinned.Checks[1].Scripts, filepath.Join(f.folder, "check.sh"))
}
