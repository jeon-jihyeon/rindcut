package command

import (
	"encoding/json"
	"regexp"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ascii     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)
	refID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}$`)
	digest    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	timestamp = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$`)
)

func identifier(value string) bool {
	if len(value) == 0 || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func identifiers(values []string) bool {
	for _, value := range values {
		if !identifier(value) {
			return false
		}
	}
	return true
}

func attributes(values map[string]any) bool {
	if values == nil || len(values) > 32 {
		return false
	}
	for key, value := range values {
		if !ascii.MatchString(key) {
			return false
		}
		if !attribute(value) {
			return false
		}
	}
	return true
}

func attribute(value any) bool {
	switch v := value.(type) {
	case string:
		return len(v) <= 2048 && utf8.ValidString(v)
	case bool:
		return true
	case json.Number:
		i, err := strconv.ParseInt(v.String(), 10, 64)
		return integer.MatchString(v.String()) && err == nil && i >= -9007199254740991 && i <= 9007199254740991
	case int:
		return int64(v) >= -9007199254740991 && int64(v) <= 9007199254740991
	case int64:
		return v >= -9007199254740991 && v <= 9007199254740991
	default:
		return false
	}
}

func timeValue(value string) (time.Time, bool) {
	if !timestamp.MatchString(value) {
		return time.Time{}, false
	}
	if value[len(value)-1] != 'Z' {
		zone := value[len(value)-5:]
		hour, err := strconv.Atoi(zone[:2])
		if err != nil || hour > 23 {
			return time.Time{}, false
		}
		minute, err := strconv.Atoi(zone[3:])
		if err != nil || minute > 59 {
			return time.Time{}, false
		}
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	return t, err == nil
}

func validEvidence(values []Evidence) bool {
	if values == nil || len(values) > 16 {
		return false
	}
	for _, value := range values {
		if !identifier(value.Kind) || !refID.MatchString(value.Ref.Namespace) || !refID.MatchString(value.Ref.ID) {
			return false
		}
		if value.Digest != nil && !digest.MatchString(*value.Digest) {
			return false
		}
	}
	return true
}

func validEvent(e Event) bool {
	if !identifiers([]string{e.SourceInstance, e.Generation, e.Stream, e.ID, e.Type,
		e.SchemaVersion, e.AdapterRevision, e.ProjectionProfile}) {
		return false
	}
	for _, value := range []*string{e.ProducerVersion, e.ActionID, e.AttemptID} {
		if value != nil && !identifier(*value) {
			return false
		}
	}
	if e.OccurredAt != nil {
		if _, ok := timeValue(*e.OccurredAt); !ok {
			return false
		}
	}
	if e.Links == nil || len(e.Links) > 32 || !attributes(e.Attributes) || !validEvidence(e.Evidence) {
		return false
	}
	for _, link := range e.Links {
		r := link.Target
		if !identifiers([]string{link.Relation, r.SourceID, r.SourceInstance, r.Generation, r.Stream,
			r.Type, r.ID, r.ProjectionProfile}) {
			return false
		}
	}
	return true
}

func validCapability(c Capability) bool {
	return (c.Kind == "event_stream" || c.Kind == "snapshot") && c.Types != nil &&
		c.Limitations != nil && identifiers(c.Types) && identifiers(c.Limitations)
}

func validCoverage(c Coverage) bool {
	if c.Kind != "event_stream" && c.Kind != "snapshot" && c.Kind != "unknown" {
		return false
	}
	if c.Limitations == nil || !identifiers(c.Limitations) {
		return false
	}
	var start, end time.Time
	var ok bool
	if c.WindowStart != nil {
		start, ok = timeValue(*c.WindowStart)
		if !ok {
			return false
		}
	}
	if c.WindowEnd != nil {
		end, ok = timeValue(*c.WindowEnd)
		if !ok {
			return false
		}
	}
	return start.IsZero() || end.IsZero() || !end.Before(start)
}
