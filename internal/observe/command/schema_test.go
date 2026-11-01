package command_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/jeon-jihyeon/rindcut/internal/observe/command"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemaConformance(t *testing.T) {
	base := filepath.Join("..", "..", "..", "schemas", "observe")
	data, err := os.ReadFile(filepath.Join(base, "conformance.json"))
	require.NoError(t, err)
	var tcs []struct {
		Name      string          `json:"name"`
		Schema    string          `json:"schema"`
		Valid     bool            `json:"valid"`
		WireValid *bool           `json:"wire_valid,omitempty"`
		Document  json.RawMessage `json:"document"`
	}
	require.NoError(t, json.Unmarshal(data, &tcs))
	for _, tc := range tcs {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join(base, tc.Schema+".schema.json"))
			require.NoError(t, err)
			var schema jsonschema.Schema
			require.NoError(t, json.Unmarshal(data, &schema))
			resolved, err := schema.Resolve(nil)
			require.NoError(t, err)
			var schemaValue any
			require.NoError(t, json.Unmarshal(tc.Document, &schemaValue))
			assert.Equal(t, tc.Valid, resolved.Validate(schemaValue) == nil, "parsed JSON shape")
			wireValid := tc.Valid
			if tc.WireValid != nil {
				wireValid = *tc.WireValid
			}
			switch tc.Schema {
			case "source-response":
				request, definition := sourceInput()
				_, err = command.DecodePage(tc.Document, request, definition)
				assert.Equal(t, wireValid, err == nil, "original wire bytes")
			case "definition":
				_, err = command.ParseDefinition(tc.Document)
				assert.Equal(t, wireValid, err == nil, "original wire bytes")
			case "observer-response":
				request := observerInput(t)
				request.SourceRevision = 1
				_, err = command.DecodeObservation(tc.Document, request)
				assert.Equal(t, wireValid, err == nil, "original wire bytes")
			default:
				assert.Nil(t, tc.WireValid, "wire expectation needs a runtime decoder")
			}
		})
	}
}
