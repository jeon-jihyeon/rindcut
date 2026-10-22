package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strconv"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeon-jihyeon/rindcut/internal/app"
	"github.com/jeon-jihyeon/rindcut/internal/export"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
	"github.com/jeon-jihyeon/rindcut/internal/mcp"
	"github.com/jeon-jihyeon/rindcut/internal/output"
)

// call is one command invocation with its flags still unparsed
type call struct {
	ctx  context.Context
	a    app.App
	e    env
	fs   *flag.FlagSet
	args []string
	show func(v any, t output.Table) error
}

var commands = map[string]func(c call) error{
	"tick":   tick,
	"check":  probe,
	"jobs":   jobs,
	"ledger": listLedger,
	"report": report,
	"export": exportNow,
	"install": func(c call) error {
		if err := c.a.Installer.Install(); err != nil {
			return err
		}
		fmt.Fprintln(c.e.stderr, "tick scheduled every 5 minutes")
		return nil
	},
	"uninstall": func(c call) error { return c.a.Installer.Uninstall() },
	"status":    status,
	"mcp":       func(c call) error { return mcp.New(c.a).Run(c.ctx, &sdk.StdioTransport{}) },
}

func dispatch(ctx context.Context, name string, args []string, a app.App, e env, asJSON bool) error {
	cmd, ok := commands[name]
	if !ok {
		return fmt.Errorf("unknown command %q: %w", name, errUsage)
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	return cmd(call{ctx: ctx, a: a, e: e, fs: fs, args: args, show: func(v any, t output.Table) error {
		if asJSON {
			return output.JSON(e.stdout, v)
		}
		return t.Write(e.stdout)
	}})
}

func parse(fs *flag.FlagSet, args []string, positional int) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != positional {
		return fmt.Errorf("want %d argument(s), got %d: %w", positional, fs.NArg(), errUsage)
	}
	return nil
}

func tick(c call) error {
	if err := parse(c.fs, c.args, 0); err != nil {
		return err
	}
	r, err := c.a.Tick(c.ctx)
	if err != nil {
		return err
	}
	t := output.Table{Header: []string{"SLOT", "VERDICT", "NOTE"}}
	if r.Busy {
		t.Rows = append(t.Rows, []string{"-", "busy", "another tick is running"})
	}
	for _, e := range r.Verdicts {
		var j ledger.Judgment
		if e.Decode(&j) == nil {
			t.Rows = append(t.Rows, []string{j.SlotKey, string(j.Verdict), j.Note})
		}
	}
	return c.show(r, t)
}

func status(c call) error {
	if err := parse(c.fs, c.args, 0); err != nil {
		return err
	}
	st, err := c.a.Status(c.ctx)
	if err != nil {
		return err
	}
	last := "never"
	if st.LastTick != nil {
		last = st.LastTick.Format(time.RFC3339)
	}
	t := output.Table{Header: []string{"VERSION", "HOME", "TICK SCHEDULED", "LAST TICK", "JOBS"}}
	t.Rows = append(t.Rows, []string{st.Version, st.Home, strconv.FormatBool(st.TickInstalled), last, strconv.Itoa(st.Jobs)})
	return c.show(st, t)
}

func probe(c call) error {
	at := c.fs.String("at", "", "judge the latest slot at or before this RFC 3339 time, default now")
	if err := parse(c.fs, c.args, 1); err != nil {
		return err
	}
	ctx, a, id := c.ctx, c.a, c.fs.Arg(0)
	when := a.Now()
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			return fmt.Errorf("--at: %w", err)
		}
		when = t
	}
	entries, err := a.Ledger().All(ctx)
	if err != nil {
		return err
	}
	j, ok := job.Current(app.Revisions(entries))[id]
	if !ok || j.Removed {
		return fmt.Errorf("%s: %w", id, app.ErrNoJob)
	}
	e, err := a.Judge().Probe(ctx, j, when, app.Fingerprint(j))
	if err != nil {
		return err
	}
	var p ledger.Judgment
	_ = e.Decode(&p)
	t := output.Table{Header: []string{"CHECK", "STATUS", "MESSAGE"}}
	for _, r := range p.Checks {
		t.Rows = append(t.Rows, []string{r.Kind, r.Status, r.Message})
	}
	t.Rows = append(t.Rows, []string{"verdict", string(p.Verdict), p.Note})
	return c.show(p, t)
}

func jobs(c call) error {
	all := c.fs.Bool("all", false, "include removed jobs")
	if err := parse(c.fs, c.args, 0); err != nil {
		return err
	}
	list, err := c.a.Jobs(c.ctx, *all)
	if err != nil {
		return err
	}
	t := output.Table{Header: []string{"ID", "NAME", "SCHEDULE", "REV", "LAST", "NEXT"}}
	for _, j := range list {
		last, next := "-", "-"
		if j.LastVerdict != nil {
			last = string(j.LastVerdict.Verdict)
		}
		if j.NextSlot != nil {
			next = j.NextSlot.Format("2006-01-02 15:04 MST")
		}
		if j.Removed {
			next = "removed"
		}
		t.Rows = append(t.Rows, []string{j.ID, j.Name, j.Schedule + " " + j.Timezone, strconv.Itoa(j.Revision), last, next})
	}
	return c.show(list, t)
}

func report(c call) error {
	since := c.fs.Duration("since", 24*time.Hour, "how far back")
	if err := parse(c.fs, c.args, 0); err != nil {
		return err
	}
	r, err := c.a.Report(c.ctx, *since)
	if err != nil {
		return err
	}
	t := output.Table{Header: []string{"SLOT", "VERDICT", "REASON"}}
	t.Rows = append(t.Rows, []string{"totals", fmt.Sprintf("pass %d, fail %d, missed %d, inconclusive %d",
		r.Totals.Pass, r.Totals.Fail, r.Totals.Missed, r.Totals.Inconclusive), ""})
	for _, p := range r.Problems {
		reason := p.Reason
		if p.LateStart != nil {
			reason += ", a session started late at " + p.LateStart.StartedAt.Format(time.RFC3339)
		}
		t.Rows = append(t.Rows, []string{p.SlotKey, string(p.Verdict), reason})
	}
	for _, id := range r.Silent {
		g := r.Unjudged[id]
		t.Rows = append(t.Rows, []string{id, "silent", fmt.Sprintf("%d closed slots with no verdict, %d past the lookback, is the tick running?",
			g.Overdue, g.Expired)})
	}
	for _, l := range r.Lag {
		if l.Pending > 0 || l.Dropped > 0 || l.LastError != "" {
			t.Rows = append(t.Rows, []string{"export " + l.Exporter, fmt.Sprintf("%d pending, %d dropped", l.Pending, l.Dropped), cmp.Or(l.LastError, l.LastDropped)})
		}
	}
	return c.show(r, t)
}

func listLedger(c call) error {
	jobID := c.fs.String("job", "", "only entries of this job")
	kind := c.fs.String("kind", "", "only this kind: job, session, probe, verdict or correction")
	since := c.fs.Duration("since", 0, "only entries written this recently")
	limit := c.fs.Int("limit", 50, "newest n entries, 0 for all")
	if err := parse(c.fs, c.args, 0); err != nil {
		return err
	}
	entries, err := c.a.Ledger().All(c.ctx)
	if err != nil {
		return err
	}
	var out []ledger.Entry
	for _, e := range entries {
		recent := *since == 0 || c.a.Now().Sub(e.At) <= *since
		if (*kind == "" || string(e.Kind) == *kind) && recent && (*jobID == "" || aboutJob(e, *jobID)) {
			out = append(out, e)
		}
	}
	if *limit > 0 && len(out) > *limit {
		out = out[len(out)-*limit:]
	}
	// The ledger's own JSONL, so it pipes into jq and back
	enc := json.NewEncoder(c.e.stdout)
	enc.SetEscapeHTML(false)
	for _, e := range out {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// aboutJob is true for the job's revisions and for its probes and verdicts
func aboutJob(e ledger.Entry, id string) bool {
	var ref struct {
		ID    string `json:"id"`
		JobID string `json:"job_id"`
	}
	if json.Unmarshal(e.Data, &ref) != nil {
		return false
	}
	return ref.JobID == id || (e.Kind == ledger.KindJob && ref.ID == id)
}

func exportNow(c call) error {
	exporter := c.fs.String("exporter", "", "only this exporter")
	fromStart := c.fs.Bool("from-start", false, "resend everything")
	if err := parse(c.fs, c.args, 0); err != nil {
		return err
	}
	lags, err := c.a.Export(c.ctx, *exporter, *fromStart)
	if err != nil {
		return err
	}
	return c.show(lags, lagTable(lags))
}

func lagTable(lags []export.Lag) output.Table {
	t := output.Table{Header: []string{"EXPORTER", "PENDING", "DROPPED", "ERROR"}}
	for _, l := range lags {
		t.Rows = append(t.Rows, []string{l.Exporter, strconv.Itoa(l.Pending), strconv.Itoa(l.Dropped), cmp.Or(l.LastError, l.LastDropped)})
	}
	return t
}
