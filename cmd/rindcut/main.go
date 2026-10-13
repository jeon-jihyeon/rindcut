// Command rindcut checks that scheduled AI agent jobs really ran and really produced what they promised
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/app"
	"github.com/jeon-jihyeon/rindcut/internal/home"
	"github.com/jeon-jihyeon/rindcut/internal/judge"
	"github.com/jeon-jihyeon/rindcut/internal/output"
)

// Set by goreleaser through -X main.version
var version = "dev"

const usageText = `rindcut checks that scheduled AI agent jobs really ran and produced what they promised

usage: rindcut [--json] COMMAND [flags]

commands:
  tick                  judge closed slots and run the exporters, what the scheduler calls every 5 minutes
  check JOB [--at T]    dry run of a job's latest slot, recorded as a probe
  jobs [--all]          list jobs
  ledger [flags]        list ledger entries
  report [--since D]    summary of verdicts, problems, silent jobs and export lag
  export [flags]        send new entries to the exporters
  install | uninstall   schedule or unschedule the tick
  status                version, home and tick state
  mcp                   serve the MCP tools over stdio, for the Claude Code plugin
  hook session-end      record a session from the SessionEnd hook input
  version               print the version

Jobs are registered by talking to Claude through the rindcut plugin.

exit codes: 0 done, 1 the command failed, 2 usage error
`

var errUsage = errors.New("usage")

type env struct {
	getenv func(string) string
	now    func() time.Time
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func main() {
	os.Exit(run(os.Args[1:], env{getenv: os.Getenv, now: time.Now, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}))
}

func run(args []string, e env) int {
	asJSON := false
	if len(args) > 0 && args[0] == "--json" {
		asJSON, args = true, args[1:]
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(e.stderr, usageText)
		return 2
	}
	name, rest := args[0], args[1:]
	if name == "version" {
		fmt.Fprintln(e.stdout, version)
		return 0
	}
	// The hook must never break the session it observes, so it always exits 0
	if name == "hook" {
		if err := runHook(rest, e); err != nil {
			fmt.Fprintln(e.stderr, "rindcut hook:", err)
		}
		return 0
	}
	a, err := newApp(e)
	if err == nil {
		err = dispatch(context.Background(), name, rest, a, e, asJSON)
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage), errors.Is(err, flag.ErrHelp):
		fmt.Fprintf(e.stderr, "rindcut %s: %v\n", name, err)
		return 2
	default:
		if asJSON {
			_ = output.JSON(e.stdout, map[string]string{"error": err.Error()})
		}
		fmt.Fprintf(e.stderr, "rindcut %s: %v\n", name, err)
		return 1
	}
}

func newApp(e env) (app.App, error) {
	h, err := home.Resolve(e.getenv)
	if err != nil {
		return app.App{}, err
	}
	userHome := e.getenv("HOME")
	a := app.App{
		Home: h, UserHome: userHome, Now: e.now, Version: version, Warn: e.stderr, SearchPath: e.getenv("PATH"),
		Installer: judge.Installer{
			Binary: stableBinary(h), Home: string(h), UserHome: userHome, GOOS: runtime.GOOS, UID: os.Getuid(),
			Exec: func(name string, args ...string) error {
				out, err := exec.Command(name, args...).CombinedOutput()
				if err != nil {
					return fmt.Errorf("%s: %w: %s", name, err, out)
				}
				return nil
			},
		},
	}
	return a, a.Ready()
}

// The scheduler calls the path the plugin launcher keeps pointed at the current binary
// Without the plugin, it's this binary
func stableBinary(h home.Dir) string {
	stable := h.Path("bin", "rindcut")
	if _, err := os.Stat(stable); err == nil {
		return stable
	}
	if exe, err := os.Executable(); err == nil {
		if abs, err := filepath.EvalSymlinks(exe); err == nil {
			return abs
		}
		return exe
	}
	return stable
}
