package export_test

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/jeon-jihyeon/rindcut/internal/export"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

func TestOTLPPartialSuccessDoesNotReplayAcceptedBatch(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name     string
		rejected int64
		message  string
	}{
		{name: "one span rejected", rejected: 1, message: "invalid span"},
		{name: "warning without rejection", message: "limited attributes"},
		{name: "all spans rejected", rejected: 2, message: "invalid batch"},
		{name: "reported count saturates", rejected: math.MaxInt64, message: "large reported count"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			response, err := proto.Marshal(&collector.ExportTraceServiceResponse{
				PartialSuccess: &collector.ExportTracePartialSuccess{
					RejectedSpans: tc.rejected, ErrorMessage: tc.message,
				},
			})
			require.NoError(t, err)
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/x-protobuf")
				_, _ = w.Write(response)
			}))
			t.Cleanup(server.Close)
			exporter, err := export.NewOTLP(context.Background(), export.OTLPSettings{Endpoint: server.URL}, nil, "test")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, exporter.Shutdown(context.Background())) })
			dir := t.TempDir()
			runner := export.Runner{Exporters: []export.Exporter{exporter}, CursorDir: dir,
				Now: func() time.Time { return t0 }}
			entries := []ledger.Entry{verdict(t, "partial", ledger.VerdictPass)}
			want := []export.Lag{{Exporter: export.NameOTLP, RejectedSpans: tc.rejected,
				LastWarning: fmt.Sprintf("OTLP partial success: %d spans rejected; batch will not be replayed", tc.rejected)}}
			assert.Equal(t, want, runner.Run(context.Background(), entries))
			assert.Equal(t, want, runner.Run(context.Background(), entries))
			assert.Equal(t, int64(1), requests.Load())
			assert.Equal(t, want, export.Lags(dir, []string{export.NameOTLP}, entries))
			entries = append(entries, verdict(t, "later", ledger.VerdictPass))
			want[0].RejectedSpans += min(tc.rejected, math.MaxInt64-tc.rejected)
			assert.Equal(t, want, runner.Run(context.Background(), entries))
			assert.Equal(t, int64(2), requests.Load())
		})
	}
}

func TestOTLPDeliveryResponse(t *testing.T) {
	t.Parallel()
	type args struct {
		status        int
		contentType   string
		body          string
		contentLength int
	}
	type want struct {
		pending  int
		dropped  int
		requests int64
		rejected int64
		warning  bool
	}
	tcs := []struct {
		name string
		args args
		want want
	}{
		{name: "empty success", args: args{status: 200}, want: want{requests: 1}},
		{name: "protobuf success", args: args{status: 200, contentType: "application/x-protobuf"}, want: want{requests: 1}},
		{name: "JSON success", args: args{status: 200, contentType: "application/json; charset=utf-8", body: `{}`},
			want: want{requests: 1}},
		{name: "future JSON success", args: args{status: 200, contentType: "application/json", body: `{"futureField":true}`},
			want: want{requests: 1}},
		{name: "JSON partial", args: args{status: 200, contentType: "application/json",
			body: `{"partialSuccess":{"rejectedSpans":"1","errorMessage":"secret-response"}}`},
			want: want{requests: 1, rejected: 1, warning: true}},
		{name: "future JSON partial", args: args{status: 200, contentType: "application/json",
			body: `{"partialSuccess":{"rejectedSpans":"1","futureField":true},"futureTop":true}`},
			want: want{requests: 1, rejected: 1, warning: true}},
		{name: "empty populated partial", args: args{status: 200, contentType: "application/json", body: `{"partialSuccess":{}}`},
			want: want{requests: 1, warning: true}},
		{name: "bad data", args: args{status: 400, body: "secret-response"}, want: want{dropped: 2, requests: 1}},
		{name: "unauthorized", args: args{status: 401, body: "secret-response"}, want: want{dropped: 2, requests: 1}},
		{name: "redirect", args: args{status: 307, body: "secret-response"}, want: want{dropped: 2, requests: 1}},
		{name: "throttled", args: args{status: 429, body: "secret-response"}, want: want{pending: 2, requests: 2}},
		{name: "unavailable", args: args{status: 503, body: "secret-response"}, want: want{pending: 2, requests: 2}},
		{name: "invalid protobuf", args: args{status: 200, contentType: "application/x-protobuf", body: "secret-response"},
			want: want{pending: 2, requests: 2}},
		{name: "invalid JSON", args: args{status: 200, contentType: "application/json", body: "secret-response"},
			want: want{pending: 2, requests: 2}},
		{name: "unsupported encoding", args: args{status: 200, contentType: "text/html", body: "secret-response"},
			want: want{pending: 2, requests: 2}},
		{name: "response exactly at limit", args: args{status: 200, contentType: "application/json",
			body: `{}` + strings.Repeat(" ", (4<<20)-2)}, want: want{requests: 1}},
		{name: "response too large", args: args{status: 200, contentType: "application/json",
			body: `{}` + strings.Repeat(" ", (4<<20)-1)}, want: want{dropped: 2, requests: 1}},
		{name: "response read interrupted below limit", args: args{status: 200, contentType: "application/json",
			body: `{}`, contentLength: (4 << 20) + 1}, want: want{pending: 2, requests: 2}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", tc.args.contentType)
				w.Header().Set("Location", r.URL.String()+"?secret-response")
				if tc.args.contentLength > 0 {
					w.Header().Set("Content-Length", fmt.Sprint(tc.args.contentLength))
				}
				w.WriteHeader(tc.args.status)
				_, _ = w.Write([]byte(tc.args.body))
			}))
			t.Cleanup(server.Close)
			exporter, err := export.NewOTLP(context.Background(), export.OTLPSettings{Endpoint: server.URL}, nil, "test")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, exporter.Shutdown(context.Background())) })
			runner := export.Runner{Exporters: []export.Exporter{exporter}, CursorDir: t.TempDir(),
				Now: func() time.Time { return t0 }}
			entries := []ledger.Entry{verdict(t, "v1", ledger.VerdictPass), verdict(t, "v2", ledger.VerdictFail)}
			runner.Run(context.Background(), entries)
			lags := runner.Run(context.Background(), entries)
			require.Len(t, lags, 1)
			assert.Equal(t, tc.want.requests, requests.Load())
			assert.Equal(t, tc.want.pending, lags[0].Pending)
			assert.Equal(t, tc.want.dropped, lags[0].Dropped)
			assert.Equal(t, tc.want.rejected, lags[0].RejectedSpans)
			assert.Equal(t, tc.want.warning, lags[0].LastWarning != "")
			assert.NotContains(t, fmt.Sprint(lags), "secret-response")
		})
	}
}

func TestOTLPTLSAndCancellation(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name   string
		tls    bool
		cancel bool
	}{
		{name: "untrusted TLS certificate", tls: true},
		{name: "canceled before request", cancel: true},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int64
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
			}))
			if tc.tls {
				server.StartTLS()
			} else {
				server.Start()
			}
			t.Cleanup(server.Close)
			exporter, err := export.NewOTLP(context.Background(), export.OTLPSettings{
				Endpoint: server.URL + "/private-token", Headers: map[string]string{"Authorization": "private-auth"},
			}, nil, "test")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, exporter.Shutdown(context.Background())) })
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			if tc.cancel {
				cancel()
			}
			err = exporter.Export(ctx, []ledger.Entry{verdict(t, "v1", ledger.VerdictPass)})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private-token")
			assert.NotContains(t, err.Error(), "private-auth")
			assert.Equal(t, int64(0), requests.Load())
		})
	}
}

func TestOTLPRedirectDoesNotForwardCredentials(t *testing.T) {
	t.Parallel()
	var forwarded atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		forwarded.Add(1)
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	exporter, err := export.NewOTLP(context.Background(), export.OTLPSettings{
		Endpoint: origin.URL, Headers: map[string]string{"Authorization": "private-auth"},
	}, nil, "test")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, exporter.Shutdown(context.Background())) })
	err = exporter.Export(context.Background(), []ledger.Entry{verdict(t, "v1", ledger.VerdictPass)})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "private-auth")
	assert.Equal(t, int64(0), forwarded.Load())
}

func TestOTLPShutdownCancelsRequestsAndRejectsLaterExports(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests.Add(1)
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	exporter, err := export.NewOTLP(context.Background(), export.OTLPSettings{Endpoint: server.URL}, nil, "test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = exporter.Shutdown(context.Background()) })
	entries := []ledger.Entry{verdict(t, "v1", ledger.VerdictPass)}
	done := make(chan error, 1)
	go func() { done <- exporter.Export(context.Background(), entries) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	require.NoError(t, exporter.Shutdown(context.Background()))
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel the request")
	}
	require.Error(t, exporter.Export(context.Background(), entries))
	assert.Equal(t, int64(1), requests.Load())
}
