package judge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/check"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

// Slots older than this get no verdict, so a laptop off for weeks doesn't wake to a flood
const Lookback = 7 * 24 * time.Hour

const (
	TriggerTick  = "tick"
	TriggerCheck = "check"
)

// Judge reads the ledger, judges closed slots and appends verdicts
// Callers run one Tick at a time, since two would judge the same slot twice
type Judge struct {
	Ledger *ledger.Ledger
	Now    func() time.Time
	// UserHome expands ~ in check paths and holds .claude/projects for the backfill
	UserHome string
	// Dir is where commands of jobs without a folder run
	Dir string
}

type TickResult struct {
	Verdicts   []ledger.Entry `json:"verdicts"`
	Backfilled int            `json:"backfilled"`
}

// state is the ledger read once per tick
type state struct {
	revs     []job.Job
	sessions map[string]ledger.Session
	judged   map[string]bool
}

func (j Judge) Tick(ctx context.Context) (TickResult, error) {
	st, err := j.load(ctx)
	if err != nil {
		return TickResult{}, err
	}
	due := st.due(j.Now())
	var out TickResult
	backfilled := false
	for _, s := range due {
		r, ok := job.At(st.revs, s.JobID, s.At)
		if !ok || r.Removed {
			continue
		}
		matched := st.matching(r, s)
		// One transcript sweep per tick, from the oldest due slot, covers every slot after it
		if len(matched) == 0 && r.Evidence == job.EvidenceSession && !backfilled {
			n, err := j.backfill(ctx, st, due[0].At)
			if err != nil {
				return out, err
			}
			out.Backfilled, backfilled = n, true
			matched = st.matching(r, s)
		}
		p := judgment(r, s, j.run(ctx, r, s), matched, TriggerTick)
		if len(matched) > 0 {
			p.RunID = st.sessions[matched[len(matched)-1]].RunID
		}
		e, err := j.Ledger.Append(ctx, ledger.KindVerdict, p)
		if err != nil {
			return out, err
		}
		out.Verdicts = append(out.Verdicts, e)
	}
	return out, nil
}

// Probe judges the latest slot at or before at without counting it
// It's how the owner sees what rindcut would conclude before a job is saved
func (j Judge) Probe(ctx context.Context, r job.Job, at time.Time, fingerprint string) (ledger.Entry, error) {
	if err := r.Validate(); err != nil {
		return ledger.Entry{}, err
	}
	s, ok := latestSlot(r, at)
	if !ok {
		return ledger.Entry{}, fmt.Errorf("job %s has no slot in the year before %s", r.ID, at.Format(time.RFC3339))
	}
	st, err := j.load(ctx)
	if err != nil {
		return ledger.Entry{}, err
	}
	p := judgment(r, s, j.run(ctx, r, s), st.matching(r, s), TriggerCheck)
	if len(p.Sessions) > 0 {
		p.RunID = st.sessions[p.Sessions[len(p.Sessions)-1]].RunID
	}
	p.Fingerprint = fingerprint
	return j.Ledger.Append(ctx, ledger.KindProbe, p)
}

func (j Judge) run(ctx context.Context, r job.Job, s job.Slot) []check.Result {
	env := j.env(r)
	env.Slot = s.At
	if next, ok := r.Next(s.At); ok {
		env.Next = next.At
	}
	out := make([]check.Result, len(r.Checks))
	for i, c := range r.Checks {
		out[i] = check.Run(ctx, c, env)
	}
	return out
}

// Pin pins the job's command checks to their script files, resolved the way the tick runs them
func (j Judge) Pin(r job.Job) job.Job {
	env := j.env(r)
	checks := make([]check.Spec, len(r.Checks))
	for i, c := range r.Checks {
		checks[i] = c.Pinned(env)
	}
	r.Checks = checks
	return r
}

func (j Judge) env(r job.Job) check.Env {
	return check.Env{JobID: r.ID, Folder: r.Folder, UserHome: j.UserHome, Dir: j.Dir}
}

func (j Judge) load(ctx context.Context) (state, error) {
	st := state{sessions: map[string]ledger.Session{}, judged: map[string]bool{}}
	err := j.Ledger.Scan(ctx, func(e ledger.Entry) error {
		switch e.Kind {
		case ledger.KindJob:
			var r job.Job
			if e.Decode(&r) == nil {
				r.Since = e.At
				st.revs = append(st.revs, r)
			}
		case ledger.KindSession:
			var s ledger.Session
			if e.Decode(&s) == nil && s.SessionID != "" {
				st.remember(s)
			}
		case ledger.KindVerdict:
			var v ledger.Judgment
			if e.Decode(&v) == nil {
				st.judged[v.SlotKey] = true
			}
		}
		return nil
	})
	return st, err
}

// due lists every closed, unjudged slot in the lookback, oldest first
func (st state) due(now time.Time) []job.Slot {
	var out []job.Slot
	for _, s := range Scheduled(st.revs, now.Add(-Lookback), now) {
		if !s.End.After(now) && !st.judged[s.Key()] {
			out = append(out, s)
		}
	}
	return out
}

// Scheduled lists the slots in [from, to) of every job, oldest first
// Each revision covers only the time it was in force, so a schedule change never invents old slots
func Scheduled(revs []job.Job, from, to time.Time) []job.Slot {
	byID := map[string][]job.Job{}
	for _, r := range revs {
		byID[r.ID] = append(byID[r.ID], r)
	}
	var out []job.Slot
	for _, revs := range byID {
		sort.Slice(revs, func(a, b int) bool { return revs[a].Revision < revs[b].Revision })
		for i, r := range revs {
			if r.Removed {
				continue
			}
			start, end := laterOf(from, r.Since), to
			if i+1 < len(revs) && revs[i+1].Since.Before(end) {
				end = revs[i+1].Since
			}
			slots, err := r.Slots(start, end)
			if err == nil {
				out = append(out, slots...)
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].At.Before(out[b].At) })
	return out
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// remember keeps one record per session
// A complete record replaces an incomplete one and keeps the run id the incomplete one carried
// Otherwise the first record stays, and when it has no run id it takes one from a later record with the same folder, start and first prompt
func (st state) remember(s ledger.Session) {
	old, seen := st.sessions[s.SessionID]
	if !seen {
		st.sessions[s.SessionID] = s
		return
	}
	if !old.Complete() && s.Complete() {
		if old.RunID != "" {
			s.RunID = old.RunID
		}
		st.sessions[s.SessionID] = s
		return
	}
	if old.RunID == "" && s.RunID != "" && old.Cwd == s.Cwd &&
		old.StartedAt.Equal(s.StartedAt) && old.FirstPrompt == s.FirstPrompt {
		old.RunID = s.RunID
		st.sessions[s.SessionID] = old
	}
}

func (st state) matching(r job.Job, s job.Slot) []string {
	var out []string
	for id, sess := range st.sessions {
		if Matches(r, s, sess) {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		return st.sessions[out[a]].StartedAt.Before(st.sessions[out[b]].StartedAt)
	})
	return out
}

// backfill records sessions the hook missed or caught before its transcript was readable,
// because SessionEnd doesn't fire when a session is killed
// It reads transcripts written since the slot, and keeps only sessions in a session job's folder
func (j Judge) backfill(ctx context.Context, st state, since time.Time) (int, error) {
	paths, _ := filepath.Glob(filepath.Join(j.UserHome, ".claude", "projects", "*", "*.jsonl"))
	folders := SessionFolders(st.revs)
	added := 0
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || info.ModTime().Before(since) {
			continue
		}
		t, err := ReadTranscript(p)
		if err != nil {
			continue
		}
		s := ledger.Session{
			SessionID: t.SessionID, Cwd: t.Cwd, StartedAt: t.StartedAt, FirstPrompt: t.FirstPrompt,
			TranscriptPath: p, EndedAt: info.ModTime().UTC(), Source: ledger.SourceBackfill,
		}
		if !s.Complete() || !InFolders(s.Cwd, folders) {
			continue
		}
		if known, ok := st.sessions[s.SessionID]; ok && known.Complete() {
			continue
		}
		if _, err := j.Ledger.Append(ctx, ledger.KindSession, s); err != nil {
			return added, err
		}
		st.remember(s)
		added++
	}
	return added, nil
}
