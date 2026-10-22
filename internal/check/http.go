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
	// A `Retry-After` value is a date or seconds, so anything longer is cut
	keptRetryAfter = 64
)

// Statuses that say the server is busy or down for now, so the check couldn't look and an unexpected one is an error, not a fail
var busyStatus = map[int]bool{
	http.StatusTooManyRequests:    true,
	http.StatusBadGateway:         true,
	http.StatusServiceUnavailable: true,
	http.StatusGatewayTimeout:     true,
}

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
	if after := resp.Header.Get("Retry-After"); after != "" {
		observed["retry_after"], r.Truncated = head([]byte(after), keptRetryAfter)
	}
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
	r.Truncated = r.Truncated || cut || offset > 0 || limitReached
	if err != nil {
		r.Status, r.Message = StatusError, fmt.Sprintf("reading %s broke off: %v", target, err)
		return r
	}
	r.Status, r.Message = s.answer(target, resp.StatusCode, contains, match >= 0)
	return r
}

func (s Spec) answer(target string, code int, contains string, matched bool) (Status, string) {
	switch {
	case code != s.status() && busyStatus[code]:
		return StatusError, fmt.Sprintf("%s answered %d, busy or down for now, expected %d", target, code, s.status())
	case code != s.status():
		return StatusFail, fmt.Sprintf("%s answered %d, expected %d", target, code, s.status())
	case contains != "" && !matched:
		return StatusFail, fmt.Sprintf("%s doesn't contain %q", target, contains)
	default:
		return StatusPass, fmt.Sprintf("%s answered %d", target, code)
	}
}

// The first n bytes as text, cut back to a whole UTF-8 character
func head(b []byte, n int) (string, bool) {
	if len(b) <= n {
		return string(b), false
	}
	return strings.ToValidUTF8(string(b[:n]), ""), true
}
