package job

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jeon-jihyeon/rindcut/internal/check"
)

type Criterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Critic struct {
	Source           string      `json:"source"`
	SourceRevision   string      `json:"source_revision"`
	CriteriaRevision string      `json:"criteria_revision"`
	Criteria         []Criterion `json:"criteria"`
	Command          check.Spec  `json:"command"`
	RetainReason     bool        `json:"retain_reason,omitempty"`
}

var criticLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func (c Critic) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"source", c.Source}, {"source_revision", c.SourceRevision}, {"criteria_revision", c.CriteriaRevision},
	} {
		if !criticLabel.MatchString(field.value) {
			return fmt.Errorf("critic %s must be a stable ASCII label of 1 to 128 characters", field.name)
		}
	}
	if len(c.Criteria) < 1 || len(c.Criteria) > maxChecks {
		return fmt.Errorf("critic needs 1 to %d criteria", maxChecks)
	}
	seen := map[string]bool{}
	for _, criterion := range c.Criteria {
		if len(criterion.ID) > maxIDLen || !criticLabel.MatchString(criterion.ID) {
			return errors.New("critic criterion id must be a stable ASCII label of 1 to 48 characters")
		}
		if seen[criterion.ID] {
			return errors.New("critic criterion ids must be unique")
		}
		seen[criterion.ID] = true
		if strings.TrimSpace(criterion.Text) == "" || len(criterion.Text) > 500 {
			return errors.New("critic criterion text must be 1 to 500 bytes with non-whitespace content")
		}
	}
	if c.Command.Kind != check.KindCommand {
		return errors.New("critic command must be a command check")
	}
	if err := c.Command.Validate(); err != nil {
		return fmt.Errorf("critic: %w", err)
	}
	return nil
}
