package command_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeon-jihyeon/rindcut/internal/observe/command"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const eventJSON = `{"source_instance":"acme","generation":"g1","stream":"actions","id":"e1","type":"acme.action","schema_version":"v1","adapter_revision":"v1","projection_profile":"metadata","links":[],"attributes":{},"evidence":[]}`

const pagePrefix = `{"protocol":"rindcut.source/v1","source_id":"acme","source_revision":1,"source_instance":"acme","stream":"actions","generation":"g1","capability":{"kind":"event_stream","types":[],"limitations":[]},"status":"ok","records":[`

const pageSuffix = `],"next_cursor":"1","has_more":false,"coverage":{"kind":"event_stream","limitations":[]}}`

func sourceInput() (command.ReadRequest, command.Definition) {
	return command.ReadRequest{Protocol: command.SourceProtocol, Operation: "read", SourceID: "acme", SourceRevision: 1,
			SourceInstance: "acme", Stream: "actions", Limit: 100}, command.Definition{
			ID: "acme", SourceInstance: "acme", Stream: "actions", AdapterRevision: "v1", ProjectionProfile: "metadata",
			Capability: command.Capability{Kind: "event_stream", Types: []string{}, Limitations: []string{}},
			Reader:     command.Reader{AllowedAttributes: []string{"n", "text"}},
		}
}

func TestDecodePage(t *testing.T) {
	valid := pagePrefix + eventJSON + pageSuffix
	tcs := []struct {
		name    string
		data    string
		invalid bool
	}{
		{name: "valid", data: valid},
		{name: "duplicate", data: strings.Replace(valid, `"id":"e1"`, `"id":"e1","id":"e2"`, 1), invalid: true},
		{name: "escaped_duplicate", data: strings.Replace(valid, `"id":"e1"`, `"id":"e1","\u0069d":"e2"`, 1), invalid: true},
		{name: "unknown", data: strings.Replace(valid, `"id":"e1"`, `"id":"e1","payload":"secret"`, 1), invalid: true},
		{name: "case_alias", data: strings.Replace(valid, `"id":"e1"`, `"ID":"e1"`, 1), invalid: true},
		{name: "missing", data: strings.Replace(valid, `"evidence":[]`, `"other":[]`, 1), invalid: true},
		{name: "null_optional", data: strings.Replace(valid, `"id":"e1"`, `"id":"e1","occurred_at":null`, 1), invalid: true},
		{name: "null_array", data: strings.Replace(valid, `"links":[]`, `"links":null`, 1), invalid: true},
		{name: "float", data: strings.Replace(valid, `"attributes":{}`, `"attributes":{"n":1.0}`, 1), invalid: true},
		{name: "exponent", data: strings.Replace(valid, `"attributes":{}`, `"attributes":{"n":1e1}`, 1), invalid: true},
		{name: "negative_zero", data: strings.Replace(valid, `"attributes":{}`, `"attributes":{"n":-0}`, 1), invalid: true},
		{name: "large_integer", data: strings.Replace(valid, `"attributes":{}`, `"attributes":{"n":9007199254740992}`, 1), invalid: true},
		{name: "max_integer", data: strings.Replace(valid, `"attributes":{}`, `"attributes":{"n":9007199254740991}`, 1)},
		{name: "forbidden_attribute", data: strings.Replace(valid, `"attributes":{}`, `"attributes":{"secret":"x"}`, 1), invalid: true},
		{name: "surrogate", data: strings.Replace(valid, `"id":"e1"`, `"id":"\ud800"`, 1), invalid: true},
		{name: "low_surrogate", data: strings.Replace(valid, `"id":"e1"`, `"id":"\udfff"`, 1), invalid: true},
		{name: "surrogate_pair", data: strings.Replace(valid, `"id":"e1"`, `"id":"\ud83d\ude00"`, 1)},
		{name: "invalid_utf8", data: strings.Replace(valid, "e1", string([]byte{0xff}), 1), invalid: true},
		{name: "trailing_json", data: valid + `{}`, invalid: true},
		{name: "trailing_whitespace", data: valid + "\n "},
		{name: "bad_time", data: strings.Replace(valid, `"id":"e1"`, `"id":"e1","occurred_at":"2026-01-01"`, 1), invalid: true},
		{name: "too_precise", data: strings.Replace(valid, `"id":"e1"`, `"id":"e1","occurred_at":"2026-01-01T00:00:00.1234567890Z"`, 1), invalid: true},
		{name: "url_ref", data: strings.Replace(valid, `"evidence":[]`, `"evidence":[{"kind":"read","ref":{"namespace":"https","id":"https://example.com"}}]`, 1), invalid: true},
		{name: "missing_cursor", data: strings.Replace(valid, `"next_cursor":"1"`, `"next_cursor":null`, 1), invalid: true},
		{name: "wrong_identity", data: strings.Replace(valid, `"source_revision":1`, `"source_revision":2`, 1), invalid: true},
		{name: "oversized", data: valid + strings.Repeat(" ", command.MaxOutput), invalid: true},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, d := sourceInput()
			_, err := command.DecodePage([]byte(tc.data), req, d)
			assert.Equal(t, tc.invalid, err != nil)
		})
	}
}

func TestProjection(t *testing.T) {
	var event command.Event
	require.NoError(t, command.Decode([]byte(eventJSON), &event))
	stamp := "2026-01-01T09:00:00+09:00"
	event.OccurredAt = &stamp
	event.Attributes = map[string]any{"Z": true, "text": "<>&\u2028\u2029雪/\b\f\n\r\t\u0001\"\\",
		"A": json.Number("-9007199254740991")}
	tcs := []struct {
		name    string
		encoded bool
	}{{name: "typed"}, {name: "json", encoded: true}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(event)
			require.NoError(t, err)
			var e command.Event
			require.NoError(t, command.Decode(data, &e))
			if tc.encoded {
				data, err := json.Marshal(e)
				require.NoError(t, err)
				require.NoError(t, command.Decode(data, &e))
			}
			data, sum, err := command.Projection(e)
			require.NoError(t, err)
			assert.Equal(t, "sha256:c8ff6e6a892826ac9b07c285ce6552c6e5ff31c9def7118a58b02359a0e72ea8", sum)
			assert.Contains(t, string(data), `\u003c\u003e\u0026\u2028\u2029雪/\b\f\n\r\t\u0001\"\\`)
		})
	}
}

func TestProjectionVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", "observe", "projection-vectors.json"))
	require.NoError(t, err)
	var tcs []struct {
		Name      string          `json:"name"`
		Event     json.RawMessage `json:"event"`
		Canonical string          `json:"canonical"`
		Digest    string          `json:"digest"`
	}
	require.NoError(t, json.Unmarshal(data, &tcs))
	for _, tc := range tcs {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			var event command.Event
			require.NoError(t, command.Decode(tc.Event, &event))
			canonical, digest, err := command.Projection(event)
			require.NoError(t, err)
			assert.Equal(t, tc.Canonical, string(canonical))
			assert.Equal(t, tc.Digest, digest)
		})
	}
}
