package check_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/check"
)

func TestHTTPMatchEvidence(t *testing.T) {
	t.Parallel()
	type args struct {
		body       string
		contains   string
		incomplete bool
	}
	type want struct {
		status  check.Status
		offset  int
		read    int
		limited bool
		match   string
	}
	tcs := []struct {
		name string
		args args
		want want
	}{
		{name: "late first match", args: args{body: strings.Repeat("x", 5000) + "ready ready", contains: "ready"},
			want: want{status: check.StatusPass, offset: 5000, read: 5011, match: "ready"}},
		{name: "multibyte context", args: args{body: strings.Repeat("文", 1700) + "ready", contains: "ready"},
			want: want{status: check.StatusPass, offset: 5100, read: 5105, match: "ready"}},
		{name: "missing match", args: args{body: "pending", contains: "ready"},
			want: want{status: check.StatusFail, offset: -1, read: 7}},
		{name: "no contains condition", args: args{body: "ready"},
			want: want{status: check.StatusPass, offset: -1, read: 5}},
		{name: "exact boundary", args: args{body: strings.Repeat("x", (1<<20)-5) + "ready", contains: "ready"},
			want: want{status: check.StatusPass, offset: (1 << 20) - 5, read: 1 << 20, match: "ready"}},
		{name: "outside boundary", args: args{body: strings.Repeat("x", 1<<20) + "ready", contains: "ready"},
			want: want{status: check.StatusFail, offset: -1, read: 1 << 20, limited: true}},
		{name: "match longer than context", args: args{body: strings.Repeat("m", 4096), contains: strings.Repeat("m", 4096)},
			want: want{status: check.StatusPass, offset: 0, read: 4096, match: strings.Repeat("m", 4096)}},
		{name: "partial read retains match", args: args{body: "ready", contains: "ready", incomplete: true},
			want: want{status: check.StatusError, offset: 0, read: 5, match: "ready"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.args.incomplete {
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.args.body)+1))
				}
				_, _ = w.Write([]byte(tc.args.body))
			}))
			defer srv.Close()
			r := check.Run(context.Background(), check.Spec{Kind: check.KindHTTP, URL: srv.URL, Contains: tc.args.contains},
				envIn(t.TempDir()))
			assert.Equal(t, tc.want.status, r.Status)
			assert.Equal(t, tc.want.offset, r.Observed["match_offset"])
			assert.Equal(t, tc.want.read, r.Observed["bytes_read"])
			assert.Equal(t, tc.want.limited, r.Observed["limit_reached"])
			text, ok := r.Observed["body"].(string)
			require.True(t, ok)
			assert.True(t, utf8.ValidString(text))
			assert.LessOrEqual(t, len(text), 2048)
			assert.Equal(t, 1<<20, r.Observed["body_limit"])
			match, _ := r.Observed["match"].(string)
			assert.Equal(t, tc.want.match, match)
			count, _ := r.Observed["match_bytes"].(int)
			assert.Equal(t, len(tc.want.match), count)
		})
	}
}
