package check

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	keptOutput = 4 << 10
	// Exit codes the shell uses for a command it couldn't run at all
	exitNotExecutable = 126
	exitNotFound      = 127
)

// Runs through sh so the owner's own pipes and scripts work as typed
// The whole process group is killed on timeout, so a stuck child can't outlive the check
func runCommand(ctx context.Context, s Spec, env Env) Result {
	line := env.expand(s.Run)
	observed := map[string]any{"command": line}
	r := Result{Kind: KindCommand, Observed: observed}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", line)
	cmd.Dir = env.dir()
	cmd.Env = append(os.Environ(),
		"RINDCUT_JOB="+env.JobID,
		"RINDCUT_SLOT="+env.Slot.Format(time.RFC3339),
		"RINDCUT_DATE="+env.Slot.Format(time.DateOnly))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	stdout, stderr := &capped{limit: keptOutput}, &capped{limit: keptOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	start := time.Now()
	err := cmd.Run()
	observed["elapsed_ms"] = time.Since(start).Milliseconds()
	observed["stdout"] = stdout.String()
	observed["stderr"] = stderr.String()
	r.Truncated = stdout.cut || stderr.cut
	// ExitCode is -1 when the process never started or was killed by a signal
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	observed["exit_code"] = code
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil:
		r.Status, r.Message = StatusError, fmt.Sprintf("%q didn't finish within %s", line, s.timeout())
	case err != nil && !errors.As(err, &exitErr):
		r.Status, r.Message = StatusError, fmt.Sprintf("couldn't run %q: %v", line, err)
	case code == exitNotExecutable || code == exitNotFound:
		r.Status, r.Message = StatusError, fmt.Sprintf("the shell couldn't run %q, exit %d", line, code)
	case code != 0:
		r.Status, r.Message = StatusFail, fmt.Sprintf("%q exited %d", line, code)
	default:
		r.Status, r.Message = StatusPass, fmt.Sprintf("%q exited 0", line)
	}
	return r
}

// capped keeps the first limit bytes and drops the rest, so a chatty command can't bloat the ledger
type capped struct {
	buf   bytes.Buffer
	limit int
	cut   bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room < len(p) {
		c.cut = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// The cut may land inside a UTF-8 character, which is dropped so the ledger stays valid JSON text
func (c *capped) String() string {
	return strings.ToValidUTF8(c.buf.String(), "")
}
