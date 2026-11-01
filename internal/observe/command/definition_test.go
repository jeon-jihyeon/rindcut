package command_test

import (
	"strings"
	"testing"

	"github.com/jeon-jihyeon/rindcut/internal/observe/command"
	"github.com/stretchr/testify/assert"
)

const definitionJSON = `{"id":"acme","enabled":true,"source_instance":"acme","stream":"actions","capability":{"kind":"event_stream","types":[],"limitations":[]},"adapter_revision":"v1","projection_profile":"metadata","reader":{"program":"acme","args":[],"allowed_attributes":[]},"observers":[{"id":"effect","types":["acme.action"],"criterion_id":"landed","criterion_revision":"v1","parameters":{},"program":"acme","args":[],"deadline_seconds":60}]}`

func TestParseDefinition(t *testing.T) {
	tcs := []struct {
		name    string
		data    string
		invalid bool
	}{
		{name: "defaults", data: definitionJSON},
		{name: "zero_timeout", data: strings.Replace(definitionJSON, `"program":"acme"`, `"program":"acme","timeout_seconds":0`, 1), invalid: true},
		{name: "large_timeout", data: strings.Replace(definitionJSON, `"program":"acme"`, `"program":"acme","timeout_seconds":601`, 1), invalid: true},
		{name: "no_types", data: strings.Replace(definitionJSON, `["acme.action"]`, `[]`, 1), invalid: true},
		{name: "actor", data: strings.Replace(definitionJSON, `"deadline_seconds":60`, `"deadline_seconds":60,"actor_kind":"model"`, 1)},
		{name: "empty_actor", data: strings.Replace(definitionJSON, `"deadline_seconds":60`, `"deadline_seconds":60,"actor_kind":""`, 1), invalid: true},
		{name: "empty_mode", data: strings.Replace(definitionJSON, `"deadline_seconds":60`, `"deadline_seconds":60,"mode":""`, 1), invalid: true},
		{name: "invalid_actor", data: strings.Replace(definitionJSON, `"deadline_seconds":60`, `"deadline_seconds":60,"actor_kind":"person"`, 1), invalid: true},
		{name: "null_args", data: strings.Replace(definitionJSON, `"args":[]`, `"args":null`, 1), invalid: true},
		{name: "parameter_object", data: strings.Replace(definitionJSON, `"parameters":{}`, `"parameters":{"x":{}}`, 1), invalid: true},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := command.ParseDefinition([]byte(tc.data))
			assert.Equal(t, tc.invalid, err != nil)
		})
	}
}
