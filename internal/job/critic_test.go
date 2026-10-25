package job_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/check"
	"github.com/jeon-jihyeon/rindcut/internal/job"
)

func TestCriticValidation(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		edit func(*job.Critic)
	}{
		{name: "valid", edit: func(*job.Critic) {}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j := valid()
			j.Critic = &job.Critic{Source: "acme", SourceRevision: "adapter-v1", CriteriaRevision: "C1",
				Criteria: []job.Criterion{{ID: "accuracy", Text: "Every number has a source"}},
				Command:  check.Spec{Kind: check.KindCommand, Run: "true"}}
			tc.edit(j.Critic)
			assert.NoError(t, j.Validate())
		})
	}
}

func TestCriticValidationErrors(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		edit func(*job.Critic)
		bad  string
	}{
		{name: "missing source", edit: func(c *job.Critic) { c.Source = "" }, bad: "source"},
		{name: "unsafe source", edit: func(c *job.Critic) { c.Source = "acme\nann" }, bad: "source"},
		{name: "long source", edit: func(c *job.Critic) { c.Source = strings.Repeat("a", 129) }, bad: "source"},
		{name: "missing source revision", edit: func(c *job.Critic) { c.SourceRevision = "" }, bad: "source_revision"},
		{name: "missing criteria revision", edit: func(c *job.Critic) { c.CriteriaRevision = "" }, bad: "criteria_revision"},
		{name: "no criteria", edit: func(c *job.Critic) { c.Criteria = nil }, bad: "criteria"},
		{name: "too many criteria", edit: func(c *job.Critic) { c.Criteria = make([]job.Criterion, 11) }, bad: "criteria"},
		{name: "duplicate id", edit: func(c *job.Critic) { c.Criteria = append(c.Criteria, c.Criteria[0]) }, bad: "unique"},
		{name: "invalid id", edit: func(c *job.Critic) { c.Criteria[0].ID = "a b" }, bad: "id"},
		{name: "long id", edit: func(c *job.Critic) { c.Criteria[0].ID = strings.Repeat("a", 49) }, bad: "id"},
		{name: "empty text", edit: func(c *job.Critic) { c.Criteria[0].Text = " " }, bad: "text"},
		{name: "long text", edit: func(c *job.Critic) { c.Criteria[0].Text = strings.Repeat("a", 501) }, bad: "text"},
		{name: "wrong command kind", edit: func(c *job.Critic) { c.Command.Kind = check.KindFile }, bad: "command"},
		{name: "missing command", edit: func(c *job.Critic) { c.Command.Run = " " }, bad: "run"},
		{name: "long timeout", edit: func(c *job.Critic) { c.Command.Timeout = check.Duration(11 * time.Minute) },
			bad: "timeout"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j := valid()
			j.Critic = &job.Critic{Source: "acme", SourceRevision: "adapter-v1", CriteriaRevision: "C1",
				Criteria: []job.Criterion{{ID: "accuracy", Text: "Every number has a source"}},
				Command:  check.Spec{Kind: check.KindCommand, Run: "true"}}
			tc.edit(j.Critic)
			err := j.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.bad)
		})
	}
}
