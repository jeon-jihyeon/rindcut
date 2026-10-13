// Package check looks at the real state a job was supposed to produce
package check

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Kind string

const (
	KindFile    Kind = "file"
	KindHTTP    Kind = "http"
	KindCommand Kind = "command"
)

const (
	defaultHTTPTimeout    = 30 * time.Second
	defaultCommandTimeout = 60 * time.Second
	maxTimeout            = 10 * time.Minute
)

// Spec is one condition a job's output must meet
// Only the fields of its kind are set
type Spec struct {
	Kind Kind `json:"kind"`
	// file
	Path     string `json:"path,omitempty"`
	MinBytes int64  `json:"min_bytes,omitempty"`
	// Nil means true, so a file older than its slot fails unless the owner said otherwise
	Fresh *bool `json:"fresh,omitempty"`
	// http
	URL    string `json:"url,omitempty"`
	Status int    `json:"status,omitempty"`
	// Contains applies to http bodies only
	Contains string `json:"contains,omitempty"`
	// command
	Run     string   `json:"run,omitempty"`
	Timeout Duration `json:"timeout,omitempty"`
}

var placeholder = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

// Template variables a check field may use
var variables = map[string]bool{"date": true, "yesterday": true, "slot": true, "job": true}

func (s Spec) Validate() error {
	if err := s.validateKind(); err != nil {
		return err
	}
	if s.Timeout < 0 || time.Duration(s.Timeout) > maxTimeout {
		return fmt.Errorf("%s check: timeout must be between 0 and %s", s.Kind, maxTimeout)
	}
	for _, field := range []string{s.Path, s.URL, s.Contains, s.Run} {
		for _, m := range placeholder.FindAllStringSubmatch(field, -1) {
			if !variables[m[1]] {
				return fmt.Errorf("%s check: unknown variable {{%s}}, use date, yesterday, slot or job", s.Kind, m[1])
			}
		}
	}
	return nil
}

func (s Spec) validateKind() error {
	switch s.Kind {
	case KindFile:
		if strings.TrimSpace(s.Path) == "" {
			return errors.New("file check: path is required")
		}
		if s.MinBytes < 0 {
			return errors.New("file check: min_bytes can't be negative")
		}
	case KindHTTP:
		u, err := url.Parse(placeholder.ReplaceAllString(s.URL, "x"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("http check: url %q must be an http or https URL", s.URL)
		}
		if s.Status != 0 && (s.Status < 100 || s.Status > 599) {
			return fmt.Errorf("http check: status %d is not an HTTP status", s.Status)
		}
	case KindCommand:
		if strings.TrimSpace(s.Run) == "" {
			return errors.New("command check: run is required")
		}
	default:
		return fmt.Errorf("unknown check kind %q, use file, http or command", s.Kind)
	}
	return nil
}

func (s Spec) fresh() bool { return s.Fresh == nil || *s.Fresh }

func (s Spec) minBytes() int64 {
	if s.MinBytes == 0 {
		return 1
	}
	return s.MinBytes
}

func (s Spec) status() int {
	if s.Status == 0 {
		return 200
	}
	return s.Status
}

func (s Spec) timeout() time.Duration {
	switch {
	case s.Timeout > 0:
		return time.Duration(s.Timeout)
	case s.Kind == KindHTTP:
		return defaultHTTPTimeout
	default:
		return defaultCommandTimeout
	}
}

// Duration reads and writes Go duration text like "30m", which is what an owner and a model both write
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be text like \"30m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
