package export_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/export"
)

func TestSettingsFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "exporters.json")
	s := export.Settings{
		OTLP:     &export.OTLPSettings{Enabled: true, Endpoint: "https://x", Headers: map[string]string{"Authorization": "Basic cGstbGYtMTIzOnNrLWxmLTQ1Ng=="}},
		Langfuse: &export.LangfuseSettings{Enabled: true, PublicKey: "pk-lf-123456", SecretKey: "sk-lf-abcdef"},
	}
	require.NoError(t, export.SaveSettings(path, s))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	got, err := export.LoadSettings(path)
	require.NoError(t, err)
	assert.Equal(t, s, got)

	masked, err := json.Marshal(got.Masked())
	require.NoError(t, err)
	assert.NotContains(t, string(masked), "abcdef")
	assert.NotContains(t, string(masked), "cGst")
	assert.Equal(t, "sk-lf-abcdef", got.Langfuse.SecretKey, "masking returns a copy")

	empty, err := export.LoadSettings(filepath.Join(t.TempDir(), "none.json"))
	require.NoError(t, err)
	assert.Equal(t, export.Settings{}, empty)
}

func TestSaveSettingsFailure(t *testing.T) {
	t.Parallel()
	tcs := []struct{ name string }{{name: "parent"}, {name: "temporary file"}, {name: "rename"}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "settings.json")
			switch tc.name {
			case "parent":
				path = filepath.Join(dir, "file", "settings.json")
				require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), nil, 0o600))
			case "temporary file":
				require.NoError(t, os.Mkdir(path+".tmp", 0o700))
			case "rename":
				require.NoError(t, os.Mkdir(path, 0o700))
			}
			assert.Error(t, export.SaveSettings(path, export.Settings{Nodloop: &export.NodloopSettings{Enabled: true}}))
		})
	}
}

func TestReplace(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name, exporter, raw string
		bad                 string
		want                export.Settings
	}{
		{name: "replaces headers as a whole", exporter: export.NameOTLP, raw: `{"enabled":true,"endpoint":"https://y"}`,
			want: export.Settings{OTLP: &export.OTLPSettings{Enabled: true, Endpoint: "https://y"}}},
		{name: "turns nodloop off", exporter: export.NameNodloop, raw: `{"enabled":false}`,
			want: export.Settings{OTLP: &export.OTLPSettings{Enabled: true, Endpoint: "https://x", Headers: map[string]string{"A": "b"}}, Nodloop: &export.NodloopSettings{}}},
		{name: "unknown exporter", exporter: "slack", raw: `{}`, bad: "unknown exporter"},
		{name: "broken json", exporter: export.NameLangfuse, raw: `{`, bad: "langfuse-score settings"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := export.Settings{OTLP: &export.OTLPSettings{Enabled: true, Endpoint: "https://x", Headers: map[string]string{"A": "b"}}}
			err := s.Replace(tc.exporter, json.RawMessage(tc.raw))
			if tc.bad != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.bad)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, s)
		})
	}
}

func TestEnabled(t *testing.T) {
	t.Parallel()
	withNodloop := t.TempDir()
	bin := filepath.Join(withNodloop, ".nodloop", "bin", "nodloop")
	require.NoError(t, os.MkdirAll(filepath.Dir(bin), 0o700))
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700))
	on := &export.OTLPSettings{Enabled: true}
	tcs := []struct {
		name     string
		s        export.Settings
		userHome string
		want     []string
	}{
		{name: "nothing set", s: export.Settings{}, userHome: t.TempDir(), want: nil},
		{name: "otlp and auto nodloop", s: export.Settings{OTLP: on}, userHome: withNodloop, want: []string{export.NameOTLP, export.NameNodloop}},
		{name: "nodloop turned off", s: export.Settings{OTLP: on, Nodloop: &export.NodloopSettings{}}, userHome: withNodloop, want: []string{export.NameOTLP}},
		{name: "disabled langfuse", s: export.Settings{Langfuse: &export.LangfuseSettings{}}, userHome: t.TempDir(), want: nil},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.s.Enabled(tc.userHome, "")
			if tc.want == nil {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
