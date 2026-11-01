package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"strconv"

	"github.com/jeon-jihyeon/rindcut/app"
	"github.com/jeon-jihyeon/rindcut/internal/output"
)

func observe(c call) error {
	if len(c.args) == 0 {
		return fmt.Errorf("want observe sync, records or status: %w", errUsage)
	}
	name := c.args[0]
	c.args = c.args[1:]
	c.fs = flag.NewFlagSet("observe "+name, flag.ContinueOnError)
	c.fs.SetOutput(c.e.stderr)
	switch name {
	case "sync":
		return observeSync(c)
	case "records":
		return observeRecords(c)
	case "status":
		return observeStatus(c)
	default:
		return fmt.Errorf("unknown observe command %q: %w", name, errUsage)
	}
}

func observeSync(c call) error {
	if err := parse(c.fs, c.args, 0); err != nil {
		return err
	}
	r, err := c.a.ObserveSync(c.ctx)
	if err != nil {
		return err
	}
	t := output.Table{Header: []string{"CYCLE", "ATTEMPTS", "CAPTURED", "OBSERVATIONS", "INCOMPLETE"}}
	t.Rows = append(t.Rows, []string{r.ID, strconv.Itoa(r.Attempts), strconv.Itoa(r.Captured),
		strconv.Itoa(r.Observations), strconv.FormatBool(r.Incomplete)})
	return c.show(r, t)
}

func observeRecords(c call) error {
	q := app.ObserveQuery{}
	c.fs.StringVar(&q.SourceID, "source-id", "", "only this registered source")
	c.fs.StringVar(&q.Type, "type", "", "only this event type")
	c.fs.StringVar(&q.ActionID, "action-id", "", "only this logical action")
	c.fs.StringVar(&q.Kind, "kind", "", "event, observation or cycle")
	c.fs.StringVar(&q.Cursor, "cursor", "", "opaque cursor from the previous page")
	c.fs.IntVar(&q.Limit, "limit", 100, "records per page, from 1 to 100")
	store := c.fs.String("target-store", "", "target store identity")
	capture := c.fs.String("target-capture", "", "target capture identity")
	if err := parse(c.fs, c.args, 0); err != nil {
		return err
	}
	if *store != "" || *capture != "" {
		q.TargetRef = &app.ObserveTarget{StoreID: *store, CaptureID: *capture}
	}
	page, err := c.a.ObserveRecords(c.ctx, q)
	if err != nil {
		return err
	}
	if c.asJSON {
		return c.show(page, output.Table{})
	}
	enc := json.NewEncoder(c.e.stdout)
	enc.SetEscapeHTML(false)
	for _, record := range page.Records {
		if err := enc.Encode(record); err != nil {
			return err
		}
	}
	return enc.Encode(struct {
		Kind         string  `json:"kind"`
		NextCursor   *string `json:"next_cursor"`
		HasMore      bool    `json:"has_more"`
		StorePresent bool    `json:"store_present"`
	}{Kind: "page", NextCursor: page.NextCursor, HasMore: page.HasMore, StorePresent: page.StorePresent})
}

func observeStatus(c call) error {
	if err := parse(c.fs, c.args, 0); err != nil {
		return err
	}
	r, err := c.a.ObserveStatus(c.ctx)
	if err != nil {
		return err
	}
	t := output.Table{Header: []string{"SOURCE", "REV", "CAPABILITY", "GAP", "CONFLICTS", "PENDING", "OVERDUE", "REASON"}}
	for _, s := range r.Sources {
		t.Rows = append(t.Rows, []string{s.Source.ID, strconv.FormatInt(s.Source.Revision, 10), s.Source.Capability.Kind,
			strconv.FormatBool(s.Gap), strconv.Itoa(s.Conflicts), strconv.Itoa(s.Pending), strconv.Itoa(s.Overdue), s.LastReasonCode})
	}
	return c.show(r, t)
}
