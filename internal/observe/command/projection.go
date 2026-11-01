package command

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

func Projection(e Event) ([]byte, string, error) {
	if !validEvent(e) {
		return nil, "", &Error{Code: "invalid_event"}
	}
	var occurred any
	if e.OccurredAt != nil {
		t, _ := timeValue(*e.OccurredAt)
		occurred = Timestamp(t)
	}
	links := make([]any, 0, len(e.Links))
	for _, link := range e.Links {
		r := link.Target
		links = append(links, []any{link.Relation, []any{r.SourceID, r.SourceInstance, r.Generation, r.Stream,
			r.Type, r.ID, r.ProjectionProfile}})
	}
	keys := make([]string, 0, len(e.Attributes))
	for key := range e.Attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	attrs := make([]any, 0, len(keys))
	for _, key := range keys {
		attrs = append(attrs, []any{key, e.Attributes[key]})
	}
	evidence := make([]any, 0, len(e.Evidence))
	for _, ev := range e.Evidence {
		evidence = append(evidence, []any{ev.Kind, ev.Ref.Namespace, ev.Ref.ID, ev.Digest})
	}
	data, err := json.Marshal([]any{"rindcut.projection/v1", e.SourceInstance, e.Generation, e.Stream, e.ID, e.Type,
		e.SchemaVersion, e.ProducerVersion, e.AdapterRevision, e.ProjectionProfile, occurred, e.ActionID, e.AttemptID,
		links, attrs, evidence})
	if err != nil {
		return nil, "", &Error{Code: "invalid_event"}
	}
	sum := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(sum[:]), nil
}
