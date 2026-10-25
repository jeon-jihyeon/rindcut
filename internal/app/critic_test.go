package app_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/app"
	"github.com/jeon-jihyeon/rindcut/internal/check"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
	"github.com/jeon-jihyeon/rindcut/internal/lock"
)

const appCriticResponse = `{"protocol":"rindcut.critic/v1","job_id":"report","job_revision":1,` +
	`"slot":"2026-10-08T09:00:00Z",` +
	`"criteria_revision":"C1","complete":true,"criteria":[{"id":"accuracy","verdict":"rejected"}]}`

func withCritic(t *testing.T, w world) job.Job {
	t.Helper()
	j := w.job()
	j.Name, j.Evidence, j.Marker = "Report", job.EvidenceNone, ""
	j.Checks = []check.Spec{{Kind: check.KindCommand, Run: "true"}}
	j.Critic = &job.Critic{Source: "acme", SourceRevision: "adapter-v1", CriteriaRevision: "C1",
		Criteria: []job.Criterion{{ID: "accuracy", Text: "Numbers have sources"}},
		Command:  check.Spec{Kind: check.KindCommand, Run: "sh critic.sh"}}
	require.NoError(t, os.WriteFile(filepath.Join(w.folder, "response.json"), []byte(appCriticResponse), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(w.folder, "critic.sh"),
		[]byte("cat > input.json\nprintf x >> calls\ncat response.json\n"), 0o700))
	return j
}

func TestCriticChangesRequirePreview(t *testing.T) {
	t.Parallel()
	type args struct {
		edit    func(*job.Critic)
		script  bool
		preview bool
	}
	tcs := []struct {
		name string
		args args
		want error
	}{
		{name: "same definition"},
		{name: "source", args: args{edit: func(c *job.Critic) { c.Source = "other" }}, want: app.ErrNoPreview},
		{name: "source revision", args: args{edit: func(c *job.Critic) { c.SourceRevision = "adapter-v2" }},
			want: app.ErrNoPreview},
		{name: "criteria revision", args: args{edit: func(c *job.Critic) { c.CriteriaRevision = "C2" }},
			want: app.ErrNoPreview},
		{name: "criterion text", args: args{edit: func(c *job.Critic) { c.Criteria[0].Text = "Sources are current" }},
			want: app.ErrNoPreview},
		{name: "reason retention", args: args{edit: func(c *job.Critic) { c.RetainReason = true }}, want: app.ErrNoPreview},
		{name: "edited adapter", args: args{script: true}, want: app.ErrNoPreview},
		{name: "edited adapter previewed again", args: args{script: true, preview: true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			j := withCritic(t, w)
			ctx := context.Background()
			_, err := w.a.Preview(ctx, j)
			require.NoError(t, err)
			if tc.args.edit != nil {
				tc.args.edit(j.Critic)
			}
			if tc.args.script {
				require.NoError(t, os.WriteFile(filepath.Join(w.folder, "critic.sh"), []byte("exit 75\n"), 0o700))
			}
			if tc.args.preview {
				_, err = w.a.Preview(ctx, j)
				require.NoError(t, err)
			}
			_, err = w.a.Save(ctx, j)
			assert.ErrorIs(t, err, tc.want)
		})
	}
}

func TestCriticRevisionAndReport(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	*w.now = time.Date(2026, 10, 8, 8, 50, 0, 0, time.UTC)
	j := w.saved(t, withCritic(t, w))
	*w.now = time.Date(2026, 10, 8, 9, 20, 0, 0, time.UTC)
	j.Critic.CriteriaRevision, j.Critic.SourceRevision = "C2", "adapter-v2"
	j = w.saved(t, j)
	*w.now = time.Date(2026, 10, 8, 9, 31, 0, 0, time.UTC)
	out, err := w.a.Tick(ctx)
	require.NoError(t, err)
	require.Len(t, out.Verdicts, 1)
	var original ledger.Judgment
	require.NoError(t, out.Verdicts[0].Decode(&original))
	require.NotNil(t, original.Critic)
	assert.Equal(t, 1, original.JobRevision)
	assert.Equal(t, "C1", original.Critic.CriteriaRevision)
	assert.Equal(t, "adapter-v1", original.Critic.SourceRevision)
	report, err := w.a.Report(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Totals.Pass)
	assert.Empty(t, report.Problems)
	require.Len(t, report.CriticProblems, 1)
	assert.Equal(t, ledger.CriticRejected, report.CriticProblems[0].Critic.Verdict)
	_, err = w.a.Correct(ctx, ledger.Correction{VerdictID: out.Verdicts[0].ID,
		Verdict: ledger.VerdictFail, Reason: "Owner correction", By: "ann"})
	require.NoError(t, err)
	report, err = w.a.Report(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Totals.Fail)
	require.Len(t, report.CriticProblems, 1)
	assert.Equal(t, ledger.CriticRejected, report.CriticProblems[0].Critic.Verdict)
	probe, err := w.a.Judge().Probe(ctx, j, *w.now, app.Fingerprint(j))
	require.NoError(t, err)
	var current ledger.Judgment
	require.NoError(t, probe.Decode(&current))
	require.NotNil(t, current.Critic)
	assert.Equal(t, 2, current.JobRevision)
	assert.Equal(t, "C2", current.Critic.CriteriaRevision)
	assert.Equal(t, "criteria_revision_mismatch", current.Critic.Cause)
}

func TestCriticTickConcurrency(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	j := withCritic(t, w)
	j.ID, j.Revision = "report", 1
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	j.Checks = []check.Spec{{Kind: check.KindHTTP, URL: srv.URL}}
	*w.now = time.Date(2026, 10, 8, 8, 50, 0, 0, time.UTC)
	_, err := w.a.Ledger().Append(context.Background(), ledger.KindJob, j)
	require.NoError(t, err)
	*w.now = time.Date(2026, 10, 8, 9, 31, 0, 0, time.UTC)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		tick app.TickResult
		err  error
	}
	done := make(chan result, 1)
	go func() {
		tick, err := w.a.Tick(ctx)
		done <- result{tick: tick, err: err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first tick did not reach the HTTP check")
	}
	busy, err := w.a.Tick(ctx)
	require.NoError(t, err)
	assert.True(t, busy.Busy)
	close(release)
	finished := <-done
	require.NoError(t, finished.err)
	require.Len(t, finished.tick.Verdicts, 1)
	calls, err := os.ReadFile(filepath.Join(w.folder, "calls"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(calls))
}

func TestCriticConcurrentRevisions(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name  string
		count int
		want  []int
	}{
		{name: "two saves", count: 2, want: []int{1, 2}},
		{name: "four saves", count: 4, want: []int{1, 2, 3, 4}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			j := withCritic(t, w)
			ctx := context.Background()
			_, err := w.a.Preview(ctx, j)
			require.NoError(t, err)
			type result struct {
				saved app.Saved
				err   error
			}
			start, done := make(chan struct{}), make(chan result, tc.count)
			for range tc.count {
				go func() {
					<-start
					saved, err := w.a.Save(ctx, j)
					done <- result{saved: saved, err: err}
				}()
			}
			close(start)
			revisions := make([]int, 0, tc.count)
			for range tc.count {
				finished := <-done
				require.NoError(t, finished.err)
				revisions = append(revisions, finished.saved.Job.Revision)
			}
			sort.Ints(revisions)
			assert.Equal(t, tc.want, revisions)
		})
	}
}

func TestCriticSaveDefinitionLockFailure(t *testing.T) {
	t.Parallel()
	tcs := []struct{ name string }{{name: "save"}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			j := w.saved(t, withCritic(t, w))
			unlock, err := lock.Acquire(context.Background(), w.a.Home.Lock("jobs"), 0)
			require.NoError(t, err)
			defer unlock()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = w.a.Save(ctx, j)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "job definition busy or unavailable")
		})
	}
}

func TestCriticRemoveDefinitionLockFailure(t *testing.T) {
	t.Parallel()
	tcs := []struct{ name string }{{name: "remove"}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			j := w.saved(t, withCritic(t, w))
			unlock, err := lock.Acquire(context.Background(), w.a.Home.Lock("jobs"), 0)
			require.NoError(t, err)
			defer unlock()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = w.a.Remove(ctx, j.ID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "job definition busy or unavailable")
		})
	}
}
