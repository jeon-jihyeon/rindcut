package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
	"github.com/jeon-jihyeon/rindcut/internal/lock"
)

var t0 = time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)

// skips collects the lines a ledger couldn't read
type skips struct {
	mu    sync.Mutex
	lines []int
}

func (s *skips) add(n int, _ error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, n)
}

func open(t *testing.T) (*ledger.Ledger, string, *skips) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")
	sk := &skips{}
	return ledger.Open(path, filepath.Join(dir, "ledger.lock"), func() time.Time { return t0 }, "test", sk.add), path, sk
}

func TestAppendScan(t *testing.T) {
	t.Parallel()
	l, path, _ := open(t)
	ctx := context.Background()
	e, err := l.Append(ctx, ledger.KindSession, ledger.Session{SessionID: "s1", Source: ledger.SourceHook})
	require.NoError(t, err)
	assert.Equal(t, "test", e.Version)
	assert.True(t, e.At.Equal(t0))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	all, err := l.All(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	var s ledger.Session
	require.NoError(t, all[0].Decode(&s))
	assert.Equal(t, "s1", s.SessionID)
}

func TestMissingLedgerIsEmpty(t *testing.T) {
	t.Parallel()
	l, _, _ := open(t)
	all, err := l.All(context.Background())
	require.NoError(t, err)
	assert.Empty(t, all)
}

func TestTooLong(t *testing.T) {
	t.Parallel()
	l, _, _ := open(t)
	_, err := l.Append(context.Background(), ledger.KindProbe, strings.Repeat("x", 64<<10))
	assert.ErrorIs(t, err, ledger.ErrLineTooLong)
}

func TestDamagedLines(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name    string
		damage  string
		skipped []int
	}{
		{name: "torn line from a crash", damage: `{"id":"torn","kind":"sess`, skipped: []int{2}},
		{name: "overlong line", damage: strings.Repeat("x", 200<<10) + "\n", skipped: []int{2}},
		{name: "entry without id", damage: `{"kind":"session"}` + "\n", skipped: []int{2}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, path, sk := open(t)
			ctx := context.Background()
			first, err := l.Append(ctx, ledger.KindSession, ledger.Session{SessionID: "a"})
			require.NoError(t, err)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = f.WriteString(tc.damage)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			last, err := l.Append(ctx, ledger.KindSession, ledger.Session{SessionID: "b"})
			require.NoError(t, err)

			all, err := l.All(ctx)
			require.NoError(t, err)
			assert.Equal(t, []string{first.ID, last.ID}, []string{all[0].ID, all[len(all)-1].ID})
			assert.Len(t, all, 2)
			assert.Equal(t, tc.skipped, sk.lines)
		})
	}
}

func appendRetry(l *ledger.Ledger) error {
	for {
		_, err := l.Append(context.Background(), ledger.KindSession, ledger.Session{SessionID: strings.Repeat("s", 2000)})
		if !errors.Is(err, lock.ErrBusy) {
			return err
		}
	}
}

func TestConcurrentGoroutines(t *testing.T) {
	t.Parallel()
	l, _, sk := open(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				assert.NoError(t, appendRetry(l))
			}
		})
	}
	wg.Wait()
	all, err := l.All(context.Background())
	require.NoError(t, err)
	assert.Len(t, all, 400)
	assert.Empty(t, sk.lines)
}

// Two processes append through the same lock, as the hook and the tick do
func TestConcurrentProcesses(t *testing.T) {
	if os.Getenv("RINDCUT_LEDGER_CHILD") != "" {
		l := ledger.Open(os.Getenv("RINDCUT_LEDGER_PATH"), os.Getenv("RINDCUT_LEDGER_LOCK"), time.Now, "child", func(int, error) {})
		for range 500 {
			if err := appendRetry(l); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
	t.Parallel()
	l, path, sk := open(t)
	var cmds []*exec.Cmd
	for range 2 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestConcurrentProcesses$")
		cmd.Env = append(os.Environ(), "RINDCUT_LEDGER_CHILD=1",
			"RINDCUT_LEDGER_PATH="+path, "RINDCUT_LEDGER_LOCK="+filepath.Join(filepath.Dir(path), "ledger.lock"))
		require.NoError(t, cmd.Start())
		cmds = append(cmds, cmd)
	}
	for _, c := range cmds {
		require.NoError(t, c.Wait())
	}
	all, err := l.All(context.Background())
	require.NoError(t, err)
	assert.Len(t, all, 1000)
	assert.Empty(t, sk.lines)
}

func TestSummary(t *testing.T) {
	t.Parallel()
	l, _, _ := open(t)
	ctx := context.Background()
	judge := func(job string, slot time.Time, v ledger.Verdict, msg string) ledger.Entry {
		e, err := l.Append(ctx, ledger.KindVerdict, ledger.Judgment{
			JobID: job, SlotKey: job + "@" + slot.Format(time.RFC3339), Slot: slot, Verdict: v,
			Checks: []ledger.CheckRecord{
				{Kind: "file", Status: ledger.CheckStatusPass, Message: "ok"},
				{Kind: "file", Status: string(v), Message: msg},
			},
		})
		require.NoError(t, err)
		return e
	}
	judge("price", t0.Add(-48*time.Hour), ledger.VerdictFail, "too old")
	judge("price", t0.Add(-2*time.Hour), ledger.VerdictPass, "")
	wrong := judge("price", t0.Add(-1*time.Hour), ledger.VerdictFail, "no file")
	judge("report", t0.Add(-3*time.Hour), ledger.VerdictMissed, "no session")
	_, err := l.Append(ctx, ledger.KindProbe, ledger.Judgment{JobID: "price", Slot: t0.Add(-time.Hour), Verdict: ledger.VerdictFail})
	require.NoError(t, err)
	_, err = l.Append(ctx, ledger.KindCorrection, ledger.Correction{VerdictID: wrong.ID, Verdict: ledger.VerdictInconclusive, Reason: "disk was unmounted"})
	require.NoError(t, err)
	_, err = l.Append(ctx, ledger.KindCorrection, ledger.Correction{VerdictID: wrong.ID, Verdict: ledger.VerdictPass, Reason: "file was renamed"})
	require.NoError(t, err)

	all, err := l.All(ctx)
	require.NoError(t, err)
	r := ledger.Summary(all, t0.Add(-24*time.Hour), t0)
	assert.Equal(t, ledger.Counts{Pass: 2, Missed: 1}, r.Totals)
	assert.Equal(t, map[string]ledger.Counts{"price": {Pass: 2}, "report": {Missed: 1}}, r.Jobs)
	require.Len(t, r.Problems, 1)
	assert.Equal(t, "report", r.Problems[0].JobID)
	assert.Equal(t, "no session", r.Problems[0].Reason)
	assert.Equal(t, ledger.Correction{VerdictID: wrong.ID, Verdict: ledger.VerdictPass, Reason: "file was renamed"}, ledger.Corrections(all)[wrong.ID])
}

func TestSessionComplete(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		s    ledger.Session
		want bool
	}{
		{name: "prompt and start", s: ledger.Session{FirstPrompt: "x", StartedAt: t0}, want: true},
		{name: "no prompt", s: ledger.Session{StartedAt: t0}, want: false},
		{name: "no start", s: ledger.Session{FirstPrompt: "x"}, want: false},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.s.Complete())
		})
	}
}
