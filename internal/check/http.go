package check

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	observed["elapsed_ms"] = time.Since(start).Milliseconds()
	observed["status"] = resp.StatusCode
	limitReached := len(body) > maxBody
	if limitReached {
		body = body[:maxBody]
	}
	observed["bytes_read"] = len(body)
	observed["body_limit"] = maxBody
	observed["limit_reached"] = limitReached
	contains := env.expand(s.Contains)
	match := -1
	if contains != "" {
		match = strings.Index(string(body), contains)
	}
	observed["match_offset"] = match
	offset := 0
	if match >= 0 {
		observed["match"] = contains
		observed["match_bytes"] = len(contains)
		offset = max(0, match-keptBodyText/2)
		for offset > 0 && !utf8.RuneStart(body[offset]) {
			offset--
		}
	}
	text, cut := head(body[offset:], keptBodyText)
	observed["body"] = text
	observed["body_offset"] = offset
	r.Truncated = cut || offset > 0 || limitReached
	if err != nil {
		r.Status, r.Message = StatusError, fmt.Sprintf("reading %s broke off: %v", target, err)
		return r
	}
	switch {
	case resp.StatusCode != s.status():
		r.Status, r.Message = StatusFail, fmt.Sprintf("%s answered %d, expected %d", target, resp.StatusCode, s.status())
	case contains != "" && match < 0:
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
