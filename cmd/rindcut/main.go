// Command rindcut checks scheduled job outputs and records observations from connected agent sources
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jeon-jihyeon/rindcut/app"
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
  observe sync          collect source records and run due observations
  observe records       query observation records as JSONL, with a final page row
  observe status        show source coverage, gaps and unresolved observations
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
	a, err := commandApp(e, name, rest)
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
	return commandApp(e, "", nil)
}

func commandApp(e env, name string, args []string) (app.App, error) {
	a, err := app.New(e.getenv, e.now, version, e.stderr)
	if err != nil {
		return app.App{}, err
	}
	if name == "observe" && len(args) > 0 && (args[0] == "records" || args[0] == "status") {
		return a, nil
	}
	return a, a.Ready()
}
