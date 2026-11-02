package export

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"time"

	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const maxOTLPResponse = 4 << 20

type otlpClient struct {
	endpoint string
	headers  map[string]string
	http     *http.Client
	lifetime context.Context
	stop     context.CancelFunc
}

type partialDelivery struct{ rejected int64 }

func (e partialDelivery) Error() string {
	return fmt.Sprintf("OTLP partial success: %d spans rejected; batch will not be replayed", e.rejected)
}

type refusedDelivery string

func (e refusedDelivery) Error() string {
	return string(e)
}

func newOTLPClient(endpoint string, headers map[string]string) (*otlpClient, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.Scheme != "https" && u.Scheme != "http" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid OTLP endpoint")
	}
	transport := http.DefaultTransport
	if base, ok := transport.(*http.Transport); ok {
		transport = base.Clone()
	}
	lifetime, stop := context.WithCancel(context.Background())
	return &otlpClient{endpoint: endpoint, headers: maps.Clone(headers), lifetime: lifetime, stop: stop, http: &http.Client{
		Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *otlpClient) Start(ctx context.Context) error { return ctx.Err() }

func (c *otlpClient) Stop(ctx context.Context) error {
	c.stop()
	c.http.CloseIdleConnections()
	return ctx.Err()
}

func (c *otlpClient) UploadTraces(ctx context.Context, spans []*tracepb.ResourceSpans) error {
	if c.lifetime.Err() != nil {
		return errors.New("OTLP exporter stopped")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.lifetime, cancel)
	defer stop()
	data, err := proto.Marshal(&collector.ExportTraceServiceRequest{ResourceSpans: spans})
	if err != nil {
		return errors.New("encode OTLP request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid OTLP request")
	}
	for key, value := range c.headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("OTLP request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 429 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 {
			return fmt.Errorf("OTLP backend unavailable with HTTP %d", resp.StatusCode)
		}
		return refusedDelivery(fmt.Sprintf("OTLP request refused with HTTP %d", resp.StatusCode))
	}
	return readOTLPResponse(resp)
}

func readOTLPResponse(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOTLPResponse+1))
	if len(body) > maxOTLPResponse {
		return refusedDelivery("OTLP response exceeds size limit")
	}
	if err != nil {
		return errors.New("invalid OTLP response read")
	}
	if len(body) == 0 {
		return nil
	}
	var reply collector.ExportTraceServiceResponse
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch contentType {
	case "application/x-protobuf":
		err = proto.Unmarshal(body, &reply)
	case "application/json":
		err = (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, &reply)
	default:
		return errors.New("unsupported OTLP response type")
	}
	if err != nil {
		return errors.New("invalid OTLP response encoding")
	}
	if partial := reply.PartialSuccess; partial != nil {
		return partialDelivery{rejected: max(0, partial.RejectedSpans)}
	}
	return nil
}
