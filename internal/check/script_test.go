package check_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/check"
)

func TestPinned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"check.sh", "rows.py", "count.sh", "space name.sh", `quote"name.sh`, `back\slash.sh`} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("exit 0\n"), 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tool"), []byte{0x7f, 'E', 'L', 'F', 0, 1}, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "locked.sh"), []byte("exit 0\n"), 0o000))
	// Root still able to read files without permission bits
	unreadable := []string{}
	if os.Geteuid() == 0 {
		unreadable = []string{"locked.sh"}
	}
	tcs := []struct {
		name string
		spec check.Spec
		want []string
	}{
		{name: "script in command position", spec: check.Spec{Kind: check.KindCommand, Run: "./check.sh --today"},
			want: []string{"check.sh"}},
		{name: "interpreter argument", spec: check.Spec{Kind: check.KindCommand, Run: "python3 -u rows.py out.csv"},
			want: []string{"rows.py"}},
		{name: "interpreter by path", spec: check.Spec{Kind: check.KindCommand, Run: "/usr/bin/env -i; /bin/sh ./count.sh"},
			want: []string{"count.sh"}},
		{name: "after an assignment and a pipe",
			spec: check.Spec{Kind: check.KindCommand, Run: "LANG=C ./check.sh | bash 'count.sh'"},
			want: []string{"check.sh", "count.sh"}},
		{name: "newlines separate commands", spec: check.Spec{Kind: check.KindCommand, Run: "./check.sh\n./count.sh"},
			want: []string{"check.sh", "count.sh"}},
		{name: "escaped spaces", spec: check.Spec{Kind: check.KindCommand, Run: `./space\ name.sh`},
			want: []string{"space name.sh"}},
		{name: "escaped quote", spec: check.Spec{Kind: check.KindCommand, Run: `sh "quote\"name.sh"`},
			want: []string{`quote"name.sh`}},
		{name: "literal backslash in quotes", spec: check.Spec{Kind: check.KindCommand, Run: `sh "back\slash.sh"`},
			want: []string{`back\slash.sh`}},
		{name: "continued command", spec: check.Spec{Kind: check.KindCommand, Run: "sh \\\ncheck.sh"},
			want: []string{"check.sh"}},
		{name: "home path", spec: check.Spec{Kind: check.KindCommand, Run: "~/check.sh"}, want: []string{"check.sh"}},
		{name: "plain words are not scripts",
			spec: check.Spec{Kind: check.KindCommand, Run: "test -s rows.py && grep -q x rows.py"},
			want: []string{}},
		{name: "templated, missing, a folder and a program", spec: check.Spec{Kind: check.KindCommand,
			Run: "./{{date}}.sh; ./gone.sh; ./sub; ./tool; sh -c 'exit 0'; python3 {{date}}.py rows.py"}, want: []string{}},
		{name: "unreadable script", spec: check.Spec{Kind: check.KindCommand, Run: "./locked.sh"}, want: unreadable},
		{name: "other kinds stay as they are", spec: check.Spec{Kind: check.KindFile, Path: "./check.sh"}, want: []string{}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := []string{}
			for path, sum := range tc.spec.Pinned(envIn(dir)).Scripts {
				assert.Len(t, sum, 64)
				got = append(got, filepath.Base(path))
			}
			sort.Strings(got)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPinnedScriptChange(t *testing.T) {
	t.Parallel()
	type args struct {
		contents []byte
		removed  bool
	}
	type want struct {
		status   check.Status
		scripts  bool
		executed bool
	}
	tcs := []struct {
		name string
		args args
		want want
	}{
		{name: "unchanged script runs", args: args{contents: []byte("exit 0\n")},
			want: want{status: check.StatusPass, executed: true}},
		{name: "edited script", args: args{contents: []byte("exit 1\n")},
			want: want{status: check.StatusError, scripts: true}},
		{name: "removed script", args: args{removed: true},
			want: want{status: check.StatusError, scripts: true}},
		{name: "no longer a text file", args: args{contents: []byte{'x', 0, 'y'}},
			want: want{status: check.StatusError, scripts: true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "check.sh")
			require.NoError(t, os.WriteFile(path, []byte("exit 0\n"), 0o700))
			spec := check.Spec{Kind: check.KindCommand, Run: "sh check.sh; touch ran"}.Pinned(envIn(dir))
			require.NoError(t, os.WriteFile(path, tc.args.contents, 0o700))
			if tc.args.removed {
				require.NoError(t, os.Remove(path))
			}
			r := check.Run(context.Background(), spec, envIn(dir))
			_, changed := r.Observed["scripts"]
			_, err := os.Stat(filepath.Join(dir, "ran"))
			assert.Equal(t, tc.want, want{status: r.Status, scripts: changed, executed: err == nil}, r.Message)
		})
	}
}

func TestSlotFree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dated := filepath.Join(dir, "dated.sh")
	plain := filepath.Join(dir, "plain.sh")
	require.NoError(t, os.WriteFile(dated, []byte("test -s \"out/$RINDCUT_DATE.csv\"\n"), 0o700))
	require.NoError(t, os.WriteFile(plain, []byte("test -s out/latest.csv\n"), 0o700))
	no := false
	pin := func(s check.Spec) check.Spec { return s.Pinned(envIn(dir)) }
	tcs := []struct {
		name string
		spec check.Spec
		want bool
	}{
		{name: "fresh file", spec: check.Spec{Kind: check.KindFile, Path: "out/latest.csv"}},
		{name: "dated file", spec: check.Spec{Kind: check.KindFile, Path: "out/{{ date }}.csv", Fresh: &no}},
		{name: "stale file allowed", spec: check.Spec{Kind: check.KindFile, Path: "out/latest.csv", Fresh: &no}, want: true},
		{name: "job id alone", spec: check.Spec{Kind: check.KindFile, Path: "out/{{job}}.csv", Fresh: &no}, want: true},
		{name: "page with a date",
			spec: check.Spec{Kind: check.KindHTTP, URL: "https://example.com/r", Contains: "{{yesterday}}"}},
		{name: "page without one", spec: check.Spec{Kind: check.KindHTTP, URL: "https://example.com/r", Contains: "ok"},
			want: true},
		{name: "command with a slot variable", spec: check.Spec{Kind: check.KindCommand, Run: "grep -q {{slot}} log"}},
		{name: "command reading the date", spec: check.Spec{Kind: check.KindCommand, Run: `test -s "out/$RINDCUT_DATE.csv"`}},
		{name: "script reading the date", spec: pin(check.Spec{Kind: check.KindCommand, Run: "./dated.sh"})},
		{name: "script without it", spec: pin(check.Spec{Kind: check.KindCommand, Run: "./plain.sh"}), want: true},
		{name: "unreadable script", spec: check.Spec{Kind: check.KindCommand, Run: "./plain.sh",
			Scripts: map[string]string{filepath.Join(dir, "gone.sh"): "x"}}},
		{name: "script read fails", spec: check.Spec{Kind: check.KindCommand, Run: "./plain.sh",
			Scripts: map[string]string{dir: "x"}}},
		{name: "unknown kind", spec: check.Spec{Kind: "sheet"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.spec.SlotFree())
		})
	}
}

func TestSlotFreeScriptReads(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		text string
		want bool
	}{
		{name: "plain large script", text: strings.Repeat("x", 100000), want: true},
		{name: "slot name across reads", text: strings.Repeat("x", 4101) + "RINDCUT_SLOT"},
		{name: "date at the end", text: strings.Repeat("x", 100000) + "RINDCUT_DATE"},
		{name: "empty script", want: true},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "check.sh")
			require.NoError(t, os.WriteFile(path, []byte(tc.text), 0o600))
			s := check.Spec{Kind: check.KindCommand, Run: "sh check.sh", Scripts: map[string]string{path: "pinned"}}
			assert.Equal(t, tc.want, s.SlotFree())
		})
	}
}
