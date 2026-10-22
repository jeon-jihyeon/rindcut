// Package job holds what a scheduled job promises: when it runs and what proves it worked
package job

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jeon-jihyeon/rindcut/internal/check"
)

// Evidence says where rindcut can see a job's run
type Evidence string

const (
	EvidenceSession Evidence = "session" // runs as a local Claude session the plugin hook records
	EvidenceNone    Evidence = "none"    // runs where rindcut can't see, like a cloud schedule
)

const (
	maxIDLen     = 48
	maxNameLen   = 200
	maxMarkerLen = 200
	maxChecks    = 10
	minGrace     = time.Minute
	maxGrace     = 24 * time.Hour
)

var idPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Job is one revision of a job definition
// A change is a new revision, never an edit, so a verdict always names the definition it used
type Job struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Schedule string         `json:"schedule"`
	Timezone string         `json:"timezone"`
	Grace    check.Duration `json:"grace"`
	Evidence Evidence       `json:"evidence"`
	// Folder and Marker identify the job's session and are required with session evidence
	Folder   string       `json:"folder,omitempty"`
	Marker   string       `json:"marker,omitempty"`
	Checks   []check.Spec `json:"checks"`
	Revision int          `json:"revision"`
	Removed  bool         `json:"removed,omitempty"`
	// Since is when the revision was recorded
	// It comes from the record that stores the job, not from the job's own text
	Since time.Time `json:"-"`
}

func (j Job) Validate() error {
	if len(j.ID) > maxIDLen || !idPattern.MatchString(j.ID) {
		return fmt.Errorf("id %q must be lowercase letters, digits and dashes, up to %d chars", j.ID, maxIDLen)
	}
	if strings.TrimSpace(j.Name) == "" || len(j.Name) > maxNameLen {
		return fmt.Errorf("name must be 1 to %d chars", maxNameLen)
	}
	if j.Revision < 1 {
		return errors.New("revision starts at 1")
	}
	if j.Removed {
		return nil
	}
	if err := j.validateTiming(); err != nil {
		return err
	}
	if err := j.validateEvidence(); err != nil {
		return err
	}
	return j.validateChecks()
}

func (j Job) validateChecks() error {
	if len(j.Checks) == 0 || len(j.Checks) > maxChecks {
		return fmt.Errorf("a job needs 1 to %d checks", maxChecks)
	}
	for i, c := range j.Checks {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("check %d: %w", i+1, err)
		}
		// A relative path is read from the job folder, and the tick runs from /, so a job without one needs a full path
		if c.Kind == check.KindFile && j.Folder == "" && !filepath.IsAbs(c.Path) && !strings.HasPrefix(c.Path, "~/") {
			return fmt.Errorf("check %d: path %q must start with / or ~/ for a job without a folder", i+1, c.Path)
		}
	}
	return nil
}

func (j Job) validateTiming() error {
	if _, err := parseSchedule(j.Schedule); err != nil {
		return err
	}
	if _, err := time.LoadLocation(j.Timezone); err != nil || j.Timezone == "" {
		return fmt.Errorf("timezone %q must be an IANA name like UTC", j.Timezone)
	}
	if g := time.Duration(j.Grace); g < minGrace || g > maxGrace {
		return fmt.Errorf("grace %s must be between %s and %s", g, minGrace, maxGrace)
	}
	return nil
}

func (j Job) validateEvidence() error {
	switch j.Evidence {
	case EvidenceNone:
		return nil
	case EvidenceSession:
	default:
		return fmt.Errorf("evidence %q must be session or none", j.Evidence)
	}
	if !filepath.IsAbs(j.Folder) {
		return fmt.Errorf("folder %q must be an absolute path for a session job", j.Folder)
	}
	// Counted in characters, since the session side keeps the first 200 characters of a prompt
	m := strings.TrimSpace(j.Marker)
	if m == "" || utf8.RuneCountInString(m) > maxMarkerLen || strings.ContainsAny(m, "\r\n") {
		return fmt.Errorf("marker must be the scheduled prompt's first line, 1 to %d chars", maxMarkerLen)
	}
	return nil
}

// Slug turns what the owner calls a job into an id
// A name with no ASCII letters or digits, like a Korean one, gets a stable hash id
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" {
		sum := sha256.Sum256([]byte(strings.TrimSpace(name)))
		return "job-" + hex.EncodeToString(sum[:4])
	}
	if len(id) > maxIDLen {
		id = strings.TrimRight(id[:maxIDLen], "-")
	}
	return id
}

// Current is the latest revision of every job
func Current(revs []Job) map[string]Job {
	out := map[string]Job{}
	for _, r := range revs {
		if cur, ok := out[r.ID]; !ok || r.Revision > cur.Revision {
			out[r.ID] = r
		}
	}
	return out
}

// At is the revision of a job in force at t
// False means the job didn't exist yet
func At(revs []Job, id string, t time.Time) (Job, bool) {
	var found Job
	ok := false
	for _, r := range revs {
		if r.ID != id || r.Since.After(t) {
			continue
		}
		if !ok || r.Revision > found.Revision {
			found, ok = r, true
		}
	}
	return found, ok
}
