package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jeon-jihyeon/rindcut/app"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserveProcessAdapter(t *testing.T) {
	index := slices.Index(os.Args, "--observe-fixture")
	if index < 0 {
		return
	}
	var request map[string]any
	if json.NewDecoder(os.Stdin).Decode(&request) != nil {
		os.Exit(2)
	}
	var response map[string]any
	if request["protocol"] == "rindcut.source/v1" {
		if marker := slices.Index(os.Args, "--observe-marker"); marker >= 0 {
			file, err := os.OpenFile(os.Args[marker+1], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				os.Exit(2)
			}
			_, writeErr := file.WriteString("read\n")
			if closeErr := file.Close(); writeErr != nil || closeErr != nil {
				os.Exit(2)
			}
		}
		if data, _ := os.ReadFile(os.Args[index+2]); string(data) == "block_reader" && request["source_id"] == "acme" {
			fixtureBlock(os.Args[index+2])
		}
		response = fixturePage(request, os.Args[index+1])
	} else {
		response = fixtureObservation(request, os.Args[index+2])
	}
	if json.NewEncoder(os.Stdout).Encode(response) != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func fixturePage(request map[string]any, path string) map[string]any {
	var events []map[string]any
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &events) != nil {
		os.Exit(2)
	}
	offset := 0
	if cursor, ok := request["cursor"].(string); ok {
		offset, err = strconv.Atoi(cursor)
		if err != nil || offset > len(events) {
			os.Exit(2)
		}
	}
	end := min(len(events), offset+int(request["limit"].(float64)))
	return map[string]any{"protocol": request["protocol"], "source_id": request["source_id"],
		"source_revision": request["source_revision"], "source_instance": request["source_instance"],
		"stream": request["stream"], "generation": "g1", "status": "ok", "records": events[offset:end],
		"next_cursor": strconv.Itoa(end), "has_more": end < len(events),
		"capability": map[string]any{"kind": "event_stream", "types": []string{"acme.delivery"}, "limitations": []string{}},
		"coverage":   map[string]any{"kind": "event_stream", "limitations": []string{}}}
}

func fixtureObservation(request map[string]any, destination string) map[string]any {
	response := map[string]any{}
	for _, key := range []string{"protocol", "source_id", "source_revision", "observer_id", "observer_revision",
		"criterion_id", "criterion_revision", "series_id", "target_ref"} {
		response[key] = request[key]
	}
	data, _ := os.ReadFile(destination)
	if string(data) == "block" {
		fixtureBlock(destination)
	}
	response["result"], response["reason_code"], response["evidence"] = "pending", "absent", []any{}
	if string(data) == "delivered" {
		response["result"], response["reason_code"] = "confirmed", "destination_present"
	}
	if string(data) == "corrected" {
		response["result"], response["reason_code"] = "refuted", "destination_corrected"
		if previous, ok := request["previous_observation_id"]; ok {
			response["supersedes"] = map[string]any{"store_id": request["target_ref"].(map[string]any)["store_id"], "observation_id": previous}
		}
	}
	if string(data) == "invalid" {
		response["result"] = "human_approved"
	}
	if string(data) == "future" {
		now, err := time.Parse(time.RFC3339Nano, request["now"].(string))
		if err != nil {
			os.Exit(2)
		}
		response["next_observe_at"] = now.Add(24 * time.Hour).Format(time.RFC3339Nano)
	}
	return response
}

func fixtureBlock(path string) {
	if os.WriteFile(path+".started", []byte("started"), 0o600) != nil {
		os.Exit(2)
	}
	time.Sleep(10 * time.Second)
}

type observeFixture struct {
	app         app.App
	definition  map[string]any
	events      string
	destination string
	now         time.Time
}

func newObserveFixture(t *testing.T) *observeFixture {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	require.NoError(t, os.Mkdir(home, 0o700))
	program, err := os.Executable()
	require.NoError(t, err)
	f := &observeFixture{events: filepath.Join(dir, "events.json"), destination: filepath.Join(dir, "destination"),
		now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	f.app = app.App{Home: home, Now: func() time.Time { return f.now }}
	args := []string{"-test.run=^TestObserveProcessAdapter$", "--", "--observe-fixture", f.events, f.destination}
	f.definition = map[string]any{"id": "acme", "enabled": true, "source_instance": "acme", "stream": "deliveries",
		"capability":       map[string]any{"kind": "event_stream", "types": []string{"acme.delivery"}, "limitations": []string{}},
		"adapter_revision": "v1", "projection_profile": "v1",
		"reader": map[string]any{"program": program, "args": args, "allowed_attributes": []string{"producer_claim", "authority", "verified"}},
		"observers": []any{map[string]any{"id": "destination", "types": []string{"acme.delivery"}, "criterion_id": "actual_delivery",
			"criterion_revision": "v1", "parameters": map[string]any{}, "program": program, "args": args,
			"deadline_seconds": 2, "interval_seconds": 1, "mode": "watch"}}}
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success")})
	f.save(t, 0)
	return f
}

func fixtureEvent(id, claim string) map[string]any {
	return map[string]any{"source_instance": "acme", "generation": "g1", "stream": "deliveries", "id": id,
		"type": "acme.delivery", "schema_version": "v1", "adapter_revision": "v1", "projection_profile": "v1",
		"occurred_at": "2025-12-31T23:00:00Z", "action_id": "a1", "links": []any{},
		"attributes": map[string]any{"producer_claim": claim}, "evidence": []any{}}
}

func (f *observeFixture) writeEvents(t *testing.T, events []map[string]any) {
	t.Helper()
	data, err := json.Marshal(events)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.events, data, 0o600))
}

func (f *observeFixture) save(t *testing.T, revision int64) {
	t.Helper()
	data, err := json.Marshal(f.definition)
	require.NoError(t, err)
	request := app.SourceRequest{Definition: data, ExpectedRevision: &revision}
	preview, err := f.app.SourcePreview(t.Context(), request)
	require.NoError(t, err)
	request.PreviewDigest, request.Confirmed = preview.PreviewDigest, true
	_, err = f.app.SourceSave(t.Context(), request)
	require.NoError(t, err)
}

func (f *observeFixture) sync(t *testing.T) app.ObserveCycle {
	t.Helper()
	cycle, err := f.app.ObserveSync(t.Context())
	require.NoError(t, err)
	f.now = f.now.Add(time.Second)
	return cycle
}

func TestObserveIndependentDestinationAndCorrection(t *testing.T) {
	t.Parallel()
	f := newObserveFixture(t)
	assert.Equal(t, 1, f.sync(t).Captured)
	assert.Equal(t, 1, f.sync(t).Observations)
	page, err := f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "observation"})
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	assert.Contains(t, string(page.Records[0].Data), `"result":"pending"`)
	assert.NotContains(t, string(page.Records[0].Data), "producer_claim")
	require.NoError(t, os.WriteFile(f.destination, []byte("delivered"), 0o600))
	assert.Equal(t, 1, f.sync(t).Observations)
	require.NoError(t, os.WriteFile(f.destination, []byte("corrected"), 0o600))
	assert.Equal(t, 1, f.sync(t).Observations)
	page, err = f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "observation"})
	require.NoError(t, err)
	require.Len(t, page.Records, 3)
	assert.Contains(t, string(page.Records[1].Data), `"result":"confirmed"`)
	assert.Contains(t, string(page.Records[2].Data), `"result":"refuted"`)
	assert.Contains(t, string(page.Records[2].Data), page.Records[1].ID)
	assert.NotContains(t, string(page.Records[1].Data), "supersedes")
}

func TestObserveReplayConflictAndLateAppend(t *testing.T) {
	t.Parallel()
	f := newObserveFixture(t)
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e1", "success"), fixtureEvent("e1", "failed")})
	assert.Equal(t, 2, f.sync(t).Captured)
	status, err := f.app.ObserveStatus(t.Context())
	require.NoError(t, err)
	require.Len(t, status.Sources, 1)
	assert.Equal(t, 1, status.Sources[0].Conflicts)
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e1", "success"), fixtureEvent("e1", "failed"), fixtureEvent("e2", "success")})
	assert.Equal(t, 1, f.sync(t).Captured)
	page, err := f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "event", Limit: 1})
	require.NoError(t, err)
	ids := []string{}
	for {
		for _, row := range page.Records {
			ids = append(ids, row.ID)
		}
		if !page.HasMore {
			break
		}
		require.NotNil(t, page.NextCursor)
		page, err = f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "event", Cursor: *page.NextCursor, Limit: 2})
		require.NoError(t, err)
	}
	assert.Len(t, ids, 3)
}

func TestObserveRejectsAuthorityAndDisabledSourceStops(t *testing.T) {
	t.Parallel()
	f := newObserveFixture(t)
	event := fixtureEvent("e1", "success")
	event["attributes"].(map[string]any)["authority"] = "person"
	event["attributes"].(map[string]any)["verified"] = true
	f.writeEvents(t, []map[string]any{event})
	f.sync(t)
	require.NoError(t, os.WriteFile(f.destination, []byte("invalid"), 0o600))
	cycle := f.sync(t)
	assert.Contains(t, cycle.Errors, "invalid_response")
	page, err := f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "observation"})
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	assert.Contains(t, string(page.Records[0].Data), `"result":"unknown"`)
	assert.NotContains(t, string(page.Records[0].Data), "human_approved")
	f.definition["enabled"] = false
	f.save(t, 1)
	cycle = f.sync(t)
	assert.Zero(t, cycle.Attempts)
	page, err = f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "observation"})
	require.NoError(t, err)
	assert.Len(t, page.Records, 1)
}

func TestObserveRejectsWholePageWithoutAdvancing(t *testing.T) {
	t.Parallel()
	f := newObserveFixture(t)
	bad := fixtureEvent("e2", "success")
	bad["attributes"].(map[string]any)["raw_body"] = "forbidden"
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), bad})
	cycle := f.sync(t)
	assert.Zero(t, cycle.Captured)
	assert.Contains(t, cycle.Errors, "invalid_response")
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e2", "success")})
	assert.Equal(t, 2, f.sync(t).Captured)
	page, err := f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "event"})
	require.NoError(t, err)
	assert.Len(t, page.Records, 2)
	assert.NotContains(t, fmt.Sprint(page.Records), "forbidden")
}

func TestObserveCancelledCycleDoesNotClaimSuccess(t *testing.T) {
	t.Parallel()
	f := newObserveFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.app.ObserveSync(ctx)
	require.Error(t, err)
	page, err := f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "event"})
	require.NoError(t, err)
	assert.Empty(t, page.Records)
}

func TestObserveInterruptedObserverRetainsUnknown(t *testing.T) {
	t.Parallel()
	f := newObserveFixture(t)
	f.sync(t)
	require.NoError(t, os.WriteFile(f.destination, []byte("block"), 0o600))
	f.interrupt(t)
	page, err := f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "observation"})
	require.NoError(t, err)
	assert.Empty(t, page.Records)
	require.NoError(t, os.WriteFile(f.destination, []byte("delivered"), 0o600))
	f.sync(t)
	page, err = f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "observation"})
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	assert.Contains(t, string(page.Records[0].Data), `"result":"unknown"`)
	assert.Contains(t, string(page.Records[0].Data), `"reason_code":"interrupted"`)
	f.sync(t)
	page, err = f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "observation"})
	require.NoError(t, err)
	require.Len(t, page.Records, 2)
	assert.Contains(t, string(page.Records[1].Data), `"result":"confirmed"`)
}

func (f *observeFixture) interrupt(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.app.ObserveSync(ctx); done <- err }()
	require.Eventually(t, func() bool { _, err := os.Stat(f.destination + ".started"); return err == nil },
		5*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop the process")
	}
}

func TestObserveRotationSurvivesInterruptedSource(t *testing.T) {
	t.Parallel()
	f := newObserveFixture(t)
	f.definition["id"] = "acme.z"
	f.save(t, 0)
	require.NoError(t, os.WriteFile(f.destination, []byte("block_reader"), 0o600))
	f.interrupt(t)
	require.NoError(t, os.Remove(f.destination+".started"))
	f.interrupt(t)
	page, err := f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "event", SourceID: "acme.z"})
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	assert.Contains(t, string(page.Records[0].Data), `"source_id":"acme.z"`)
}

func TestObserveFutureRetryStillShowsOverdue(t *testing.T) {
	t.Parallel()
	f := newObserveFixture(t)
	f.sync(t)
	require.NoError(t, os.WriteFile(f.destination, []byte("future"), 0o600))
	f.sync(t)
	f.now = f.now.Add(time.Minute)
	status, err := f.app.ObserveStatus(t.Context())
	require.NoError(t, err)
	require.Len(t, status.Sources, 1)
	assert.Equal(t, 1, status.Sources[0].Pending)
	assert.Equal(t, 1, status.Sources[0].Overdue)
}

func TestObserveBudgetExhaustionPersistsIncompleteCycle(t *testing.T) {
	t.Parallel()
	f := newObserveFixture(t)
	require.NoError(t, os.WriteFile(f.destination, []byte("block_reader"), 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	cycle, err := f.app.ObserveSync(ctx)
	require.Error(t, err)
	assert.True(t, cycle.Incomplete)
	page, err := f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "cycle"})
	require.NoError(t, err)
	found := false
	for _, record := range page.Records {
		var data map[string]any
		require.NoError(t, json.Unmarshal(record.Data, &data))
		if data["id"] == cycle.ID {
			found = true
			assert.Equal(t, true, data["incomplete"])
		}
	}
	assert.True(t, found, "incomplete cycle must be durable")
}
