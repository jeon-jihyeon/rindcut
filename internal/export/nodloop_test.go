package export_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/export"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

// fakeNodloop writes a script that logs its arguments, prints two runs for trace list and exits with outcomeExit on feedback
func fakeNodloop(t *testing.T, outcomeExit string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	bin = filepath.Join(dir, "nodloop")
	script := `#!/bin/sh
echo "$@" >> ` + log + `
if [ "$1" = trace ]; then
  printf 'r1\t2026-10-08T00:01:00Z\trun\t\t$0.0100\n'
  printf 'r2\t2026-10-08T00:05:00Z\trun\t\t$0.0100\n'
  printf 'x1\t2026-10-08T00:06:00Z\tcompaction\t\t$0.0000\n'
fi
if [ "$1" = feedback ]; then exit ` + outcomeExit + `; fi
`
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700))
	return bin, log
}

func TestNodloop(t *testing.T) {
	t.Parallel()
	es := []ledger.Entry{
		verdict(t, "v1", ledger.VerdictFail, "sess-1"),
		verdict(t, "v2", ledger.VerdictMissed),
		entry(t, "c1", ledger.KindCorrection, ledger.Correction{VerdictID: "v1", Verdict: ledger.VerdictPass, Reason: "renamed"}),
	}
	type want struct {
		calls     []string
		permanent bool
		failed    bool
	}
	tcs := []struct {
		name string
		exit string
		want want
	}{
		{name: "outcomes on the newest run", exit: "0", want: want{calls: []string{
			"trace list --session sess-1",
			"feedback outcome --trace r2 --result refuted --reviewer rindcut --note rindcut price-watch@2026-10-08T09:00:00+09:00",
			"trace list --session sess-1",
			"feedback outcome --trace r2 --result confirmed --reviewer rindcut --note rindcut correction of price-watch@2026-10-08T09:00:00+09:00: renamed",
		}}},
		{name: "a refused outcome is permanent", exit: "3", want: want{calls: []string{
			"trace list --session sess-1",
			"feedback outcome --trace r2 --result refuted --reviewer rindcut --note rindcut price-watch@2026-10-08T09:00:00+09:00",
		}, permanent: true, failed: true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin, log := fakeNodloop(t, tc.exit)
			err := export.NewNodloop(bin, es).Export(context.Background(), es)
			calls, readErr := os.ReadFile(log)
			require.NoError(t, readErr)
			var permanent export.PermanentError
			assert.Equal(t, tc.want, want{
				calls:     strings.Split(strings.TrimSpace(string(calls)), "\n"),
				permanent: errors.As(err, &permanent),
				failed:    err != nil,
			})
		})
	}
}

func TestNodloopMissingBinary(t *testing.T) {
	t.Parallel()
	es := []ledger.Entry{verdict(t, "v1", ledger.VerdictFail, "sess-1")}
	n := export.NewNodloop(filepath.Join(t.TempDir(), "gone"), es)
	err := n.Export(context.Background(), es)
	require.Error(t, err)
	var permanent export.PermanentError
	assert.False(t, errors.As(err, &permanent), "a binary that can't start is retried, it may come back")
	assert.Error(t, n.Test(context.Background()))
}

func TestFindNodloop(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	bin := filepath.Join(home, ".nodloop", "bin", "nodloop")
	require.NoError(t, os.MkdirAll(filepath.Dir(bin), 0o700))
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700))
	onPath := t.TempDir()
	other := filepath.Join(onPath, "nodloop")
	require.NoError(t, os.WriteFile(other, []byte("#!/bin/sh\n"), 0o700))
	tcs := []struct {
		name, home, path, want string
	}{
		{name: "plugin link wins", home: home, path: onPath, want: bin},
		{name: "PATH when no plugin", home: t.TempDir(), path: "/nowhere" + string(os.PathListSeparator) + onPath, want: other},
		{name: "none", home: t.TempDir(), path: "", want: ""},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, export.FindNodloop(tc.home, tc.path))
		})
	}
}
