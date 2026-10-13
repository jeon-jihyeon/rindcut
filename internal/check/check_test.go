package check_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/check"
)

var slot = time.Date(2026, 10, 8, 9, 0, 0, 0, time.FixedZone("KST", 9*3600))

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
