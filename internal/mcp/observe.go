package mcp

import (
	"context"
	"encoding/json"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeon-jihyeon/rindcut/app"
)

type sourceListInput struct {
	IncludeRemoved bool `json:"include_removed,omitempty"`
}

func addObserve(srv *sdk.Server, a app.App) {
	addInput(srv, "source_preview", "Preview a common-contract source without running its reader or observers. Show the owner the definition, resolved programs, fingerprints, recovery effects and tick schedule target. Enabled sources ensure a shared five-minute tick after approval. Programs run with your OS privileges. Registration grants no human approval authority. Adapter semantics are in the bundled rindcut skill's observe-contract.md."+dataNote,
		sourceInputSchema(false), a.SourcePreview)
	addInput(srv, "source_save", "Save exactly the source definition, recovery choice and tick target the owner previewed and confirmed. Reject a stale digest before running any program. An approved reset runs the reader once to verify its generation. After saving an enabled source, ensure the shared five-minute schedule without waiting for its first tick. Read schedule.state and schedule.reason separately: scheduling failure leaves the source saved. Retry install without saving another revision. A changed definition, executable or tick target needs a fresh preview. No producer action is rerun.",
		sourceInputSchema(true), a.SourceSave)
	add(srv, "source_list", "List registered source metadata without commands, credentials or native cursors."+dataNote,
		func(ctx context.Context, in sourceListInput) (listResult[app.SourceSummary], error) {
			items, err := a.SourceList(ctx, in.IncludeRemoved)
			return listResult[app.SourceSummary]{Items: append([]app.SourceSummary{}, items...)}, err
		})
	add(srv, "source_remove", "Stop collecting and observing one source after confirmation. Preserve its records and cancel its unresolved work.", a.SourceRemove)
	add(srv, "observe_sync", "Run one bounded collection and observation cycle. Source claims and observed results stay separate. No producer action is retried."+dataNote,
		func(ctx context.Context, _ Empty) (app.ObserveCycle, error) { return a.ObserveSync(ctx) })
	add(srv, "observe_records", "Read a cursor page of event, observation or cycle metadata. This calls no source or observer and changes no progress. The cursor binds the store and filters."+dataNote, a.ObserveRecords)
	add(srv, "observe_status", "Read source coverage, gaps, conflicts, unresolved observations and collector health without external calls or writes."+dataNote,
		func(ctx context.Context, _ Empty) (app.ObserveStatus, error) { return a.ObserveStatus(ctx) })
}

func sourceInputSchema(save bool) map[string]any {
	var definition map[string]any
	if err := json.Unmarshal([]byte(app.SourceDefinitionSchema()), &definition); err != nil {
		panic(err)
	}
	defs := definition["$defs"]
	delete(definition, "$defs")
	delete(definition, "$id")
	delete(definition, "$schema")
	configure := map[string]any{
		"operation":         map[string]any{"type": "string", "const": "configure"},
		"definition":        definition,
		"expected_revision": map[string]any{"type": "integer", "minimum": 0},
		"recovery":          sourceRecoverySchema(),
	}
	recoverStore := map[string]any{
		"operation": map[string]any{"type": "string", "const": "recover_store"},
		"recovery": map[string]any{"type": "object", "additionalProperties": false,
			"properties": map[string]any{"mode": map[string]any{"type": "string", "const": "new_store"}},
			"required":   []string{"mode"}},
		"expected_store_token": map[string]any{"type": "string"},
	}
	configureRequired := []string{"definition", "expected_revision"}
	recoveryRequired := []string{"operation", "recovery"}
	if save {
		for _, properties := range []map[string]any{configure, recoverStore} {
			properties["preview_digest"] = map[string]any{"type": "string"}
			properties["confirmed"] = map[string]any{"type": "boolean", "const": true}
		}
		configureRequired = append(configureRequired, "preview_digest", "confirmed")
		recoveryRequired = append(recoveryRequired, "expected_store_token", "preview_digest", "confirmed")
	}
	return map[string]any{"type": "object", "$defs": defs, "oneOf": []any{
		map[string]any{"properties": configure, "required": configureRequired, "additionalProperties": false},
		map[string]any{"properties": recoverStore, "required": recoveryRequired, "additionalProperties": false},
	}}
}

func sourceRecoverySchema() map[string]any {
	return map[string]any{"type": "object", "oneOf": []any{
		map[string]any{"properties": map[string]any{
			"mode":             map[string]any{"type": "string", "const": "backfill"},
			"checkpoint_token": map[string]any{"type": "string"},
		}, "required": []string{"mode", "checkpoint_token"}, "additionalProperties": false},
		map[string]any{"properties": map[string]any{
			"mode":                map[string]any{"type": "string", "const": "reset"},
			"checkpoint_token":    map[string]any{"type": "string"},
			"collection_identity": map[string]any{"type": "string"},
			"generation":          map[string]any{"type": "string"},
		}, "required": []string{"mode", "checkpoint_token", "collection_identity", "generation"}, "additionalProperties": false},
	}}
}
