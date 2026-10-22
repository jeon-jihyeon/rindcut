package check

import (
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
	// Each stream keeps this much of its start and as much of its end
	keptOutput = 2 << 10
	// Exit codes for a check that couldn't look
	exitTempFail      = 75 // EX_TEMPFAIL of sysexits.h, the owner's script saying try later
	exitNotExecutable = 126
	exitNotFound      = 127
)

// Runs through sh so the owner's own pipes and scripts work as typed
// The whole process group is killed on timeout, so a stuck child can't outlive the check
func runCommand(ctx context.Context, s Spec, env Env) Result {
	line := env.expand(s.Run)
	observed := map[string]any{"command": line}
	r := Result{Kind: KindCommand, Observed: observed}
	if changed := s.changedScripts(); len(changed) > 0 {
		observed["scripts"] = changed
		r.Status, r.Message = StatusError, fmt.Sprintf("a script of %q changed since the job was saved, preview and save the job again", line)
		return r
	}
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
	observed["stdout"], observed["stdout_bytes"] = stdout.String(), stdout.total
	observed["stderr"], observed["stderr_bytes"] = stderr.String(), stderr.total
	r.Truncated = stdout.cut() || stderr.cut()
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
	case code == exitTempFail:
		r.Status, r.Message = StatusError, fmt.Sprintf("%q exited %d, it couldn't look right now", line, code)
	case code == exitNotExecutable || code == exitNotFound:
		r.Status, r.Message = StatusError, fmt.Sprintf("the shell couldn't run %q, exit %d", line, code)
	case code != 0:
		r.Status, r.Message = StatusFail, fmt.Sprintf("%q exited %d", line, code)
	default:
		r.Status, r.Message = StatusPass, fmt.Sprintf("%q exited 0", line)
	}
	return r
}

// capped keeps the first and the last limit bytes of a stream and counts the rest
// A test runner prints the reason for a failure last, so the end matters as much as the start
type capped struct {
	head, tail []byte
	limit      int
	total      int
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	c.total += n
	if room := c.limit - len(c.head); room > 0 {
		take := min(room, len(p))
		c.head, p = append(c.head, p[:take]...), p[take:]
	}
	if len(p) >= c.limit {
		c.tail = append(c.tail[:0], p[len(p)-c.limit:]...)
		return n, nil
	}
	if drop := len(c.tail) + len(p) - c.limit; drop > 0 {
		c.tail = c.tail[:copy(c.tail, c.tail[drop:])]
	}
	c.tail = append(c.tail, p...)
	return n, nil
}

func (c *capped) cut() bool { return c.total > len(c.head)+len(c.tail) }

// Either cut may land inside a UTF-8 character, which is dropped so the ledger stays valid JSON text
func (c *capped) String() string {
	if !c.cut() {
		return strings.ToValidUTF8(string(c.head)+string(c.tail), "")
	}
	return strings.ToValidUTF8(string(c.head), "") +
		fmt.Sprintf("\n… %d bytes cut …\n", c.total-len(c.head)-len(c.tail)) +
		strings.ToValidUTF8(string(c.tail), "")
}
