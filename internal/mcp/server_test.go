package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/app"
	"github.com/jeon-jihyeon/rindcut/internal/home"
	"github.com/jeon-jihyeon/rindcut/internal/judge"
	"github.com/jeon-jihyeon/rindcut/internal/mcp"
)

type harness struct {
	sess   *sdk.ClientSession
	folder string
}

func start(t *testing.T) *harness {
	t.Helper()
	h := &harness{folder: t.TempDir()}
	now := time.Date(2026, 10, 8, 9, 10, 0, 0, time.FixedZone("KST", 9*3600))
	userHome := t.TempDir()
	a := app.App{
		Home: home.Dir(t.TempDir()), UserHome: userHome, Now: func() time.Time { return now }, Version: "test", Warn: os.Stderr,
		Installer: judge.Installer{Binary: "/bin/rindcut", Home: "/h", UserHome: userHome, GOOS: "darwin", UID: 501,
			Exec: func(string, ...string) error { return nil }},
	}
	require.NoError(t, a.Ready())
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	_, err := mcp.New(a).Connect(ctx, st, nil)
	require.NoError(t, err)
	sess, err := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })
	h.sess = sess
	return h
}

// call returns the tool's JSON answer, or the error text when the tool refused
func (h *harness) call(t *testing.T, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := h.sess.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	text := res.Content[0].(*sdk.TextContent).Text
	if res.IsError {
		return nil, text
	}
	var out any
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	if m, ok := out.(map[string]any); ok {
		return m, ""
	}
	return map[string]any{"list": out}, ""
}

func (h *harness) jobArgs(extra map[string]any) map[string]any {
	args := map[string]any{
		"name": "가격 수집", "schedule": "0 9 * * *", "timezone": "Asia/Seoul", "grace": "30m",
		"evidence": "session", "folder": h.folder, "marker": "오늘 가격 수집해줘",
		"checks": []any{map[string]any{"kind": "file", "path": "out/{{date}}.csv"}},
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

func TestTools(t *testing.T) {
	t.Parallel()
	h := start(t)
	res, err := h.sess.ListTools(context.Background(), nil)
	require.NoError(t, err)
	names := []string{}
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	assert.ElementsMatch(t, []string{"job_preview", "job_save", "job_list", "job_remove", "verdicts", "correct", "report",
		"exporter_set", "exporter_list", "status"}, names)
}

func TestRegisterFlow(t *testing.T) {
	t.Parallel()
	h := start(t)
	type step struct {
		tool    string
		args    map[string]any
		refused string
	}
	steps := []step{
		{tool: "job_save", args: h.jobArgs(map[string]any{"confirmed": true}), refused: "preview the job first"},
		{tool: "job_preview", args: h.jobArgs(nil)},
		{tool: "job_save", args: h.jobArgs(nil), refused: "confirmed"},
		{tool: "job_save", args: h.jobArgs(map[string]any{"confirmed": true})},
		{tool: "job_save", args: h.jobArgs(map[string]any{"grace": "45m", "confirmed": true}), refused: "preview"},
		{tool: "job_preview", args: h.jobArgs(map[string]any{"grace": "nope"}), refused: "grace"},
		{tool: "job_preview", args: h.jobArgs(map[string]any{"checks": []any{map[string]any{"kind": "command", "run": "true", "timeout": "x"}}}), refused: "timeout"},
	}
	for _, s := range steps {
		_, msg := h.call(t, s.tool, s.args)
		if s.refused == "" {
			assert.Empty(t, msg, s.tool)
			continue
		}
		assert.Contains(t, msg, s.refused, s.tool)
	}
	list, _ := h.call(t, "job_list", map[string]any{})
	assert.Len(t, list["list"], 1)
	st, _ := h.call(t, "status", map[string]any{})
	assert.Equal(t, map[string]any{"version": "test", "tick_installed": true, "jobs": float64(1)},
		map[string]any{"version": st["version"], "tick_installed": st["tick_installed"], "jobs": st["jobs"]})
	rep, msg := h.call(t, "report", map[string]any{"since": "48h"})
	assert.Empty(t, msg)
	assert.Contains(t, rep, "silent")
}

func TestPreviewProbe(t *testing.T) {
	t.Parallel()
	h := start(t)
	out := filepath.Join(h.folder, "out")
	require.NoError(t, os.MkdirAll(out, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(out, "2026-10-08.csv"), []byte("row"), 0o600))
	tcs := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "no output yet for a session job", args: map[string]any{"checks": []any{map[string]any{"kind": "file", "path": "out/none.csv"}}}, want: "missed"},
		{name: "cloud job with its output", args: map[string]any{"evidence": "none", "folder": "", "marker": "",
			"checks": []any{map[string]any{"kind": "file", "path": out + "/{{date}}.csv"}}}, want: "pass"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, msg := h.call(t, "job_preview", h.jobArgs(tc.args))
			require.Empty(t, msg)
			assert.Equal(t, tc.want, p["probe"].(map[string]any)["verdict"])
			assert.Len(t, p["next_slots"], 3)
		})
	}
}

func TestCorrectAndExporters(t *testing.T) {
	t.Parallel()
	h := start(t)
	tcs := []struct {
		name, tool string
		args       map[string]any
		refused    string
	}{
		{name: "unknown verdict id", tool: "correct", args: map[string]any{"verdict_id": "nope", "verdict": "pass", "reason": "x", "by": "owner"}, refused: "no verdict"},
		{name: "bad verdict", tool: "correct", args: map[string]any{"verdict_id": "nope", "verdict": "great", "reason": "x", "by": "owner"}, refused: "must be pass"},
		{name: "unknown exporter", tool: "exporter_set", args: map[string]any{"name": "slack", "settings": map[string]any{}}, refused: "unknown exporter"},
		{name: "bad since", tool: "verdicts", args: map[string]any{"since": "yesterday"}, refused: "duration"},
	}
	for _, tc := range tcs {
		_, msg := h.call(t, tc.tool, tc.args)
		assert.Contains(t, msg, tc.refused, tc.name)
	}
	set, msg := h.call(t, "exporter_set", map[string]any{"name": "langfuse-score",
		"settings": map[string]any{"enabled": true, "host": "http://127.0.0.1:1", "public_key": "pk-lf-1234567890", "secret_key": "sk-lf-abcdefghij"}})
	require.Empty(t, msg)
	b, err := json.Marshal(set)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "abcdefghij")
	assert.NotEqual(t, "ok", set["test"], "an unreachable host fails its test")
	listed, _ := h.call(t, "exporter_list", map[string]any{})
	b, err = json.Marshal(listed)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "abcdefghij")
	assert.True(t, strings.Contains(string(b), "langfuse-score"))
}
