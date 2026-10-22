package judge_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

func TestTickRunReference(t *testing.T) {
	t.Parallel()
	type args struct {
		first, second                                               string
		incomplete, differentPrompt, differentTime, differentFolder bool
	}
	tcs := []struct {
		name string
		args args
		want string
	}{
		{name: "first explicit reference stays", args: args{first: "r1", second: "r2"}, want: "r1"},
		{name: "identical record fills missing reference", args: args{second: "r2"}, want: "r2"},
		{name: "completion preserves earlier reference", args: args{first: "r1", second: "r2", incomplete: true},
			want: "r1"},
		{name: "completion brings new reference", args: args{second: "r2", incomplete: true}, want: "r2"},
		{name: "completion has no reference", args: args{first: "r1", incomplete: true}, want: "r1"},
		{name: "another prompt cannot fill", args: args{second: "r2", differentPrompt: true}},
		{name: "another start cannot fill", args: args{second: "r2", differentTime: true}},
		{name: "another folder cannot fill", args: args{second: "r2", differentFolder: true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			r := baseJob(f.folder)
			f.save(r, time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
			s := ledger.Session{SessionID: "session-one", RunID: tc.args.first, Cwd: f.folder,
				StartedAt: time.Date(2026, 10, 8, 9, 1, 0, 0, time.UTC), FirstPrompt: r.Marker}
			first := s
			if tc.args.incomplete {
				first.FirstPrompt = ""
			}
			f.session(first)
			s.RunID = tc.args.second
			if tc.args.differentPrompt {
				s.FirstPrompt = "another prompt"
			}
			if tc.args.differentTime {
				s.StartedAt = s.StartedAt.Add(time.Minute)
			}
			if tc.args.differentFolder {
				s.Cwd = t.TempDir()
			}
			f.session(s)
			out := f.tick(time.Date(2026, 10, 8, 9, 31, 0, 0, time.UTC))
			require.Len(t, out.Verdicts, 1)
			p, err := f.j.Probe(context.Background(), r, f.now, "fingerprint")
			require.NoError(t, err)
			for _, e := range []ledger.Entry{out.Verdicts[0], p} {
				var j ledger.Judgment
				require.NoError(t, e.Decode(&j))
				assert.Equal(t, tc.want, j.RunID)
			}
		})
	}
}
