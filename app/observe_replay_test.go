package app_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeon-jihyeon/rindcut/app"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserveReplayProcessAdapter(t *testing.T) {
	index := slices.Index(os.Args, "--observe-replay-fixture")
	if index < 0 {
		return
	}
	var request map[string]any
	if json.NewDecoder(os.Stdin).Decode(&request) != nil {
		os.Exit(2)
	}
	var response map[string]any
	if request["protocol"] == "rindcut.source/v1" {
		blocked, _ := os.ReadFile(os.Args[index+3])
		if string(blocked) == "readonly_ledger" {
			ledger := filepath.Join(filepath.Dir(os.Args[index+1]), "home", "observe", "ledger.jsonl")
			if os.Chmod(ledger, 0400) != nil {
				os.Exit(4)
			}
		}
		if cursor, ok := request["cursor"].(string); ok && cursor == string(blocked) {
			fixtureBlock(os.Args[index+3])
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

func newReplayFixture(t *testing.T, limit int, observerIDs []string) (*observeFixture, string) {
	t.Helper()
	dir := t.TempDir()
	program, err := os.Executable()
	require.NoError(t, err)
	f := &observeFixture{events: filepath.Join(dir, "events.json"), destination: filepath.Join(dir, "destination"),
		now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	f.app = app.App{Home: filepath.Join(dir, "home"), Now: func() time.Time { return f.now }}
	block := filepath.Join(dir, "blocked-cursor")
	args := []string{"-test.run=^TestObserveReplayProcessAdapter$", "--", "--observe-replay-fixture", f.events, f.destination, block}
	observers := []any{}
	for _, id := range observerIDs {
		observers = append(observers, map[string]any{"id": id, "types": []string{"acme.delivery"}, "criterion_id": "actual_delivery",
			"criterion_revision": "v1", "parameters": map[string]any{}, "program": program, "args": args,
			"deadline_seconds": 2, "interval_seconds": 1, "mode": "watch"})
	}
	f.definition = map[string]any{"id": "acme", "enabled": true, "source_instance": "acme", "stream": "deliveries",
		"capability":       map[string]any{"kind": "event_stream", "types": []string{"acme.delivery"}, "limitations": []string{}},
		"adapter_revision": "v1", "projection_profile": "v1",
		"reader":    map[string]any{"program": program, "args": args, "allowed_attributes": []string{"producer_claim"}, "limit": limit},
		"observers": observers}
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success")})
	f.save(t, 0)
	return f, block
}

func replayRecovery(t *testing.T, f *observeFixture, revision int64, mode string) {
	t.Helper()
	definition, err := json.Marshal(f.definition)
	require.NoError(t, err)
	in := app.SourceRequest{Definition: definition, ExpectedRevision: &revision}
	preview, err := f.app.SourcePreview(t.Context(), in)
	require.NoError(t, err)
	in.Recovery = &app.SourceRecovery{Mode: mode, CheckpointToken: preview.CheckpointToken}
	if mode == "reset" {
		in.Recovery.CollectionIdentity = preview.CollectionIdentity
		in.Recovery.Generation = "g1"
	}
	preview, err = f.app.SourcePreview(t.Context(), in)
	require.NoError(t, err)
	in.PreviewDigest, in.Confirmed = preview.PreviewDigest, true
	_, err = f.app.SourceSave(t.Context(), in)
	require.NoError(t, err)
}

func replayProgress(t *testing.T, f *observeFixture, field string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.app.Home, "observe", "ledger.jsonl"))
	require.NoError(t, err)
	out := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &row))
		if row["kind"] != "checkpoint" {
			continue
		}
		value, ok := row["data"].(map[string]any)[field].(map[string]any)
		if !ok {
			continue
		}
		id, _ := value["id"].(string)
		if field == "checkpoint" {
			id = value["source_id"].(string)
		}
		out[id] = value
	}
	return out
}

func replaySeriesIDs(t *testing.T, f *observeFixture, revision int, observer, capture string) []string {
	t.Helper()
	ids := []string{}
	for id, series := range replayProgress(t, f, "series") {
		if revision != 0 && series["source_revision"] != float64(revision) {
			continue
		}
		if observer != "" && series["observer_id"] != observer {
			continue
		}
		if capture != "" && series["capture_id"] != capture {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func replayCaptureIDs(t *testing.T, f *observeFixture) map[string]string {
	t.Helper()
	page, err := f.app.ObserveRecords(t.Context(), app.ObserveQuery{Kind: "event"})
	require.NoError(t, err)
	out := map[string]string{}
	for _, row := range page.Records {
		var event map[string]any
		require.NoError(t, json.Unmarshal(row.Data, &event))
		out[event["id"].(string)] = row.ID
	}
	return out
}

func replayInterrupt(t *testing.T, f *observeFixture, block string) {
	t.Helper()
	require.NoError(t, os.WriteFile(block, []byte("1"), 0600))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.app.ObserveSync(ctx); done <- err }()
	require.Eventually(t, func() bool { _, err := os.Stat(block + ".started"); return err == nil },
		10*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop replay")
	}
}

func TestObserveDescriptionReplayKeepsSeries(t *testing.T) {
	t.Parallel()
	f, _ := newReplayFixture(t, 100, []string{"a"})
	f.sync(t)
	f.sync(t)
	before := replayProgress(t, f, "series")
	require.Len(t, before, 1)
	f.definition["description"] = "new description"
	f.save(t, 1)
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e1", "success")})
	assert.Zero(t, f.sync(t).Captured)
	f.sync(t)
	after := replayProgress(t, f, "series")
	require.Len(t, after, 1)
	for id, prior := range before {
		assert.Equal(t, prior["source_revision"], after[id]["source_revision"])
		assert.Equal(t, prior["captured_at"], after[id]["captured_at"])
		assert.Equal(t, prior["deadline_seconds"], after[id]["deadline_seconds"])
	}
	assert.Empty(t, replaySeriesIDs(t, f, 2, "", ""))
	assert.Equal(t, "2", replayProgress(t, f, "checkpoint")["acme"]["cursor"])
	assert.Len(t, replayCaptureIDs(t, f), 1)
}

func TestObserveChangedObserverAppliesToNewCapture(t *testing.T) {
	t.Parallel()
	f, _ := newReplayFixture(t, 100, []string{"a"})
	f.sync(t)
	f.sync(t)
	old := replayCaptureIDs(t, f)["e1"]
	f.definition["observers"].([]any)[0].(map[string]any)["criterion_revision"] = "v2"
	f.save(t, 1)
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e1", "success"), fixtureEvent("e2", "success")})
	assert.Equal(t, 1, f.sync(t).Captured)
	f.sync(t)
	assert.Empty(t, replaySeriesIDs(t, f, 2, "a", old))
	fresh := replayCaptureIDs(t, f)["e2"]
	assert.Len(t, replaySeriesIDs(t, f, 2, "a", fresh), 1)
}

func TestObserveExplicitReplayIsOneShot(t *testing.T) {
	tcs := []struct {
		name string
		mode string
	}{{name: "backfill", mode: "backfill"}, {name: "reset", mode: "reset"}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, _ := newReplayFixture(t, 100, []string{"a"})
			f.sync(t)
			f.sync(t)
			old := replayCaptureIDs(t, f)["e1"]
			prior := replayProgress(t, f, "series")
			if tc.mode == "reset" {
				f.definition["reader"].(map[string]any)["timeout_seconds"] = 31
			}
			replayRecovery(t, f, 1, tc.mode)
			f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e1", "success")})
			assert.Zero(t, f.sync(t).Captured)
			f.sync(t)
			ids := replaySeriesIDs(t, f, 2, "a", old)
			require.Len(t, ids, 1)
			created := replayProgress(t, f, "series")[ids[0]]
			for _, before := range prior {
				assert.Equal(t, before["captured_at"], created["captured_at"])
				assert.Equal(t, before["deadline_seconds"], created["deadline_seconds"])
			}
			assert.Equal(t, float64(1), created["capture_source_revision"])
			assert.NotContains(t, replayProgress(t, f, "checkpoint")["acme"], "replay_source_revision")
			f.definition["description"] = "after replay"
			f.save(t, 2)
			f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e1", "success"), fixtureEvent("e1", "success")})
			f.sync(t)
			assert.Empty(t, replaySeriesIDs(t, f, 3, "", ""))
			assert.Len(t, replayCaptureIDs(t, f), 1)
		})
	}
}

func TestObserveInterruptedReplayRetainsEpochAfterDescription(t *testing.T) {
	t.Parallel()
	f, block := newReplayFixture(t, 1, []string{"a"})
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e2", "success")})
	f.sync(t)
	f.sync(t)
	replayRecovery(t, f, 1, "backfill")
	replayInterrupt(t, f, block)
	cp := replayProgress(t, f, "checkpoint")["acme"]
	assert.Equal(t, "1", cp["cursor"])
	assert.Equal(t, float64(2), cp["replay_source_revision"])
	f.definition["description"] = "replay continues"
	f.save(t, 2)
	cp = replayProgress(t, f, "checkpoint")["acme"]
	assert.Equal(t, "1", cp["cursor"])
	assert.Equal(t, float64(2), cp["replay_source_revision"])
	require.NoError(t, os.Remove(block))
	f.sync(t)
	f.sync(t)
	assert.Len(t, replaySeriesIDs(t, f, 2, "a", ""), 2)
	assert.Empty(t, replaySeriesIDs(t, f, 3, "", ""))
	assert.Len(t, replayCaptureIDs(t, f), 2)
	assert.NotContains(t, replayProgress(t, f, "checkpoint")["acme"], "replay_source_revision")
}

func TestObserveReplaySkipsChangedObserver(t *testing.T) {
	t.Parallel()
	f, block := newReplayFixture(t, 1, []string{"a", "b"})
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e2", "success")})
	f.sync(t)
	f.sync(t)
	replayRecovery(t, f, 1, "backfill")
	replayInterrupt(t, f, block)
	f.definition["observers"].([]any)[0].(map[string]any)["criterion_revision"] = "v2"
	f.save(t, 2)
	require.NoError(t, os.Remove(block))
	f.sync(t)
	f.sync(t)
	captures := replayCaptureIDs(t, f)
	assert.Empty(t, replaySeriesIDs(t, f, 2, "a", captures["e2"]))
	assert.Len(t, replaySeriesIDs(t, f, 2, "b", captures["e2"]), 1)
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e2", "success"), fixtureEvent("e3", "success")})
	assert.Equal(t, 1, f.sync(t).Captured)
	f.sync(t)
	assert.Len(t, replaySeriesIDs(t, f, 3, "a", replayCaptureIDs(t, f)["e3"]), 1)
}

func TestObserveDisableCancelsReplayEpoch(t *testing.T) {
	t.Parallel()
	f, block := newReplayFixture(t, 1, []string{"a"})
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e2", "success")})
	f.sync(t)
	f.sync(t)
	replayRecovery(t, f, 1, "backfill")
	replayInterrupt(t, f, block)
	f.definition["enabled"] = false
	f.save(t, 2)
	assert.NotContains(t, replayProgress(t, f, "checkpoint")["acme"], "replay_source_revision")
	f.definition["enabled"] = true
	f.save(t, 3)
	require.NoError(t, os.Remove(block))
	cycle := f.sync(t)
	assert.Zero(t, cycle.Captured)
	assert.Zero(t, cycle.Observations)
	assert.Empty(t, replaySeriesIDs(t, f, 4, "", ""))
	replayRecovery(t, f, 4, "backfill")
	f.sync(t)
	f.sync(t)
	assert.Len(t, replaySeriesIDs(t, f, 5, "a", ""), 2)
	assert.Len(t, replayCaptureIDs(t, f), 2)
}

func TestObserveNewCaptureDuringReplayHasOneBinding(t *testing.T) {
	t.Parallel()
	f, block := newReplayFixture(t, 1, []string{"a"})
	f.sync(t)
	f.sync(t)
	replayRecovery(t, f, 1, "backfill")
	f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e2", "success"), fixtureEvent("e2", "success")})
	replayInterrupt(t, f, block)
	f.definition["description"] = "new capture after preview"
	f.save(t, 2)
	require.NoError(t, os.Remove(block))
	f.sync(t)
	f.sync(t)
	fresh := replayCaptureIDs(t, f)["e2"]
	assert.Len(t, replaySeriesIDs(t, f, 3, "a", fresh), 1)
	assert.Empty(t, replaySeriesIDs(t, f, 2, "a", fresh))
	assert.Equal(t, "3", replayProgress(t, f, "checkpoint")["acme"]["cursor"])
	assert.Len(t, replayCaptureIDs(t, f), 2)
}

func TestObserveRejectedAppendDoesNotCountCapture(t *testing.T) {
	t.Parallel()
	f, _ := newReplayFixture(t, 100, []string{"a"})
	keys := []string{}
	attributes := map[string]any{}
	for i := range 32 {
		key := "k" + strconv.Itoa(i)
		keys = append(keys, key)
		attributes[key] = strings.Repeat("x", 2048)
	}
	f.definition["reader"].(map[string]any)["allowed_attributes"] = keys
	event := fixtureEvent("e1", "success")
	event["attributes"] = attributes
	f.writeEvents(t, []map[string]any{event})
	replayRecovery(t, f, 1, "reset")
	cycle, err := f.app.ObserveSync(t.Context())
	require.Error(t, err)
	assert.Equal(t, "record_too_large", err.Error())
	assert.Zero(t, cycle.Captured)
	assert.Zero(t, cycle.Observations)
	assert.Empty(t, replayCaptureIDs(t, f))
	assert.Nil(t, replayProgress(t, f, "checkpoint")["acme"]["cursor"])
}

func TestObserveFailedCommitDoesNotCountCapture(t *testing.T) {
	t.Parallel()
	f, block := newReplayFixture(t, 100, []string{"a"})
	require.NoError(t, os.WriteFile(block, []byte("readonly_ledger"), 0600))
	cycle, err := f.app.ObserveSync(t.Context())
	require.Error(t, err)
	assert.ErrorContains(t, err, "store_corrupt")
	assert.Zero(t, cycle.Captured)
	assert.Zero(t, cycle.Observations)
	require.NoError(t, os.Chmod(filepath.Join(f.app.Home, "observe", "ledger.jsonl"), 0600))
	assert.Empty(t, replayCaptureIDs(t, f))
	assert.Nil(t, replayProgress(t, f, "checkpoint")["acme"]["cursor"])
}

func TestObserveBoundaryChangeClearsReplayEpoch(t *testing.T) {
	tcs := []struct {
		name    string
		removed bool
	}{{name: "collection_changed"}, {name: "removed", removed: true}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, block := newReplayFixture(t, 1, []string{"a"})
			f.writeEvents(t, []map[string]any{fixtureEvent("e1", "success"), fixtureEvent("e2", "success")})
			f.sync(t)
			f.sync(t)
			replayRecovery(t, f, 1, "backfill")
			replayInterrupt(t, f, block)
			if tc.removed {
				_, err := f.app.SourceRemove(t.Context(), app.SourceRemoveRequest{ID: "acme", ExpectedRevision: 2, Confirmed: true})
				require.NoError(t, err)
			} else {
				f.definition["reader"].(map[string]any)["timeout_seconds"] = 31
				f.save(t, 2)
			}
			assert.NotContains(t, replayProgress(t, f, "checkpoint")["acme"], "replay_source_revision")
			require.NoError(t, os.Remove(block))
			cycle := f.sync(t)
			assert.Zero(t, cycle.Attempts)
			assert.Zero(t, cycle.Captured)
			assert.Zero(t, cycle.Observations)
			assert.Empty(t, replaySeriesIDs(t, f, 3, "", ""))
		})
	}
}
