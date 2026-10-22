package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

// Nodloop hands a verdict to nodloop as the outcome of the run the matched session produced
// It talks to nodloop only through its CLI, so nodloop's code and scope stay untouched
type Nodloop struct {
	bin string
	// path is the bindings file, kept apart from the delivery cursor so a reset never rebinds a verdict
	path string
	// verdicts maps verdict entry ids to their judgment, so a correction finds its session
	verdicts map[string]ledger.Judgment
	// bindings maps verdict entry ids to the run their first outcome went to, loaded on the first export
	bindings map[string]runBinding
}

// runBinding is the first run a verdict was sent to, with the ledger identity it was bound under
// Corrections and retries reuse it, and a changed identity is an error instead of a rebind
type runBinding struct {
	JobID    string `json:"job_id"`
	Revision int    `json:"revision"`
	SlotKey  string `json:"slot_key"`
	Session  string `json:"session"`
	RunID    string `json:"run_id"`
}

type NodloopSettings struct {
	Enabled bool `json:"enabled"`
}

// FindNodloop is the binary nodloop's plugin links, else a nodloop in searchPath, else empty
// The plugin link comes first because launchd gives the tick a bare PATH
func FindNodloop(userHome, searchPath string) string {
	dirs := append([]string{filepath.Join(userHome, ".nodloop", "bin")}, filepath.SplitList(searchPath)...)
	for _, d := range dirs {
		p := filepath.Join(d, "nodloop")
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 && filepath.IsAbs(p) {
			return p
		}
	}
	return ""
}

func NewNodloop(bin, path string, entries []ledger.Entry) *Nodloop {
	n := &Nodloop{bin: bin, path: path, verdicts: map[string]ledger.Judgment{}}
	for _, e := range entries {
		var j ledger.Judgment
		if e.Kind == ledger.KindVerdict && e.Decode(&j) == nil {
			n.verdicts[e.ID] = j
		}
	}
	return n
}

func (n *Nodloop) Name() string { return NameNodloop }

// nodloop keeps the latest outcome per run, so a resend or a correction just restates it
func (n *Nodloop) Export(ctx context.Context, entries []ledger.Entry) error {
	if err := n.load(); err != nil {
		return err
	}
	for _, e := range entries {
		id, verdict, note, ok := n.target(e)
		if !ok {
			continue
		}
		j := n.verdicts[id]
		result, known := nodloopResult[verdict]
		if !known || len(j.Sessions) == 0 {
			continue
		}
		binding := runBinding{JobID: j.JobID, Revision: j.JobRevision, SlotKey: j.SlotKey,
			Session: j.Sessions[len(j.Sessions)-1], RunID: j.RunID}
		run, err := n.bound(ctx, id, binding, j.Slot, j.WindowEnd)
		if err != nil {
			return err
		}
		// A non-zero exit can't tell a refusal from a storage failure in nodloop, so the entry stays pending and is retried
		_, err = n.call(ctx, "feedback", "outcome", "--trace", run, "--result", result,
			"--actor", "code", "--reviewer", "rindcut", "--note", note)
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			return fmt.Errorf("%w: %w", ErrDeferred, err)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// A missed verdict has no run to cite, so it is skipped for having no session
// A correction to missed says the matched session wasn't the scheduled run, so it sends inconclusive to the bound run
var nodloopResult = map[ledger.Verdict]string{
	ledger.VerdictPass:         "confirmed",
	ledger.VerdictFail:         "refuted",
	ledger.VerdictInconclusive: "inconclusive",
	ledger.VerdictMissed:       "inconclusive",
}

func (n *Nodloop) target(e ledger.Entry) (string, ledger.Verdict, string, bool) {
	switch e.Kind {
	case ledger.KindVerdict:
		j, ok := n.verdicts[e.ID]
		return e.ID, j.Verdict, "rindcut " + j.SlotKey, ok
	case ledger.KindCorrection:
		var c ledger.Correction
		if e.Decode(&c) != nil {
			return "", "", "", false
		}
		j, ok := n.verdicts[c.VerdictID]
		return c.VerdictID, c.Verdict, "rindcut correction of " + j.SlotKey + ": " + c.Reason, ok
	}
	return "", "", "", false
}

// Test asks the binary for its version, which proves rindcut can run it
func (n *Nodloop) Test(ctx context.Context) error {
	_, err := n.call(ctx, "version")
	return err
}

// run finds the nodloop run of the session a verdict is about
// An explicit run id must be listed in the session, at any time
// Without one, exactly one run completed between the slot time and the window end binds
// No run or several runs is ErrDeferred instead of a guess
func (n *Nodloop) run(ctx context.Context, session, explicit string, from, until time.Time) (string, error) {
	out, err := n.call(ctx, "trace", "list", "--session", session, "--json")
	if err != nil {
		return "", fmt.Errorf("trace list --json failed: check binary compatibility and record access: %w", err)
	}
	runs := map[string]struct{}{}
	var candidate string
	var rows []struct {
		ID        string    `json:"id"`
		Time      time.Time `json:"time"`
		Name      string    `json:"name"`
		SessionID string    `json:"session_id"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return "", errors.New("nodloop trace list: invalid machine JSON")
	}
	if rows == nil {
		return "", fmt.Errorf("nodloop trace list: expected an array")
	}
	for _, row := range rows {
		if row.Name != "run" || row.SessionID != session {
			continue
		}
		if row.ID == "" || row.Time.IsZero() {
			return "", fmt.Errorf("nodloop trace list: invalid run identity or time")
		}
		if explicit != "" {
			if row.ID == explicit {
				return explicit, nil
			}
			continue
		}
		if !row.Time.Before(from) && !row.Time.After(until) {
			runs[row.ID] = struct{}{}
			candidate = row.ID
		}
	}
	if len(runs) != 1 {
		return "", ErrDeferred
	}
	return candidate, nil
}

// load reads the bindings once per exporter, and a broken file stops the export instead of rebinding
func (n *Nodloop) load() error {
	if n.bindings != nil {
		return nil
	}
	b, err := os.ReadFile(n.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("nodloop bindings: %w", err)
	}
	n.bindings = map[string]runBinding{}
	if err == nil {
		if err := json.Unmarshal(b, &n.bindings); err != nil {
			n.bindings = nil
			return fmt.Errorf("nodloop bindings: %w", err)
		}
		if n.bindings == nil {
			return fmt.Errorf("nodloop bindings: expected an object")
		}
	}
	return nil
}

// bound returns the saved run of a verdict, or finds one and saves it before any outcome is sent
func (n *Nodloop) bound(ctx context.Context, id string, b runBinding, from, until time.Time) (string, error) {
	if saved, ok := n.bindings[id]; ok {
		explicit := b.RunID
		b.RunID = saved.RunID
		if saved != b || saved.RunID == "" || (explicit != "" && saved.RunID != explicit) {
			return "", fmt.Errorf("nodloop binding identity changed")
		}
		return saved.RunID, nil
	}
	run, err := n.run(ctx, b.Session, b.RunID, from, until)
	if err != nil {
		return "", err
	}
	b.RunID = run
	n.bindings[id] = b
	if err := n.save(); err != nil {
		delete(n.bindings, id)
		return "", err
	}
	return run, nil
}

func (n *Nodloop) save() error {
	if err := os.MkdirAll(filepath.Dir(n.path), 0o700); err != nil {
		return fmt.Errorf("nodloop bindings: %w", err)
	}
	b, err := json.Marshal(n.bindings)
	if err != nil {
		return fmt.Errorf("nodloop bindings: %w", err)
	}
	tmp := n.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("nodloop bindings: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Rename(tmp, n.path); err != nil {
		return fmt.Errorf("nodloop bindings: %w", err)
	}
	return nil
}

func (n *Nodloop) call(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, n.bin, args...)
	// stderr may echo recorded text, which must not reach cursors or export errors
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nodloop %s: %w", args[0], err)
	}
	return out, nil
}
