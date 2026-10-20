package ledger

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

func encodeEntry(e Entry, data any) (Entry, []byte, error) {
	line, err := json.Marshal(e)
	if err != nil || len(line) < maxLine {
		return e, line, err
	}
	if e.Kind != KindVerdict && e.Kind != KindProbe {
		return e, line, nil
	}
	var j Judgment
	switch value := data.(type) {
	case Judgment:
		j = value
	case *Judgment:
		if value == nil {
			return e, line, nil
		}
		j = *value
	default:
		return e, line, nil
	}
	j.Checks = slices.Clone(j.Checks)
	for i := range j.Checks {
		j.Checks[i].Observed = maps.Clone(j.Checks[i].Observed)
	}
	for len(line) >= maxLine {
		if !j.trimOutput() && !j.trimMessage() {
			return e, nil, fmt.Errorf("%s mandatory fields exceed 64 KB: %w", e.Kind, ErrLineTooLong)
		}
		e.Data, err = json.Marshal(j)
		if err != nil {
			return e, nil, err
		}
		line, err = json.Marshal(e)
		if err != nil {
			return e, nil, err
		}
	}
	return e, line, nil
}

func (j *Judgment) trimOutput() bool {
	index, key, longest := -1, "", 0
	for i, c := range j.Checks {
		for _, k := range []string{"body", "stdout", "stderr"} {
			text, ok := c.Observed[k].(string)
			if ok && len(text) > longest {
				index, key, longest = i, k, len(text)
			}
		}
	}
	if index < 0 {
		return false
	}
	c := &j.Checks[index]
	c.Observed[key] = shortened(c.Observed[key].(string))
	c.Truncated = true
	return true
}

func (j *Judgment) trimMessage() bool {
	index, longest := -1, 0
	for i, c := range j.Checks {
		if len(c.Message) > longest {
			index, longest = i, len(c.Message)
		}
	}
	if index < 0 {
		return false
	}
	j.Checks[index].Message = shortened(j.Checks[index].Message)
	j.Checks[index].Truncated = true
	return true
}

func shortened(text string) string {
	return strings.ToValidUTF8(text[:len(text)/2], "")
}
