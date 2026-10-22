package export

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

// Nodloop hands a verdict to nodloop as the outcome of the run the matched session produced
// It talks to nodloop only through its CLI, so nodloop's code and scope stay untouched
type Nodloop struct {
	bin string
	// verdicts maps verdict entry ids to their judgment, so a correction finds its session
	verdicts map[string]ledger.Judgment
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

func NewNodloop(bin string, entries []ledger.Entry) *Nodloop {
	n := &Nodloop{bin: bin, verdicts: map[string]ledger.Judgment{}}
	for _, e := range entries {
		var j ledger.Judgment
		if e.Kind == ledger.KindVerdict && e.Decode(&j) == nil {
			n.verdicts[e.ID] = j
		}
	}
	return n
}

func (n *Nodloop) Name() string { return NameNodloop }

// nodloop keeps the latest outcome per run, so a resend restates it and a correction replaces it
func (n *Nodloop) Export(ctx context.Context, entries []ledger.Entry) error {
	for _, e := range entries {
		j, verdict, note, ok := n.target(e)
		if !ok {
			continue
		}
		result, known := nodloopResult[verdict]
		if !known || len(j.Sessions) == 0 {
			continue
		}
		run, err := n.run(ctx, j.Sessions[len(j.Sessions)-1])
		if err != nil {
			return err
		}
		if run == "" {
			continue
		}
		_, err = n.call(ctx, "feedback", "outcome", "--trace", run, "--result", result, "--reviewer", "rindcut", "--note", note)
		// nodloop ran and refused, like a run that failed its own check, so resending won't help
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			return PermanentError{Err: err}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// A missed verdict has no session, so missed only reaches a run through a correction
// That correction says the session wasn't the scheduled run, and inconclusive withdraws the outcome sent for it
var nodloopResult = map[ledger.Verdict]string{
	ledger.VerdictPass:         "confirmed",
	ledger.VerdictFail:         "refuted",
	ledger.VerdictInconclusive: "inconclusive",
	ledger.VerdictMissed:       "inconclusive",
}

func (n *Nodloop) target(e ledger.Entry) (ledger.Judgment, ledger.Verdict, string, bool) {
	switch e.Kind {
	case ledger.KindVerdict:
		j, ok := n.verdicts[e.ID]
		return j, j.Verdict, "rindcut " + j.SlotKey, ok
	case ledger.KindCorrection:
		var c ledger.Correction
		if e.Decode(&c) != nil {
			return ledger.Judgment{}, "", "", false
		}
		j, ok := n.verdicts[c.VerdictID]
		return j, c.Verdict, "rindcut correction of " + j.SlotKey + ": " + c.Reason, ok
	}
	return ledger.Judgment{}, "", "", false
}

// Test asks the binary for its version, which proves rindcut can run it
func (n *Nodloop) Test(ctx context.Context) error {
	_, err := n.call(ctx, "version")
	return err
}

// The newest nodloop run of the session, from `nodloop trace list`, which prints id, time, name, subject and cost
func (n *Nodloop) run(ctx context.Context, session string) (string, error) {
	out, err := n.call(ctx, "trace", "list", "--session", session)
	if err != nil {
		return "", err
	}
	var newest, at string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		cols := strings.Split(sc.Text(), "\t")
		if len(cols) >= 3 && cols[2] == "run" && cols[1] >= at {
			newest, at = cols[0], cols[1]
		}
	}
	return newest, nil
}

func (n *Nodloop) call(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, n.bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nodloop %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
