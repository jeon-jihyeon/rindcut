package mcp_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserveReadOnlyAbsentStore(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		want map[string]any
	}{
		{name: "source_list", want: map[string]any{"items": []any{}}},
		{name: "observe_records", want: map[string]any{"records": []any{}, "next_cursor": nil,
			"has_more": false, "store_present": false}},
		{name: "observe_status", want: map[string]any{"sources": []any{}, "store_present": false}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := start(t)
			before, err := os.ReadDir(h.app.Home)
			require.NoError(t, err)
			out, msg := h.call(t, tc.name, map[string]any{})
			assert.Empty(t, msg)
			assert.Equal(t, tc.want, out)
			after, err := os.ReadDir(h.app.Home)
			require.NoError(t, err)
			assert.Equal(t, len(before), len(after))
			assert.NoDirExists(t, filepath.Join(h.app.Home, "observe"))
		})
	}
}

func TestSourceInputSchema(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		tool string
		args map[string]any
	}{
		{name: "unknown top-level key", tool: "source_preview", args: map[string]any{"acme": true}},
		{name: "configuration needs revision", tool: "source_preview", args: map[string]any{"definition": map[string]any{}}},
		{name: "configuration needs object", tool: "source_preview",
			args: map[string]any{"definition": "acme", "expected_revision": 0}},
		{name: "recovery excludes definition", tool: "source_preview", args: map[string]any{
			"operation": "recover_store", "definition": map[string]any{}, "recovery": map[string]any{"mode": "new_store"}}},
		{name: "recovery excludes source revision", tool: "source_preview", args: map[string]any{
			"operation": "recover_store", "expected_revision": 0, "recovery": map[string]any{"mode": "new_store"}}},
		{name: "reset needs generation", tool: "source_preview", args: map[string]any{
			"definition": map[string]any{}, "expected_revision": 0,
			"recovery": map[string]any{"mode": "reset", "checkpoint_token": "acme", "collection_identity": "acme"}}},
		{name: "save needs preview", tool: "source_save", args: map[string]any{
			"definition": map[string]any{}, "expected_revision": 0, "confirmed": true}},
		{name: "save needs confirmation", tool: "source_save", args: map[string]any{
			"definition": map[string]any{}, "expected_revision": 0, "preview_digest": "acme"}},
		{name: "save refuses false confirmation", tool: "source_save", args: map[string]any{
			"definition": map[string]any{}, "expected_revision": 0, "preview_digest": "acme", "confirmed": false}},
		{name: "recovery save needs store token", tool: "source_save", args: map[string]any{
			"operation": "recover_store", "recovery": map[string]any{"mode": "new_store"},
			"preview_digest": "acme", "confirmed": true}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := start(t)
			result, err := h.sess.CallTool(context.Background(), &sdk.CallToolParams{Name: tc.tool, Arguments: tc.args})
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.True(t, result.IsError)
			assert.NoDirExists(t, filepath.Join(h.app.Home, "observe"))
		})
	}
}

func TestSourceRegistration(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name  string
		extra map[string]any
	}{
		{name: "default configure", extra: map[string]any{}},
		{name: "explicit configure", extra: map[string]any{"operation": "configure"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := start(t)
			executable, err := os.Executable()
			require.NoError(t, err)
			require.NoError(t, os.Chmod(h.app.Home, 0o700))
			args := map[string]any{
				"expected_revision": 0,
				"definition": map[string]any{
					"id": "acme", "enabled": true, "source_instance": "acme", "stream": "actions",
					"capability":       map[string]any{"kind": "event_stream", "types": []string{}, "limitations": []string{}},
					"adapter_revision": "v1", "projection_profile": "metadata",
					"reader":    map[string]any{"program": executable, "args": []string{}, "allowed_attributes": []string{}},
					"observers": []any{},
				},
			}
			maps.Copy(args, tc.extra)
			preview, message := h.call(t, "source_preview", args)
			require.Empty(t, message)
			digest, ok := preview["preview_digest"].(string)
			require.True(t, ok)
			require.NotEmpty(t, digest)
			assert.NoDirExists(t, filepath.Join(h.app.Home, "observe"))
			args["preview_digest"], args["confirmed"] = digest, true
			saved, message := h.call(t, "source_save", args)
			require.Empty(t, message)
			source, ok := saved["source"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, "acme", source["id"])
			assert.Equal(t, float64(1), source["revision"])
			assert.NotContains(t, source, "reader")
			status, err := h.app.Status(context.Background())
			require.NoError(t, err)
			assert.True(t, status.TickInstalled)
			assert.Equal(t, "ready", status.Schedule.State)
			removed, message := h.call(t, "source_remove", map[string]any{
				"id": "acme", "expected_revision": 1, "confirmed": true,
			})
			require.Empty(t, message)
			assert.Equal(t, true, removed["removed"])
			out, message := h.call(t, "source_list", map[string]any{})
			assert.Empty(t, message)
			assert.Equal(t, map[string]any{"items": []any{}}, out)
		})
	}
}

func TestObserveSchemas(t *testing.T) {
	t.Parallel()
	h := start(t)
	tools, err := h.sess.ListTools(context.Background(), nil)
	require.NoError(t, err)
	byName := map[string]*sdk.Tool{}
	for _, tool := range tools.Tools {
		byName[tool.Name] = tool
	}
	tcs := []struct{ name string }{{name: "source_preview"}, {name: "source_save"}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tool := byName[tc.name]
			require.NotNil(t, tool)
			encoded, err := json.Marshal(tool.InputSchema)
			require.NoError(t, err)
			var schema struct {
				Type  string `json:"type"`
				OneOf []struct {
					AdditionalProperties bool                      `json:"additionalProperties"`
					Properties           map[string]map[string]any `json:"properties"`
				} `json:"oneOf"`
			}
			require.NoError(t, json.Unmarshal(encoded, &schema))
			assert.Equal(t, "object", schema.Type)
			require.Len(t, schema.OneOf, 2)
			assert.False(t, schema.OneOf[0].AdditionalProperties)
			assert.False(t, schema.OneOf[1].AdditionalProperties)
			assert.Equal(t, "object", schema.OneOf[0].Properties["definition"]["type"])
		})
	}
}

func TestSourceDefinitionDiscovery(t *testing.T) {
	t.Parallel()
	h := start(t)
	tools, err := h.sess.ListTools(t.Context(), nil)
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join("..", "..", "schemas", "observe", "conformance.json"))
	require.NoError(t, err)
	var fixtures []struct {
		Name     string          `json:"name"`
		Schema   string          `json:"schema"`
		Valid    bool            `json:"valid"`
		Document json.RawMessage `json:"document"`
	}
	require.NoError(t, json.Unmarshal(data, &fixtures))
	for _, tool := range tools.Tools {
		if tool.Name != "source_preview" && tool.Name != "source_save" {
			continue
		}
		t.Run(tool.Name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(tool.InputSchema)
			require.NoError(t, err)
			var shape struct {
				Defs  map[string]json.RawMessage `json:"$defs"`
				OneOf []struct {
					Properties map[string]struct {
						Required []string `json:"required"`
					} `json:"properties"`
				} `json:"oneOf"`
			}
			require.NoError(t, json.Unmarshal(encoded, &shape))
			require.Len(t, shape.OneOf, 2)
			assert.Equal(t, []string{"id", "enabled", "source_instance", "stream", "capability", "adapter_revision",
				"projection_profile", "reader", "observers"}, shape.OneOf[0].Properties["definition"].Required)
			for _, name := range []string{"capability", "reader", "observer", "attributes"} {
				assert.Contains(t, shape.Defs, name)
			}
			var schema jsonschema.Schema
			require.NoError(t, json.Unmarshal(encoded, &schema))
			resolved, err := schema.Resolve(nil)
			require.NoError(t, err)
			for _, fixture := range fixtures {
				if fixture.Schema != "definition" {
					continue
				}
				var definition any
				require.NoError(t, json.Unmarshal(fixture.Document, &definition))
				args := map[string]any{"definition": definition, "expected_revision": 0}
				if tool.Name == "source_save" {
					args["confirmed"], args["preview_digest"] = true, "acme"
				}
				assert.Equal(t, fixture.Valid, resolved.Validate(args) == nil, fixture.Name)
			}
		})
	}
}
