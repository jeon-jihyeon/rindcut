package check_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/check"
)

var slot = time.Date(2026, 10, 8, 9, 0, 0, 0, time.FixedZone("EDT", -4*3600))

func envIn(dir string) check.Env {
	return check.Env{Slot: slot, JobID: "price-watch", Folder: dir, UserHome: dir, Dir: dir}
}

func write(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		spec check.Spec
		bad  string
	}{
		{name: "file ok", spec: check.Spec{Kind: check.KindFile, Path: "out/{{date}}.csv"}},
		{name: "file without path", spec: check.Spec{Kind: check.KindFile}, bad: "path is required"},
		{name: "negative size", spec: check.Spec{Kind: check.KindFile, Path: "a", MinBytes: -1}, bad: "negative"},
		{name: "unknown variable", spec: check.Spec{Kind: check.KindFile, Path: "{{today}}.csv"}, bad: "unknown variable {{today}}"},
		{name: "http ok", spec: check.Spec{Kind: check.KindHTTP, URL: "https://example.com/{{date}}"}},
		{name: "http not a url", spec: check.Spec{Kind: check.KindHTTP, URL: "example.com"}, bad: "must be an http"},
		{name: "http bad status", spec: check.Spec{Kind: check.KindHTTP, URL: "https://a.b", Status: 99}, bad: "not an HTTP status"},
		{name: "command ok", spec: check.Spec{Kind: check.KindCommand, Run: "test -s x"}},
		{name: "command empty", spec: check.Spec{Kind: check.KindCommand, Run: "  "}, bad: "run is required"},
		{name: "timeout too long", spec: check.Spec{Kind: check.KindCommand, Run: "true", Timeout: check.Duration(time.Hour)}, bad: "timeout"},
		{name: "unknown kind", spec: check.Spec{Kind: "sheet"}, bad: "unknown check kind"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.spec.Validate()
			if tc.bad == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.bad)
		})
	}
}

type want struct {
	status check.Status
	cut    bool
}

func TestFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "2026-10-08.csv"), "row", slot.Add(time.Minute))
	write(t, filepath.Join(dir, "stale.csv"), "row", slot.Add(-time.Hour))
	write(t, filepath.Join(dir, "empty.csv"), "", slot.Add(time.Minute))
	no := false
	tcs := []struct {
		name string
		spec check.Spec
		want check.Status
	}{
		{name: "fresh file by template", spec: check.Spec{Kind: check.KindFile, Path: "{{date}}.csv"}, want: check.StatusPass},
		{name: "tilde path", spec: check.Spec{Kind: check.KindFile, Path: "~/2026-10-08.csv"}, want: check.StatusPass},
		{name: "missing", spec: check.Spec{Kind: check.KindFile, Path: "{{yesterday}}.csv"}, want: check.StatusFail},
		{name: "stale", spec: check.Spec{Kind: check.KindFile, Path: "stale.csv"}, want: check.StatusFail},
		{name: "stale allowed", spec: check.Spec{Kind: check.KindFile, Path: "stale.csv", Fresh: &no}, want: check.StatusPass},
		{name: "empty", spec: check.Spec{Kind: check.KindFile, Path: "empty.csv"}, want: check.StatusFail},
		{name: "folder", spec: check.Spec{Kind: check.KindFile, Path: "."}, want: check.StatusFail},
		{name: "too small", spec: check.Spec{Kind: check.KindFile, Path: "{{date}}.csv", MinBytes: 10}, want: check.StatusFail},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := check.Run(context.Background(), tc.spec, envIn(dir))
			assert.Equal(t, tc.want, r.Status, r.Message)
		})
	}
}

func TestFileUnreadable(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	require.NoError(t, os.Mkdir(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	r := check.Run(context.Background(), check.Spec{Kind: check.KindFile, Path: "locked/a.csv"}, envIn(dir))
	assert.Equal(t, check.StatusError, r.Status, r.Message)
}

func TestHTTP(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			time.Sleep(300 * time.Millisecond)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("가", 2000)))
			return
		}
		_, _ = w.Write([]byte("report for " + strings.TrimPrefix(r.URL.Path, "/")))
	}))
	t.Cleanup(srv.Close)
	tcs := []struct {
		name string
		spec check.Spec
		want want
	}{
		{name: "ok with text", spec: check.Spec{Kind: check.KindHTTP, URL: srv.URL + "/{{date}}", Contains: "report for {{date}}"}, want: want{status: check.StatusPass}},
		{name: "missing text", spec: check.Spec{Kind: check.KindHTTP, URL: srv.URL + "/x", Contains: "nothing"}, want: want{status: check.StatusFail}},
		{name: "wrong status", spec: check.Spec{Kind: check.KindHTTP, URL: srv.URL + "/missing"}, want: want{status: check.StatusFail}},
		{name: "expected status", spec: check.Spec{Kind: check.KindHTTP, URL: srv.URL + "/missing", Status: 404}, want: want{status: check.StatusPass}},
		{name: "timeout", spec: check.Spec{Kind: check.KindHTTP, URL: srv.URL + "/slow", Timeout: check.Duration(50 * time.Millisecond)}, want: want{status: check.StatusError}},
		{name: "unreachable", spec: check.Spec{Kind: check.KindHTTP, URL: "http://127.0.0.1:1/"}, want: want{status: check.StatusError}},
		{name: "big body is cut", spec: check.Spec{Kind: check.KindHTTP, URL: srv.URL + "/big"}, want: want{status: check.StatusPass, cut: true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := check.Run(context.Background(), tc.spec, envIn(t.TempDir()))
			assert.Equal(t, tc.want, want{status: r.Status, cut: r.Truncated}, r.Message)
		})
	}
}

func TestCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "rows.txt"), "a\nb\n", slot)
	tcs := []struct {
		name string
		spec check.Spec
		want want
	}{
		{name: "exit 0 in the job folder", spec: check.Spec{Kind: check.KindCommand, Run: "test -s rows.txt"}, want: want{status: check.StatusPass}},
		{name: "exit 1", spec: check.Spec{Kind: check.KindCommand, Run: "grep -q missing rows.txt"}, want: want{status: check.StatusFail}},
		{name: "template and env", spec: check.Spec{Kind: check.KindCommand, Run: `test "{{date}}" = "$RINDCUT_DATE"`}, want: want{status: check.StatusPass}},
		{name: "try later", spec: check.Spec{Kind: check.KindCommand, Run: "exit 75"}, want: want{status: check.StatusError}},
		{name: "not executable", spec: check.Spec{Kind: check.KindCommand, Run: "exit 126"},
			want: want{status: check.StatusError}},
		{name: "not found", spec: check.Spec{Kind: check.KindCommand, Run: "no-such-command-rindcut"}, want: want{status: check.StatusError}},
		{name: "timeout kills children", spec: check.Spec{Kind: check.KindCommand, Run: "sleep 30 & sleep 30", Timeout: check.Duration(100 * time.Millisecond)}, want: want{status: check.StatusError}},
		{name: "long output is cut", spec: check.Spec{Kind: check.KindCommand, Run: "yes 가 | head -c 20000"}, want: want{status: check.StatusPass, cut: true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			r := check.Run(context.Background(), tc.spec, envIn(dir))
			assert.Equal(t, tc.want, want{status: r.Status, cut: r.Truncated}, r.Message)
			assert.Less(t, time.Since(start), 5*time.Second, "a check must stop at its timeout")
			_, err := json.Marshal(r)
			assert.NoError(t, err, "a result must stay JSON")
		})
	}
}

func TestDurationJSON(t *testing.T) {
	t.Parallel()
	var s check.Spec
	require.NoError(t, json.Unmarshal([]byte(`{"kind":"command","run":"true","timeout":"90s"}`), &s))
	assert.Equal(t, 90*time.Second, time.Duration(s.Timeout))
	b, err := json.Marshal(s)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"timeout":"1m30s"`)
}

func TestCommandUnavailableFolder(t *testing.T) {
	t.Parallel()
	r := check.Run(context.Background(), check.Spec{Kind: check.KindCommand, Run: "true"},
		envIn(filepath.Join(t.TempDir(), "missing")))
	assert.Equal(t, check.StatusError, r.Status)
	assert.Equal(t, -1, r.Observed["exit_code"])
}

func TestFileNextSlot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	next := slot.AddDate(0, 0, 1)
	write(t, filepath.Join(dir, "in-window.md"), "row", slot.Add(time.Hour))
	write(t, filepath.Join(dir, "at-next.md"), "row", next)
	write(t, filepath.Join(dir, "after-next.md"), "row", next.Add(time.Hour))
	no := false
	type args struct {
		path  string
		fresh *bool
		next  time.Time
	}
	type want struct {
		status check.Status
		next   any
	}
	tcs := []struct {
		name string
		args args
		want want
	}{
		{name: "changed inside the slot", args: args{path: "in-window.md", next: next},
			want: want{status: check.StatusPass, next: "2026-10-09T13:00:00Z"}},
		{name: "changed at the next slot", args: args{path: "at-next.md", next: next},
			want: want{status: check.StatusError, next: "2026-10-09T13:00:00Z"}},
		{name: "changed after the next slot", args: args{path: "after-next.md", next: next},
			want: want{status: check.StatusError, next: "2026-10-09T13:00:00Z"}},
		{name: "no next slot", args: args{path: "after-next.md"}, want: want{status: check.StatusPass}},
		{name: "not fresh", args: args{path: "after-next.md", fresh: &no, next: next}, want: want{status: check.StatusPass}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := envIn(dir)
			env.Next = tc.args.next
			r := check.Run(context.Background(), check.Spec{Kind: check.KindFile, Path: tc.args.path, Fresh: tc.args.fresh}, env)
			assert.Equal(t, tc.want, want{status: r.Status, next: r.Observed["next_slot"]}, r.Message)
		})
	}
}

func TestHTTPBusy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if after := r.URL.Query().Get("after"); after != "" {
			w.Header().Set("Retry-After", after)
		}
		code, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			code = http.StatusOK
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	type args struct {
		path   string
		status int
	}
	type want struct {
		status check.Status
		after  any
	}
	tcs := []struct {
		name string
		args args
		want want
	}{
		{name: "429 with Retry-After", args: args{path: "/429?after=120"}, want: want{status: check.StatusError, after: "120"}},
		{name: "502", args: args{path: "/502"}, want: want{status: check.StatusError}},
		{name: "503 with a date", args: args{path: "/503?after=Wed,%2021%20Oct%202026%2007:28:00%20GMT"},
			want: want{status: check.StatusError, after: "Wed, 21 Oct 2026 07:28:00 GMT"}},
		{name: "504", args: args{path: "/504"}, want: want{status: check.StatusError}},
		{name: "long Retry-After is cut", args: args{path: "/503?after=" + strings.Repeat("9", 100)},
			want: want{status: check.StatusError, after: strings.Repeat("9", 64)}},
		{name: "500 is a fail", args: args{path: "/500"}, want: want{status: check.StatusFail}},
		{name: "expected 503", args: args{path: "/503", status: 503}, want: want{status: check.StatusPass}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := check.Run(context.Background(), check.Spec{Kind: check.KindHTTP, URL: srv.URL + tc.args.path, Status: tc.args.status},
				envIn(t.TempDir()))
			assert.Equal(t, tc.want, want{status: r.Status, after: r.Observed["retry_after"]}, r.Message)
		})
	}
}

func TestCommandOutputEnds(t *testing.T) {
	t.Parallel()
	type want struct {
		stdout []string
		bytes  int
		cut    bool
	}
	tcs := []struct {
		name string
		run  string
		want want
	}{
		{name: "short output stays whole", run: "printf 'ok'", want: want{stdout: []string{"ok"}, bytes: 2}},
		{name: "exactly head and tail stays whole", run: "head -c 4096 /dev/zero | tr '\\0' x",
			want: want{stdout: []string{strings.Repeat("x", 4096)}, bytes: 4096}},
		{name: "the reason printed last survives", run: "printf 'collected 3 items\\n'; yes . | head -c 10000; printf 'FAILED test_rows'; exit 1",
			want: want{stdout: []string{"collected 3 items", "\n… 5938 bytes cut …\n", "FAILED test_rows"}, bytes: 10034, cut: true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := check.Run(context.Background(), check.Spec{Kind: check.KindCommand, Run: tc.run}, envIn(t.TempDir()))
			stdout, ok := r.Observed["stdout"].(string)
			require.True(t, ok)
			assert.True(t, utf8.ValidString(stdout))
			assert.LessOrEqual(t, len(stdout), 4096+40)
			for _, part := range tc.want.stdout {
				assert.Contains(t, stdout, part)
			}
			assert.Equal(t, tc.want.bytes, r.Observed["stdout_bytes"])
			assert.Equal(t, tc.want.cut, r.Truncated)
		})
	}
}
