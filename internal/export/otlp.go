package export

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

// OTLP sends verdicts as traces to any OpenTelemetry backend: Langfuse, Phoenix, Opik, LangSmith or a collector
// Spans are built from the ledger with fixed ids and times, then handed straight to the OTLP exporter,
// so a failed send comes back as an error instead of vanishing in a background batcher
type OTLP struct {
	spans sdktrace.SpanExporter
	// Names maps job ids to what the owner calls them, read from the whole ledger
	names    map[string]string
	resource *resource.Resource
}

type OTLPSettings struct {
	Enabled bool `json:"enabled"`
	// Endpoint is the OTLP base URL, like https://cloud.langfuse.com/api/public/otel
	Endpoint string            `json:"endpoint"`
	Headers  map[string]string `json:"headers,omitempty"`
}

func NewOTLP(ctx context.Context, s OTLPSettings, names map[string]string, version string) (*OTLP, error) {
	spans, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(tracesURL(s.Endpoint)),
		otlptracehttp.WithHeaders(s.Headers))
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	return newOTLP(spans, names, version), nil
}

func newOTLP(spans sdktrace.SpanExporter, names map[string]string, version string) *OTLP {
	res := resource.NewSchemaless(semconv.ServiceName("rindcut"), semconv.ServiceVersion(version))
	return &OTLP{spans: spans, names: names, resource: res}
}

// The base URL gets the traces path, as OTEL_EXPORTER_OTLP_ENDPOINT does
func tracesURL(endpoint string) string {
	e := strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(e, "/v1/traces") {
		return e
	}
	return e + "/v1/traces"
}

func (o *OTLP) Name() string { return NameOTLP }

func (o *OTLP) Export(ctx context.Context, entries []ledger.Entry) error {
	var stubs tracetest.SpanStubs
	for _, e := range entries {
		switch e.Kind {
		case ledger.KindVerdict:
			var j ledger.Judgment
			if err := e.Decode(&j); err != nil {
				continue
			}
			stubs = append(stubs, o.verdictSpans(e, j)...)
		case ledger.KindCorrection:
			var c ledger.Correction
			if err := e.Decode(&c); err != nil {
				continue
			}
			stubs = append(stubs, o.correctionSpan(e, c))
		}
	}
	if len(stubs) == 0 {
		return nil
	}
	return o.spans.ExportSpans(ctx, stubs.Snapshots())
}

func (o *OTLP) Shutdown(ctx context.Context) error { return o.spans.Shutdown(ctx) }

// Test sends one span named rindcut.connection-test, so a wrong endpoint or key shows up while the owner is still talking
func (o *OTLP) Test(ctx context.Context) error {
	id := "test-" + time.Now().UTC().Format(time.RFC3339Nano)
	at := time.Now()
	stub := tracetest.SpanStub{
		Name: "rindcut.connection-test", SpanContext: spanContext(traceID(id), spanID(id, 0)),
		StartTime: at, EndTime: at, Resource: o.resource, InstrumentationScope: scope,
	}
	return o.spans.ExportSpans(ctx, tracetest.SpanStubs{stub}.Snapshots())
}

var scope = instrumentation.Scope{Name: "github.com/jeon-jihyeon/rindcut"}

// One trace per verdict: a root span for the slot and a child per check
func (o *OTLP) verdictSpans(e ledger.Entry, j ledger.Judgment) tracetest.SpanStubs {
	tid := traceID(e.ID)
	root := spanID(e.ID, 0)
	name := o.names[j.JobID]
	if name == "" {
		name = j.JobID
	}
	attrs := []attribute.KeyValue{
		attribute.String("langfuse.trace.name", name),
		attribute.StringSlice("langfuse.trace.tags", []string{"rindcut", string(j.Verdict)}),
		attribute.String("rindcut.job.id", j.JobID),
		attribute.Int("rindcut.job.revision", j.JobRevision),
		attribute.String("rindcut.slot", j.SlotKey),
		attribute.String("rindcut.verdict", string(j.Verdict)),
		attribute.String("rindcut.entry.id", e.ID),
		attribute.String("rindcut.trigger", j.Trigger),
	}
	// The newest matched session joins this trace to the Claude Code run it judged
	if n := len(j.Sessions); n > 0 {
		attrs = append(attrs, attribute.String("session.id", j.Sessions[n-1]))
	}
	if j.Note != "" {
		attrs = append(attrs, attribute.String("rindcut.note", j.Note))
	}
	status := sdktrace.Status{Code: codes.Ok}
	if j.Verdict == ledger.VerdictFail || j.Verdict == ledger.VerdictMissed {
		status = sdktrace.Status{Code: codes.Error, Description: string(j.Verdict)}
	}
	end := e.At
	if end.Before(j.Slot) {
		end = j.Slot
	}
	out := tracetest.SpanStubs{{
		Name:                 "rindcut.slot",
		SpanContext:          spanContext(tid, root),
		SpanKind:             trace.SpanKindInternal,
		StartTime:            j.Slot,
		EndTime:              end,
		Attributes:           attrs,
		Status:               status,
		Resource:             o.resource,
		InstrumentationScope: scope,
	}}
	for i, c := range j.Checks {
		out = append(out, o.checkSpan(tid, root, spanID(e.ID, i+1), c, end))
	}
	return out
}

// Checks run when the slot is judged, so their spans sit at the end of the slot span
func (o *OTLP) checkSpan(tid trace.TraceID, parent, id trace.SpanID, c ledger.CheckRecord, at time.Time) tracetest.SpanStub {
	attrs := []attribute.KeyValue{
		attribute.String("rindcut.check.kind", c.Kind),
		attribute.String("rindcut.check.status", c.Status),
		attribute.String("rindcut.check.message", c.Message),
	}
	keys := make([]string, 0, len(c.Observed))
	for k := range c.Observed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		attrs = append(attrs, attribute.String("rindcut.check.observed."+k, fmt.Sprint(c.Observed[k])))
	}
	status := sdktrace.Status{Code: codes.Ok}
	if c.Status != ledger.CheckStatusPass {
		status = sdktrace.Status{Code: codes.Error, Description: c.Message}
	}
	return tracetest.SpanStub{
		Name:                 "rindcut.check",
		SpanContext:          spanContext(tid, id),
		Parent:               spanContext(tid, parent),
		SpanKind:             trace.SpanKindInternal,
		StartTime:            at,
		EndTime:              at,
		Attributes:           attrs,
		Status:               status,
		Resource:             o.resource,
		InstrumentationScope: scope,
	}
}

// A correction joins the trace of the verdict it fixes
func (o *OTLP) correctionSpan(e ledger.Entry, c ledger.Correction) tracetest.SpanStub {
	tid := traceID(c.VerdictID)
	return tracetest.SpanStub{
		Name:        "rindcut.correction",
		SpanContext: spanContext(tid, spanID(e.ID, 0)),
		Parent:      spanContext(tid, spanID(c.VerdictID, 0)),
		SpanKind:    trace.SpanKindInternal,
		StartTime:   e.At,
		EndTime:     e.At,
		Attributes: []attribute.KeyValue{
			attribute.String("rindcut.correction.verdict", string(c.Verdict)),
			attribute.String("rindcut.correction.by", c.By),
			attribute.String("rindcut.correction.reason", c.Reason),
			attribute.String("rindcut.entry.id", e.ID),
		},
		Resource:             o.resource,
		InstrumentationScope: scope,
	}
}

// Ids derive from entry ids, so a resent batch carries the same trace and span ids
func traceID(entryID string) trace.TraceID {
	sum := sha256.Sum256([]byte(entryID))
	var t trace.TraceID
	copy(t[:], sum[:16])
	return t
}

func spanID(entryID string, n int) trace.SpanID {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	sum := sha256.Sum256(append([]byte(entryID+"/"), b[:]...))
	var s trace.SpanID
	copy(s[:], sum[:8])
	return s
}

func spanContext(t trace.TraceID, s trace.SpanID) trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: t, SpanID: s, TraceFlags: trace.FlagsSampled})
}

// TraceID is the hex trace id a verdict's spans carry, which a Langfuse score points at
func TraceID(verdictEntryID string) string { return traceID(verdictEntryID).String() }
