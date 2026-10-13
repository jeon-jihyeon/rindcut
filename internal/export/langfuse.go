package export

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

// LangfuseScore records each verdict as a categorical score on the verdict's trace
// It needs OTLP sending to the same Langfuse project, since the score points at that trace
type LangfuseScore struct {
	s      LangfuseSettings
	client *http.Client
}

type LangfuseSettings struct {
	Enabled   bool   `json:"enabled"`
	Host      string `json:"host"`
	PublicKey string `json:"public_key"`
	SecretKey string `json:"secret_key"`
}

func NewLangfuseScore(s LangfuseSettings) *LangfuseScore {
	return &LangfuseScore{s: s, client: &http.Client{Timeout: 30 * time.Second}}
}

func (l *LangfuseScore) Name() string { return NameLangfuse }

type score struct {
	ID       string `json:"id"`
	TraceID  string `json:"traceId"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	DataType string `json:"dataType"`
	Comment  string `json:"comment,omitempty"`
}

// A verdict is score rindcut.verdict and a correction is score rindcut.correction on the same trace
// Langfuse overwrites a score only when id, name and the day it was written all match,
// and the API doesn't take a timestamp, so a later correction can't rewrite the original and sits beside it
func (l *LangfuseScore) Export(ctx context.Context, entries []ledger.Entry) error {
	for _, e := range entries {
		sc, ok := toScore(e)
		if !ok {
			continue
		}
		if err := l.post(ctx, sc); err != nil {
			return err
		}
	}
	return nil
}

func toScore(e ledger.Entry) (score, bool) {
	switch e.Kind {
	case ledger.KindVerdict:
		var j ledger.Judgment
		if e.Decode(&j) != nil {
			return score{}, false
		}
		return score{ID: e.ID, TraceID: TraceID(e.ID), Name: "rindcut.verdict", Value: string(j.Verdict), DataType: "CATEGORICAL", Comment: j.Note}, true
	case ledger.KindCorrection:
		var c ledger.Correction
		if e.Decode(&c) != nil {
			return score{}, false
		}
		return score{ID: e.ID, TraceID: TraceID(c.VerdictID), Name: "rindcut.correction", Value: string(c.Verdict), DataType: "CATEGORICAL", Comment: c.Reason}, true
	}
	return score{}, false
}

func (l *LangfuseScore) post(ctx context.Context, sc score) error {
	body, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	url := strings.TrimRight(l.s.Host, "/") + "/api/public/scores"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return l.do(req)
}

// Test reads the project the keys belong to, which proves host and keys without writing anything
func (l *LangfuseScore) Test(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(l.s.Host, "/")+"/api/public/projects", http.NoBody)
	if err != nil {
		return err
	}
	return l.do(req)
}

// do sends with the project keys
// A 4xx other than a timeout or a rate limit is permanent, so the runner drops it instead of retrying forever
func (l *LangfuseScore) do(req *http.Request) error {
	req.SetBasicAuth(l.s.PublicKey, l.s.SecretKey)
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("langfuse: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 300 {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	err = fmt.Errorf("langfuse: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	retryable := resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests
	if !retryable {
		return PermanentError{Err: err}
	}
	return err
}
