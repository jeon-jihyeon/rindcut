// Package output prints command results as a table or as JSON
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

func JSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

type Table struct {
	Header []string
	Rows   [][]string
}

func (t Table) Write(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(t.Header) > 0 {
		fmt.Fprintln(tw, strings.Join(t.Header, "\t"))
	}
	for _, r := range t.Rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}
