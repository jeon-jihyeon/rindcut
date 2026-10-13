package check

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// A body larger than this is still checked up to here, enough for any page a job writes
	maxBody      = 1 << 20
	keptBodyText = 2 << 10
)

func runHTTP(ctx context.Context, s Spec, env Env) Result {
	target := env.expand(s.URL)
	observed := map[string]any{"url": target}
	r := Result{Kind: KindHTTP, Observed: observed}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		r.Status, r.Message = StatusError, fmt.Sprintf("bad url %s: %v", target, err)
		return r
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.Status, r.Message = StatusError, fmt.Sprintf("couldn't reach %s: %v", target, err)
		return r
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	observed["elapsed_ms"] = time.Since(start).Milliseconds()
	observed["status"] = resp.StatusCode
	text, cut := head(body, keptBodyText)
	observed["body"] = text
	r.Truncated = cut
	if err != nil {
		r.Status, r.Message = StatusError, fmt.Sprintf("reading %s broke off: %v", target, err)
		return r
	}
	contains := env.expand(s.Contains)
	switch {
	case resp.StatusCode != s.status():
		r.Status, r.Message = StatusFail, fmt.Sprintf("%s answered %d, expected %d", target, resp.StatusCode, s.status())
	case contains != "" && !strings.Contains(string(body), contains):
		r.Status, r.Message = StatusFail, fmt.Sprintf("%s doesn't contain %q", target, contains)
	default:
		r.Status, r.Message = StatusPass, fmt.Sprintf("%s answered %d", target, resp.StatusCode)
	}
	return r
}

// The first n bytes as text, cut back to a whole UTF-8 character
func head(b []byte, n int) (string, bool) {
	if len(b) <= n {
		return string(b), false
	}
	return strings.ToValidUTF8(string(b[:n]), ""), true
}
