// Package judge turns a closed slot into one verdict with its evidence
package judge

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/check"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

const noSessionNote = "no session matched the job folder, marker and window"

// Decide combines check results with session evidence
// 1. Outputs win: every check passing is a pass even with no matched session
// 2. One clear failure beats any number of errors
// 3. A failure without a matching session is missed for a session job
func Decide(results []check.Result, ev job.Evidence, matched bool) (ledger.Verdict, string) {
	failed, errored := false, false
	for _, r := range results {
		switch r.Status {
		case check.StatusFail:
			failed = true
		case check.StatusError:
			errored = true
		}
	}
	session := ev == job.EvidenceSession
	switch {
	case failed && session && !matched:
		return ledger.VerdictMissed, noSessionNote
	case failed:
		return ledger.VerdictFail, ""
	case errored:
		return ledger.VerdictInconclusive, ""
	case session && !matched:
		return ledger.VerdictPass, noSessionNote
	default:
		return ledger.VerdictPass, ""
	}
}

// Session evidence binds recorded fields without identifying the scheduler
func Matches(j job.Job, s job.Slot, sess ledger.Session) bool {
	if sess.StartedAt.Before(s.At) || sess.StartedAt.After(s.End) {
		return false
	}
	return sameJob(j, sess)
}

// Late finds the earliest complete session of the job that started after the slot's window closed and before the next slot
// A scheduler that wakes from sleep runs a missed time once, so a missed slot may have run hours late
func Late(j job.Job, s job.Slot, sessions []ledger.Session) (ledger.Session, bool) {
	next, bounded := j.Next(s.At)
	var found ledger.Session
	ok := false
	for _, sess := range sessions {
		started := sess.StartedAt
		late := started.After(s.End) && (!bounded || started.Before(next.At))
		earliest := !ok || started.Before(found.StartedAt)
		if late && earliest && sess.Complete() && sameJob(j, sess) {
			found, ok = sess, true
		}
	}
	return found, ok
}

func sameJob(j job.Job, sess ledger.Session) bool {
	if strings.TrimSpace(firstLine(sess.FirstPrompt)) != strings.TrimSpace(j.Marker) {
		return false
	}
	return samePath(sess.Cwd, j.Folder)
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// Paths compare after resolving symlinks, since macOS reports /private/var for /var
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

func records(results []check.Result) []ledger.CheckRecord {
	out := make([]ledger.CheckRecord, len(results))
	for i, r := range results {
		out[i] = ledger.CheckRecord{
			Kind: string(r.Kind), Status: string(r.Status), Message: r.Message, Observed: r.Observed, Truncated: r.Truncated,
		}
	}
	return out
}

func judgment(j job.Job, s job.Slot, results []check.Result, sessions []string, trigger string) ledger.Judgment {
	v, note := Decide(results, j.Evidence, len(sessions) > 0)
	return ledger.Judgment{
		JobID: j.ID, JobRevision: j.Revision, SlotKey: s.Key(), Slot: s.At, WindowEnd: s.End,
		Verdict: v, Checks: records(results), Sessions: sessions, Trigger: trigger, Note: note,
	}
}

// The slot at or before t, so a dry run judges the latest real slot
// A month back covers most schedules, a year back the rest
func latestSlot(j job.Job, t time.Time) (job.Slot, bool) {
	for _, back := range []time.Time{t.AddDate(0, -1, -1), t.AddDate(-1, 0, -1)} {
		slots, err := j.Slots(back, t.Add(time.Nanosecond))
		if err != nil {
			return job.Slot{}, false
		}
		if len(slots) > 0 {
			return slots[len(slots)-1], true
		}
	}
	return job.Slot{}, false
}

// SessionFolders lists the folders of every current job that expects a local session
// Only sessions in these folders are worth recording, so rindcut keeps no trace of the owner's other work
func SessionFolders(revs []job.Job) []string {
	var out []string
	for _, r := range job.Current(revs) {
		if r.Evidence == job.EvidenceSession && !r.Removed && r.Folder != "" {
			out = append(out, r.Folder)
		}
	}
	return out
}

func InFolders(cwd string, folders []string) bool {
	for _, f := range folders {
		if samePath(cwd, f) {
			return true
		}
	}
	return false
}
