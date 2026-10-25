package mcp_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCriticRegistration(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name    string
		timeout string
	}{
		{name: "default timeout"},
		{name: "explicit timeout", timeout: "2s"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := start(t)
			response := `{"protocol":"rindcut.critic/v1","job_id":"report","job_revision":1,` +
				`"slot":"2026-10-08T09:00:00Z",` +
				`"criteria_revision":"C1","complete":true,"criteria":[{"id":"accuracy","verdict":"rejected"}]}`
			require.NoError(t, os.WriteFile(filepath.Join(h.folder, "response.json"), []byte(response), 0o600))
			args := h.jobArgs(map[string]any{
				"name":   "Report",
				"checks": []any{map[string]any{"kind": "command", "run": "true"}},
				"critic": map[string]any{"source": "acme", "source_revision": "adapter-v1", "criteria_revision": "C1",
					"run": "cat response.json", "timeout": tc.timeout,
					"criteria": []any{map[string]any{"id": "accuracy", "text": "Numbers have sources"}}},
			})
			out, msg := h.call(t, "job_preview", args)
			require.Empty(t, msg)
			probe, ok := out["probe"].(map[string]any)
			require.True(t, ok)
			critic, ok := probe["critic"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, "pass", probe["verdict"])
			assert.Equal(t, "rejected", critic["verdict"])
			assert.Equal(t, "adapter-v1", critic["source_revision"])
			args["confirmed"] = true
			_, msg = h.call(t, "job_save", args)
			assert.Empty(t, msg)
		})
	}
}

func TestCriticRegistrationErrors(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name    string
		timeout string
		bad     string
	}{
		{name: "invalid timeout", timeout: "later", bad: "critic timeout"},
		{name: "excessive timeout", timeout: "11m", bad: "timeout must"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := start(t)
			response := `{"protocol":"rindcut.critic/v1","job_id":"report","job_revision":1,` +
				`"slot":"2026-10-08T09:00:00Z",` +
				`"criteria_revision":"C1","complete":true,"criteria":[{"id":"accuracy","verdict":"rejected"}]}`
			require.NoError(t, os.WriteFile(filepath.Join(h.folder, "response.json"), []byte(response), 0o600))
			args := h.jobArgs(map[string]any{
				"name":   "Report",
				"checks": []any{map[string]any{"kind": "command", "run": "true"}},
				"critic": map[string]any{"source": "acme", "source_revision": "adapter-v1", "criteria_revision": "C1",
					"run": "cat response.json", "timeout": tc.timeout,
					"criteria": []any{map[string]any{"id": "accuracy", "text": "Numbers have sources"}}},
			})
			_, msg := h.call(t, "job_preview", args)
			assert.Contains(t, msg, tc.bad)
		})
	}
}
