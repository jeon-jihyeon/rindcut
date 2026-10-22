package check

import (
	"context"
	"path/filepath"
	"strings"
	"time"
)

type Status string

const (
	StatusPass  Status = "pass"
	StatusFail  Status = "fail"  // the state was read and doesn't meet the condition
	StatusError Status = "error" // the state couldn't be read, so nothing is known
)

// Result is what one check concluded and what it saw on the way
type Result struct {
	Kind      Kind           `json:"kind"`
	Status    Status         `json:"status"`
	Message   string         `json:"message"`
	Observed  map[string]any `json:"observed"`
	Truncated bool           `json:"truncated,omitempty"`
}

// Env is the slot a check judges and the places it resolves paths against
type Env struct {
	Slot time.Time
	// Next is the following slot, and a fresh file changed at or after it can't prove this slot
	// Zero when the job has no next slot
	Next   time.Time
	JobID  string
	Folder string
	// UserHome expands a leading ~ in file paths
	UserHome string
	// Dir runs commands when the job has no folder
	Dir string
}

// Run never returns an error
// Every failure to look becomes a Result with status error
func Run(ctx context.Context, s Spec, env Env) Result {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	switch s.Kind {
	case KindFile:
		return runFile(s, env)
	case KindHTTP:
		return runHTTP(ctx, s, env)
	default:
		return runCommand(ctx, s, env)
	}
}

func (e Env) expand(text string) string {
	day := e.Slot.Format(time.DateOnly)
	values := map[string]string{
		"date":      day,
		"yesterday": e.Slot.AddDate(0, 0, -1).Format(time.DateOnly),
		"slot":      e.Slot.Format(time.RFC3339),
		"job":       e.JobID,
	}
	return placeholder.ReplaceAllStringFunc(text, func(m string) string {
		return values[placeholder.FindStringSubmatch(m)[1]]
	})
}

// A relative path is read from the job folder, so "out/today.csv" means what the owner meant
func (e Env) path(raw string) string {
	p := e.expand(raw)
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(e.UserHome, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) && e.Folder != "" {
		p = filepath.Join(e.Folder, p)
	}
	return filepath.Clean(p)
}

func (e Env) dir() string {
	if e.Folder != "" {
		return e.Folder
	}
	return e.Dir
}
