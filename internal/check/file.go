package check

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

func runFile(s Spec, env Env) Result {
	path := env.path(s.Path)
	observed := map[string]any{"path": path, "exists": false}
	r := Result{Kind: KindFile, Observed: observed}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.Status, r.Message = StatusFail, "no file at "+path
		return r
	case err != nil:
		r.Status, r.Message = StatusError, fmt.Sprintf("can't read %s: %v", path, err)
		return r
	}
	observed["exists"] = true
	observed["size"] = info.Size()
	observed["mtime"] = info.ModTime().UTC().Format(time.RFC3339)
	if s.fresh() && !env.Next.IsZero() {
		observed["next_slot"] = env.Next.UTC().Format(time.RFC3339)
	}
	switch {
	case info.IsDir():
		r.Status, r.Message = StatusFail, path+" is a folder, not a file"
	case info.Size() < s.minBytes():
		r.Status, r.Message = StatusFail, fmt.Sprintf("%s has %d bytes, needs at least %d", path, info.Size(), s.minBytes())
	case s.fresh() && info.ModTime().Before(env.Slot):
		r.Status, r.Message = StatusFail, fmt.Sprintf("%s was last changed %s, before the slot at %s",
			path, info.ModTime().Format(time.RFC3339), env.Slot.Format(time.RFC3339))
	// A later run may have written it, so it can't prove this slot's run
	case s.fresh() && !env.Next.IsZero() && !info.ModTime().Before(env.Next):
		r.Status, r.Message = StatusError, fmt.Sprintf("%s was last changed %s, at or after the next slot at %s, so a later run may have written it",
			path, info.ModTime().Format(time.RFC3339), env.Next.Format(time.RFC3339))
	default:
		r.Status, r.Message = StatusPass, path+" is there"
	}
	return r
}
