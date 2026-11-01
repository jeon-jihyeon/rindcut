package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestObserveReadOnlyAbsentStore(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		args []string
		want string
	}{
		{name: "JSONL page", args: []string{"observe", "records"},
			want: `{"kind":"page","next_cursor":null,"has_more":false,"store_present":false}`},
		{name: "JSON page", args: []string{"--json", "observe", "records"},
			want: `{"records":[],"next_cursor":null,"has_more":false,"store_present":false}`},
		{name: "status", args: []string{"--json", "observe", "status"},
			want: `{"store_present":false,"sources":[]}`},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHome(t)
			r := cli(t, h, "", tc.args...)
			assert.Equal(t, 0, r.code, r.stderr)
			assert.JSONEq(t, tc.want, r.stdout)
			assert.Empty(t, r.stderr)
			assert.NoDirExists(t, h)
		})
	}
}

func TestObserveUsage(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		args []string
	}{
		{name: "missing subcommand", args: []string{"observe"}},
		{name: "unknown subcommand", args: []string{"observe", "acme"}},
		{name: "extra argument", args: []string{"observe", "records", "acme"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := cli(t, newHome(t), "", tc.args...)
			assert.Equal(t, 2, r.code)
			assert.Contains(t, r.stderr, "usage")
		})
	}
}
