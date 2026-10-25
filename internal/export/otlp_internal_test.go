package export

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

// The span mapping is checked through the private constructor that takes an in-memory span exporter
var at = time.Date(2026, 10, 8, 0, 40, 0, 0, time.UTC)

func ent(t *testing.T, id string, kind ledger.Kind, data any) ledger.Entry {
	t.Helper()
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	return ledger.Entry{ID: id, Kind: kind, At: at, Data: raw}
}

func TestOTLPSpans(t *testing.T) {
	t.Parallel()
	mem := tracetest.NewInMemoryExporter()
	o := newOTLP(mem, map[string]string{"price-watch": "가격 수집"}, "0.1.0")
	es := []ledger.Entry{
		ent(t, "v1", ledger.KindVerdict, ledger.Judgment{JobID: "price-watch", SlotKey: "price-watch@x", Slot: at.Add(-40 * time.Minute),
			Verdict: ledger.VerdictFail, Sessions: []string{"older", "sess-1"},
			Checks: []ledger.CheckRecord{{Kind: "file", Status: "fail", Message: "no file", Observed: map[string]any{"exists": false}}}}),
		ent(t, "c1", ledger.KindCorrection, ledger.Correction{VerdictID: "v1", Verdict: ledger.VerdictPass, Reason: "renamed", By: "owner"}),
	}
	require.NoError(t, o.Export(context.Background(), es))
	spans := mem.GetSpans()
	require.Len(t, spans, 3)
	root, chk, fix := spans[0], spans[1], spans[2]
	attr := map[string]string{}
	for _, a := range root.Attributes {
		attr[string(a.Key)] = a.Value.String()
	}

	type got struct {
		name, session, title, verdict string
		code                          codes.Code
		start, end                    time.Time
	}
	assert.Equal(t, got{name: "rindcut.slot", session: "sess-1", title: "가격 수집", verdict: "fail", code: codes.Error, start: at.Add(-40 * time.Minute), end: at},
		got{name: root.Name, session: attr["session.id"], title: attr["langfuse.trace.name"], verdict: attr["rindcut.verdict"], code: root.Status.Code, start: root.StartTime, end: root.EndTime})
	assert.Equal(t, root.SpanContext.SpanID(), chk.Parent.SpanID(), "check span is a child of the slot span")
	assert.Equal(t, root.SpanContext.TraceID(), fix.SpanContext.TraceID(), "a correction joins the verdict's trace")
	assert.Equal(t, root.SpanContext.SpanID(), fix.Parent.SpanID())
	assert.Equal(t, TraceID("v1"), root.SpanContext.TraceID().String(), "trace ids are stable across sends")
}

func TestOTLPSendsHTTP(t *testing.T) {
	t.Parallel()
	type request struct{ path, auth string }
	got := make(chan request, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		got <- request{r.URL.Path, r.Header.Get("Authorization")}
	}))
	t.Cleanup(srv.Close)
	o, err := NewOTLP(context.Background(), OTLPSettings{Endpoint: srv.URL + "/api/public/otel/", Headers: map[string]string{"Authorization": "Basic abc"}}, nil, "dev")
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Shutdown(context.Background()) })

	require.NoError(t, o.Export(context.Background(), []ledger.Entry{ent(t, "v1", ledger.KindVerdict, ledger.Judgment{Verdict: ledger.VerdictPass})}))
	require.NoError(t, o.Test(context.Background()))
	want := request{"/api/public/otel/v1/traces", "Basic abc"}
	assert.Equal(t, want, <-got)
	assert.Equal(t, want, <-got)
}

func TestOTLPCriticStaysLocal(t *testing.T) {
	t.Parallel()
	tcs := []struct{ name, verdict string }{
		{name: "approved", verdict: "approved"},
		{name: "rejected", verdict: "rejected"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mem := tracetest.NewInMemoryExporter()
			o := newOTLP(mem, nil, "test")
			entry := ent(t, "v1", ledger.KindVerdict, ledger.Judgment{JobID: "report", Slot: at,
				Verdict: ledger.VerdictPass, Critic: &ledger.CriticResult{Source: "acme",
					Verdict: ledger.CriticVerdict(tc.verdict), Reason: "acme local explanation"}})
			require.NoError(t, o.Export(context.Background(), []ledger.Entry{entry}))
			spans := mem.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, codes.Ok, spans[0].Status.Code)
			for _, attr := range spans[0].Attributes {
				assert.NotContains(t, string(attr.Key), "critic")
				assert.NotContains(t, attr.Value.String(), "acme local explanation")
			}
		})
	}
}
